# Observability

manent exposes Prometheus metrics, OpenTelemetry traces, the log and timeline of every deploy and DORA metrics. Application logs still go to the [SIEM](log-siem.md) as well.

```yaml title="manent.yml"
observability:
  listen: ":9090"     # /metrics, /healthz, /readyz on a dedicated port
  metrics: true       # default
  tracing:
    enabled: true
    endpoint: otel-collector:4318
    insecure: true
    sample_ratio: 1
```

## Probes and metrics

| Path | Where | Description |
|---|---|---|
| `/healthz` | API and `observability.listen` | The process is alive (liveness). |
| `/readyz` | API and `observability.listen` | The process can reach the database and is not shutting down (readiness). During [drain](affidabilita.md#arresto-ordinato-drain) it responds `503`. |
| `/metrics` | API and `observability.listen` | Metrics in Prometheus format. |

`worker` has no API: probes and metrics need `observability.listen`. With `serve` the same routes are also on the API port. They require no authentication, so the port must be exposed only to the monitoring network (see [Security](sicurezza.md)).

### Metrics

| Metric | Type | Labels | Description |
|---|---|---|---|
| `manent_deployments_total` | counter | `application`, `environment`, `kind`, `outcome`, `dry_run` | Deploys completed by the process. |
| `manent_deployment_duration_seconds` | histogram | `application`, `environment`, `outcome` | Duration from start on the worker to completion, excluding queue wait and approvals. |
| `manent_step_duration_seconds` | histogram | `step`, `outcome` | Duration of each attempt of a step: retries included. |
| `manent_rollout_passes_total` | counter | `application`, `environment`, `region`, `az`, `outcome` | [Wave rollout](../guida/ondate.md) passes completed by the process. |
| `manent_http_request_duration_seconds` | histogram | `method`, `route`, `code` | API requests. |
| `manent_workflows` | gauge | `workflow`, `status`, `queue` | Workflows per state: deploys queued, running, awaiting approval and finished. |
| `manent_executor_up` | gauge | `executor`, `version` | 1 if the worker sends heartbeats, 0 if it is lost or stopped. |
| `manent_targets` | gauge | `application`, `environment`, `state` | Targets per state read by the [reconciliation](../guida/governance.md#stato-reale): `in-sync`, `drifted`, `missing`, `standby`, `unknown`. |
| `manent_executor_heartbeat_timestamp_seconds` | gauge | `executor`, `version` | Last heartbeat. |
| `manent_build_info` | gauge | `version`, `commit`, `workflow_version` | Binary and workflow version. |
| `manent_policy_revision_info` | gauge | `engine`, `source`, `revision` | [Governance policy](../guida/policy.md) in force in the process. |
| `manent_policy_age_seconds` | gauge | | Seconds since the last successful read of policy and runtime freezes. |
| `manent_policy_evaluations_total` | counter | `phase`, `outcome` | Policy evaluations (`allowed`, `denied`, `frozen`, `error`, `unavailable`). |
| `manent_policy_eval_duration_seconds` | histogram | `phase` | Duration of the evaluations. |
| `manent_policy_load_errors_total` | counter | | Failed reads: revision refused or source unreachable. |
| `manent_authz_grants_evaluations_total` | counter | `outcome` | Grants of an identity computed by the [permission policy](../guida/permessi.md) (`ok`, `cached`, `error`). |
| `manent_authz_deny_evaluations_total` | counter | `action`, `outcome` | Denials evaluated (`allowed`, `denied`, `error`). |
| `manent_authz_eval_duration_seconds` | histogram | `part` | Duration of the permission evaluations (`grants`, `deny`). |
| `manent_authz_invalid_grants_total` | counter | | Grants discarded as invalid or with `policy.*` actions. |
| `manent_authz_cache_entries` | gauge | | Identities in the grants cache. |

Plus the standard Go runtime (`go_*`) and process (`process_*`) metrics.

!!! note "Global and per-process metrics"
    `manent_deployments_total` and the histograms belong to the single process and must be summed across instances (`sum`). `manent_workflows`, `manent_executor_*` and `manent_targets` are read from the database, so every instance reports the same values: aggregate them with `max`. The database is read at most every 15 seconds for unfinished workflows (queued, running, scheduled) and at most every 5 minutes for finished ones, whose count scans the whole history.

Example queries and alerts:

```promql
# Failed deploys in the last hour per application and environment
sum by (application, environment) (increase(manent_deployments_total{outcome="failed",dry_run="false"}[1h]))

# Deploys queued for too long: stuck queue or not enough workers
max(manent_workflows{workflow="manent.deploy",status="ENQUEUED"}) > 20

# Lost worker
max by (executor) (manent_executor_up) == 0

# Targets changed outside manent
max by (application, environment) (manent_targets{state=~"drifted|missing"}) > 0

# p95 duration of production deploys
histogram_quantile(0.95, sum by (le) (rate(manent_deployment_duration_seconds_bucket{environment="prod"}[1d])))
```

## Tracing

With `tracing.enabled` manent exports traces over OTLP/HTTP (`endpoint`, or `OTEL_EXPORTER_OTLP_ENDPOINT`). A trace follows the deploy from the API request to the steps run by the worker:

```text
POST /api/v1/deployments          (API, child of the client's traceparent, e.g. CI)
└── deployment                    (worker: manent.deployment.id, application, environment, outcome)
    ├── step config
    ├── step secrets
    ├── step artifacts
    ├── step deploy
    └── step activation
```

The request's trace is saved in the deploy (`trace_parent`) and passed to the worker, even if the deploy starts after an approval or at the `schedule.at` time. Runs of recurring schedules start new traces. Each attempt of a step is a span: retries are visible. After a recovery the deploy produces a new `deployment` span in the same trace.

## Deploy log and timeline { #log-e-timeline-di-un-deploy }

Logs emitted during the steps (by manent and by steps that use the logger with the step's context) are saved with the deploy and can be read from any instance, even after the worker that ran it has stopped:

```sh
manent logs 7c0e…            # the whole log
manent logs -f 7c0e…         # follows the deploy until completion
manent logs --json 7c0e…     # one JSON line per entry
```

```text
2026-10-10 14:03:12 INFO  [artifacts] artefatto recuperato path=work/7c0e…/app.tar.gz sha256=2d27…
2026-10-10 14:03:15 INFO  [deploy] deploy completato su target host=10.0.0.5
```

The timeline reports every recorded step with times, duration, output and error:

```sh
manent steps 7c0e…
```

Through the API: `GET /api/v1/deployments/{id}/logs?offset=N` (with `next` and `done` to continue) and `GET /api/v1/deployments/{id}/steps`. They require `deployment.read`.

!!! note
    - At most 500 lines per step are saved, at `INFO` level or above, even if `logging.level` is higher. The application log and the SIEM remain complete.
    - Like outputs, logs must not contain secrets.
    - Logs of [external plugins](../plugins/esterni.md) reach the application log and the SIEM, but not the deploy log.
    - Logs and timeline are deleted together with the deploy by [retention](affidabilita.md#retention-dello-storico).

## DORA metrics { #metriche-dora }

```sh
manent dora --since 30d [-a myapp] [-e prod]
```

```json
{
  "since": "2026-09-10T00:00:00Z",
  "until": "2026-10-10T00:00:00Z",
  "total": {"deployments": 42, "failures": 3, "deployments_per_day": 1.3, "change_failure_rate": 0.07,
            "mttr_seconds": 2520, "lead_time_seconds": 172800},
  "environments": [{"application": "myapp", "environment": "prod", "deployments": 12, "failures": 1, "…": "…"}]
}
```

| Metric | Computation |
|---|---|
| `deployments_per_day` | Successful deploys in the period divided by the days. |
| `change_failure_rate` | Deploys that caused a failure divided by completed deploys. A failure is a failed deploy or a [manual rollback](../guida/governance.md#rollback-manuale), which signals a faulty release. |
| `mttr_seconds` | Mean time from a failure to the next successful deploy in the same environment. |
| `lead_time_seconds` | Median time between the first successful release of a version, in any environment, and its release in the environment. It measures promotion speed: manent does not know the commit time. |

Dry-runs and cancelled deploys are excluded. The computation examines at most the last 10,000 deploys and only sees the history kept by retention. Through the API: `GET /api/v1/metrics/dora?application=&environment=&since=`, with only the environments the identity can read.
