# manent-server

`manent-server` contains the API, the worker and the platform operations on the system database. Users and CI use the [`manent` client](../guida/cli.md) instead, which talks only to the API.

```sh
manent-server [-c <manent.yml>] <command> [options]
```

| Global option | Description |
|---|---|
| `-c`, `--config` | [manent configuration](configurazione.md). It can also be given with `MANENT_CONFIG`. Without a file the defaults apply. |

## Processes

### `serve`

Starts the HTTP API and, in the same process, a worker. It is the simplest way for development and for single-node installations.

```sh
manent-server serve [-l :8080] [--no-worker]
```

| Option | Description |
|---|---|
| `-l`, `--listen` | Listen address (overrides `http.listen`). |
| `--no-worker` | API only, like `api`. |

### `api`

Starts only the API: it enqueues and queries deploys, but does not run them. With several nodes the API instances sit behind a load balancer and execution belongs to the workers (see [Scalability](scalabilita.md)).

```sh
manent-server api [-l :8080]
```

### `worker`

Starts only the worker, without the API. On `SIGINT` or `SIGTERM` it stops picking up deploys and waits for those in progress up to `dbos.drain_timeout` ([drain](affidabilita.md#arresto-ordinato-drain)). Interrupted deploys are resumed when a worker with the same `executor_id` restarts or, with recovery, by another worker. Probes and metrics are on `observability.listen`.

```sh
manent-server worker
```

## Platform operations

They work directly on the system database, so they need its credentials (`dbos.database_url` or `DBOS_SYSTEM_DATABASE_URL`). They are reserved to platform operators: whoever has access to the database can act on DBOS without going through manent's permissions. The identity recorded in the audit log is the operating system user (`cli:<user>`).

### `admin`

```sh
manent-server admin executors                                        # registered workers and pending workflows
manent-server admin recover --executor <id> [--dry-run] [--force]   # resumes the workflows of a lost worker
manent-server admin recover --from-version <v> [--to-version <v>] [--from-start] [--dry-run]
manent-server admin prune [--max-age 2160h] [--keep-releases 10] [--dry-run]
```

`admin executors` reads the database; to see workers from a workstation, through the API and with manent's permissions, use [`manent workers`](../guida/cli.md#attivita-e-worker) (`platform.read`). Recovery and retention are recorded in the audit log (`admin.recover`, `admin.prune`). See [Recovery, shutdown and retention](affidabilita.md) and [Versions and upgrades](aggiornamenti.md).

In Kubernetes they run in a server pod, which already has configuration and credentials:

```sh
kubectl exec manent-worker-0 -- manent-server admin executors
```

### `audit verify`

```sh
manent-server audit verify [<file>...]
```

Verifies the [hash chain](accesso.md#integrita-del-file) of the audit file. Without arguments it uses `audit.file` from the configuration; with a single file it includes its rotated files. The HMAC key is read from `audit.hmac_key_ref`. It exits with an error at the first modified, inserted or missing entry.

### `validate`

Checks a manent configuration and a descriptor locally, without a database and without a running server: useful before installing a new configuration or in a pipeline that versions it.

```sh
manent-server -c manent.yml validate -f examples/descriptor.yml [--offline]
```

It shows the plan, the rollback, the governance rule and, unless `--offline`, the effective deploy read from the environment configuration (for example from Consul) with the stages and passes of any [wave rollout](../guida/ondate.md). To check a descriptor against the production server use [`manent validate`](../guida/cli.md#validate).

### `version`

```sh
manent-server version
```

## Exit codes

The command exits with code `0` if the operation succeeds and with a non-zero code on error.
