# Recovery, shutdown and retention

This page explains how manent behaves when a worker stops, how to shut down a worker without interrupting deploys and how to contain the growth of the database.

## Worker registry

Every worker registers in the system database (tables `manent.executors` and `manent.leases` on Postgres, `manent_executors` and `manent_leases` on SQLite) and updates its **heartbeat** every `recovery.heartbeat_interval`. A worker that has sent no heartbeat for `recovery.stale_after`, or that has stopped, is considered **lost**.

```sh
manent-server admin executors
```

```json
{
  "version": "0.1.0",
  "executors": [
    {"id": "manent-worker-0", "version": "0.1.0", "build": "0.3.0", "hostname": "manent-worker-0",
     "started_at": "…", "heartbeat_at": "…", "registered": true, "alive": true},
    {"id": "manent-worker-1", "version": "0.1.0", "heartbeat_at": "…", "registered": true, "alive": false,
     "pending": {"0.1.0": 2}}
  ]
}
```

| Field | Meaning |
|---|---|
| `alive` | The worker is sending heartbeats. |
| `pending` | Pending workflows (`PENDING`) assigned to the worker, per workflow version. Only the worker itself, on restart, or a recovery resume them. |
| `recovered_by` | Who has already resumed the lost worker's workflows. |
| `registered: false` | Worker with pending workflows but missing from the registry, for example lost before the registry was introduced. |

The same information is in the `manent_executor_up` and `manent_executor_heartbeat_timestamp_seconds` metrics (see [Observability](osservabilita.md)).

## Lost worker { #worker-perso }

At startup a worker resumes only the deploys left pending with its own `executor_id`. If a node does not come back, for example because the machine was decommissioned or a StatefulSet pod was removed by a scale down, its workflows stay `PENDING` and, with `environment_concurrency: 1`, also block the following deploys of the same environment.

There are two ways to have another worker resume them.

### Automatic recovery

```yaml title="manent.yml"
recovery:
  heartbeat_interval: 15s
  stale_after: 1m
  auto_recover_after: 10m   # 0s = disabled (default)
```

One worker at a time (`recovery` lease) checks the registry. The pending workflows of a worker **of the same version** that has stopped, or has sent no heartbeat for `auto_recover_after`, are requeued and resumed by any worker, from the last completed step.

### Manual recovery

```sh
manent-server admin recover --executor manent-worker-1 --dry-run   # what would be resumed
manent-server admin recover --executor manent-worker-1
```

The command refuses a worker that still appears alive, one unknown to the registry or one already recovered. `--force` skips these checks: use it only after making sure the node is really down.

### Guarantees and limits

- **One recovery per worker.** The lost worker is *claimed* in the registry with an atomic operation: two workers, or a worker and an operator, cannot resume the same workflows.
- **No overlap with restart.** A restarting worker waits for any in-progress recovery of its workflows to complete, at most 2 minutes, before starting its own DBOS recovery. A recovery can only claim a worker that sends no heartbeat. This way the same workflow is never resumed twice.
- **The interrupted step is rerun.** Completed steps are not repeated, while the one running at the time of the loss restarts. This is why the plugins' `Deploy` and `Rollback` must be idempotent.
- **Network partition.** A node that is alive but cut off from the database for longer than `auto_recover_after` is considered lost. When it becomes reachable again, the step it was running may complete while the same deploy restarts elsewhere. The node notices at the first heartbeat and reports it with an error in the log: it must be restarted. This is why `auto_recover_after` must be much longer than the expected network outages; when in doubt, leave it disabled and use manual recovery.
- **Pending approvals.** The approval workflows of a lost worker are resumed too, without losing the decisions already recorded.

## Graceful shutdown (drain) { #arresto-ordinato-drain }

On `SIGTERM` or `SIGINT` the worker:

1. stops picking up deploys from the queue and `/readyz` responds `503`;
2. waits up to `dbos.drain_timeout` (default 2 minutes) for the running deploys to finish;
3. stops DBOS and the external plugins and records the shutdown.

Pending approvals are not waited for: they resume on restart or, with automatic recovery, on another worker. Deploys not finished within the timeout stay pending and are resumed the same way.

The platform's stop timeout must exceed `drain_timeout`: `terminationGracePeriodSeconds` in Kubernetes (the [chart](installazione.md#kubernetes-helm) sets it to 6 minutes), `TimeoutStopSec` with systemd.

## History retention { #retention-dello-storico }

Every deploy leaves the workflow, steps, events and log in the system database. Without cleanup the database grows indefinitely.

```yaml title="manent.yml"
retention:
  max_age: 2160h      # 90 days; 0s = no cleanup (default)
  keep_releases: 10   # successful deploys always kept per application and environment
  interval: 1h
```

One worker at a time (`retention` lease) deletes every `interval` the manent workflows (deploys, approvals, schedule runs) **finished** more than `max_age` ago, with their steps, events and logs. For each application and environment it always keeps the last `keep_releases` successful deploys and their approvals: the [inventory](../guida/governance.md#inventario), promotion and manual rollback keep working. Running, queued or scheduled deploys are never touched.

```sh
manent-server admin prune --dry-run                    # how many workflows would be deleted
manent-server admin prune --max-age 720h --keep-releases 5
```

!!! warning "Audit"
    Retention deletes the operational history, not the [audit](accesso.md#audit): the audit file and the SIEM remain the long-term record of who did what. `max_age` must be consistent with the organization's retention requirements.

[DORA metrics](osservabilita.md#metriche-dora) and the promotion of an old version only see the retained history.

## Backup and restore { #backup-e-ripristino }

manent's state is entirely in the system database: running and finished deploys, queues, schedules, approvals, deploy logs and the worker registry. Configuration, environment configuration (Consul) and secrets live outside and must be backed up separately. The workers' working directory contains only temporary artifacts.

- **Backup.** On Postgres a continuous backup with point-in-time recovery is recommended (WAL archiving, for example with pgBackRest or the cloud's managed service). A `pg_dump` of the database is fine for small volumes.
- **Restore.** A database restored to an earlier point contains the deploys that were running or queued at that time. On restart the workers would resume them, even if they had meanwhile completed on the targets. Before starting the workers on a restored database:
    1. list the pending and queued deploys (`manent list --state PENDING`, `--state ENQUEUED`, `--state DELAYED`);
    2. cancel with `manent cancel` those already run after the restore point, comparing them with the audit in the SIEM;
    3. check the schedules (`manent schedule list`) and pause those that must not run.
- **SQLite.** The database file (with the `-wal` and `-shm` files) must be copied with processes stopped, or with `sqlite3 manent.db ".backup copy.db"`.
