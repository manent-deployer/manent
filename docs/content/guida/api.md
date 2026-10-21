# HTTP API

The complete contract is the OpenAPI specification in [`api/openapi.yaml`](https://github.com/joeg-ita/manent/blob/main/api/openapi.yaml): a test checks that it matches the routes the server registers, and the [web interface](interfaccia-web.md) generates its types from it.

The API is served by `manent-server serve` (default `:8080`). It accepts and returns JSON. A descriptor body can be at most 1 MiB.

## Authentication

With `auth.mode: enabled` the `/api/v1` routes require a bearer token, OIDC or static:

```http
Authorization: Bearer <token>
```

| Outcome | Response |
|---|---|
| Missing or invalid token | `401 Unauthorized`, header `WWW-Authenticate: Bearer realm="manent"` |
| Action not allowed by permissions | `403 Forbidden`, with the reason in `error` |

`/healthz` does not require authentication. Lists of deploys, schedules and the inventory contain only what the identity can read. See [Authentication, permissions and audit](../operazioni/accesso.md).

## Summary

| Method | Path | Description |
|---|---|---|
| `POST` | `/api/v1/deployments` | Enqueues a deploy, schedules it or creates a schedule. |
| `GET` | `/api/v1/deployments` | Lists the most recent deploys. |
| `POST` | `/api/v1/deployments/plan` | Computes the plan without starting anything. |
| `POST` | `/api/v1/deployments/validate` | Checks a descriptor as at enqueue time and returns the effective deploy. |
| `POST` | `/api/v1/environments/validate` | Checks a candidate environment configuration (`application`, `environment`, `config`, optional `descriptor`): see [CI and infrastructure as code](ci-iac.md#env-validate). |
| `GET` | `/api/v1/deployments/{id}` | Status and result of a deploy. |
| `GET` | `/api/v1/deployments/{id}/steps` | Step timeline. |
| `GET` | `/api/v1/deployments/{id}/logs?offset=` | Deploy log. |
| `GET` | `/api/v1/deployments/{id}/events` | Status and log as Server-Sent Events. |
| `POST` | `/api/v1/deployments/{id}/cancel` | Cancels a deploy. |
| `POST` | `/api/v1/deployments/{id}/approve` | Approves a pending deploy. |
| `POST` | `/api/v1/deployments/{id}/reject` | Rejects a pending deploy. |
| `GET` | `/api/v1/approvals` | Deploys waiting for a decision that the caller can take. |
| `GET` | `/api/v1/me` | Identity of the caller and the roles that apply to it. |
| `GET` | `/api/v1/activity` | Deploys not yet finished: running, awaiting approval, queued, scheduled. |
| `POST` | `/api/v1/releases` | Submits a [composite release](catene.md#release-composte). |
| `GET` | `/api/v1/releases` | Lists the most recent releases. |
| `GET` | `/api/v1/releases/{id}` | State of a release and of its deploys. |
| `POST` | `/api/v1/releases/{id}/cancel` | Cancels the deploys not yet finished and the release. |
| `GET` | `/api/v1/workers` | Registered workers with state, heartbeat, resources, queues and running workflows (`platform.read`). |
| `GET` | `/api/v1/queues` | Queues with limits, waiting, running and scheduled workflows and listening workers (`platform.read`). |
| `POST` | `/api/v1/promotions` | Promotes an artifact from one environment to another. |
| `POST` | `/api/v1/rollbacks` | Goes back to a previous version. |
| `GET` | `/api/v1/inventory` | Live releases per application and environment. |
| `GET` | `/api/v1/freezes` | Active and upcoming freeze windows. |
| `GET` | `/api/v1/tenants?application=&environment=&selector=` | [Tenants](tenant.md) of the registry, filtered by permissions. |
| `GET` | `/api/v1/tenants/{name}` | A tenant (parameters and secret names). |
| `POST` | `/api/v1/freezes` | Creates a freeze window at runtime (`freeze.write`). |
| `DELETE` | `/api/v1/freezes/{name}` | Deletes a freeze window created at runtime. |
| `GET` | `/api/v1/policies` | Governance policy revisions and the one in force (`policy.read`). |
| `POST` | `/api/v1/policies` | Pushes a draft revision (`policy.write`). |
| `GET` | `/api/v1/policies/{revision}` | A revision with its files. |
| `POST` | `/api/v1/policies/{revision}/evaluate` | Decision of a revision on a descriptor (`policy.write`). |
| `POST` | `/api/v1/policies/{revision}/access` | Permissions of an identity given by hand, difference from the active revision and outcome of an action (`policy.write`, `rbac.engine: opa`). |
| `GET` | `/api/v1/policies/{revision}/impact` | Decisions of the real deploys recomputed with a revision (`policy.write`). |
| `POST` | `/api/v1/policies/{revision}/approve` | Approves and activates a draft (`policy.approve`). |
| `POST` | `/api/v1/policies/{revision}/activate` | Goes back to a previous revision (`policy.approve`). |
| `GET` | `/api/v1/metrics/dora` | DORA metrics. |
| `GET` | `/api/v1/schedules` | Lists recurring schedules. |
| `GET` | `/api/v1/schedules/{name}` | Details of a schedule. |
| `POST` | `/api/v1/schedules/{name}/pause` | Pauses a schedule. |
| `POST` | `/api/v1/schedules/{name}/resume` | Resumes a schedule. |
| `DELETE` | `/api/v1/schedules/{name}` | Deletes a schedule. |
| `GET` | `/ui/config.json` | Public configuration of the [web interface](interfaccia-web.md). No authentication. |
| `POST` | `/integrations/slack/interactions` | Approval buttons of Slack messages, authenticated by Slack's signature (see [Chat and change management](integrazioni.md#approvazioni-da-chat)). Registered only with `integrations.slack.signing_secret_ref`. |
| `GET` | `/healthz` | The process is alive. No authentication. |
| `GET` | `/readyz` | The process can reach the database and is not shutting down. No authentication. |
| `GET` | `/metrics` | Prometheus metrics (with `observability.metrics`). No authentication. |

## Deploys

### Enqueue a deploy

```http
POST /api/v1/deployments?dry_run=false
Content-Type: application/json

{ ...descriptor... }
```

| Outcome | Response |
|---|---|
| Deploy enqueued or scheduled with `schedule.at` | `202 Accepted`, header `Location: /api/v1/deployments/{id}` |
| Recurring schedule created or updated (`schedule.cron`) | `201 Created`, header `Location: /api/v1/schedules/{name}` |
| Invalid descriptor or non-boolean `dry_run` parameter | `400 Bad Request` |
| Unacceptable request (invalid plan, time in the past, callbacks, rollback or artifact source not allowed, descriptor not allowed by the `descriptor_policy` or incompatible with the environment configuration, active freeze, version not promoted) | `422 Unprocessable Entity` |
| Action not allowed (e.g. `deployment.submit` on the environment) | `403 Forbidden` |

```json title="202 Accepted"
{"id": "myapp-1.2.3-test", "state": "ENQUEUED"}
```

```json title="202 Accepted, scheduled deploy"
{"id": "4f1c…", "state": "DELAYED", "scheduled_at": "2026-10-10T02:00:00+02:00"}
```

```json title="202 Accepted, approval required"
{"id": "7c0e…", "state": "AWAITING_APPROVAL"}
```

Compatibility with the environment configuration and the `descriptor_policy` is already checked here. If the environment configuration cannot be reached (e.g. Consul unavailable) the request is accepted and the check is left to the worker.

With `?dry_run=true` (or `"dryRun": true` in the descriptor) a [dry-run](dry-run.md) is enqueued and the response contains `"dry_run": true`.

### Compute the plan

```http
POST /api/v1/deployments/plan
```

```json title="200 OK"
{"plan": [{"name": "secrets", "max_retries": 0}, {"name": "artifacts", "max_retries": 3}]}
```

Requires `deployment.read` on the descriptor's application and environment.

### Check a descriptor

`POST /api/v1/deployments/validate` runs the enqueue-time checks (descriptor policy, environment configuration, callbacks, artifact source, rollback) without enqueuing anything. It is what `manent validate` uses. Requires `deployment.read`.

```json
{
  "valid": true,
  "plan": [{"name": "secrets", "max_retries": 0}, {"name": "deploy", "max_retries": 2}],
  "deploy": {"plugin": "remote-copy", "targets": [{"host": "10.0.0.5", "user": "deploy"}], "params": {"dest_dir": "/opt/myapp"}},
  "secrets": ["deploy-key"],
  "config": {"source": "consul", "revision": "1842"},
  "governance": {"rule": "production", "requires": ["preprod"], "approvals": 1},
  "warnings": []
}
```

With a [wave rollout](ondate.md) the response also contains `rollout`, with the stages and passes that would run. An invalid descriptor produces `422` with the reason. If the environment configuration cannot be reached the response is still `200`, with the notice in `warnings`.

### Deploy status

```http
GET /api/v1/deployments/{id}
```

```json title="200 OK"
{
  "id": "myapp-1.2.3-test",
  "state": "SUCCESS",
  "application": "myapp",
  "environment": "test",
  "artifact": "myapp",
  "version": "1.2.3",
  "sha256": "2d27fbdf…",
  "kind": "deploy",
  "requested_by": "oidc:alice",
  "created_at": "2026-10-09T10:00:00Z",
  "updated_at": "2026-10-09T10:01:12Z",
  "result": {
    "deployment_id": "myapp-1.2.3-test",
    "steps": [
      {"name": "config", "output": {"source": "consul:manent/myapp/myapp/test", "revision": "42", "targets": "1"}},
      {"name": "artifacts", "output": {"local_path": "…", "sha256": "…", "size": "10240"}},
      {"name": "deploy", "output": {"plugin": "remote-copy", "result": "{…}"}}
    ]
  }
}
```

| State | Meaning |
|---|---|
| `ENQUEUED` | Queued, waiting for a worker. |
| `DELAYED` | Scheduled with `schedule.at`, waiting for the time. |
| `PENDING` | Running. |
| `SUCCESS` | Completed successfully: `result` contains the output of each step. |
| `ERROR` | Completed with an error: `error` contains the message. |
| `CANCELLED` | Cancelled. |
| `AWAITING_APPROVAL` | Waiting for [approvals](governance.md#approvazioni): `approval` reports the decisions. |
| `REJECTED`, `EXPIRED` | Rejected or not approved in time: the deploy was not run. |
| `WAITING_DEPENDENCY` | Waiting for the deploys in [`after`](catene.md) to succeed: `after` reports their state. |
| `DEPENDENCY_FAILED` | A dependency did not succeed or the wait expired: the deploy was not run. |

A non-existent ID returns `404`.

| Field | Meaning |
|---|---|
| `requested_by` | Identity that requested the deploy; for schedule deploys, whoever last created or updated it. |
| `kind`, `source` | `deploy`, `promotion` or `rollback`; for the last two, the deploy they derive from. |
| `sha256` | Hash of the released artifact, recorded at the end of the deploy. |
| `rollback` | Outcome of the [automatic rollback](rollback.md), if run. |
| `dry_run` | `true` for a dry-run. |

### List

```http
GET /api/v1/deployments?application=myapp&environment=prod&state=SUCCESS&limit=50
```

Returns `{"deployments": [...]}` from the most recent, without the step results (read them from the detail).

| Parameter | Description |
|---|---|
| `schedule` | Only the runs of a recurring schedule. |
| `application`, `environment` | Only deploys of the application or environment. |
| `state` | Only deploys in the given state (e.g. `AWAITING_APPROVAL`). |
| `limit` | Maximum number of results (default 50), positive integer. |
| `cursor` | `next_cursor` of the previous page: the list restarts after that deploy. |

Filters and permissions do not reduce the number of results: the list keeps going through older deploys up to `limit` (examining at most 10,000). When a page is full the response also has `next_cursor`, the ID of its last deploy.

### Cancel

```http
POST /api/v1/deployments/{id}/cancel
```

Responds `204 No Content`, or `404` if the deploy does not exist.

## Schedules

`GET /api/v1/schedules` returns `{"schedules": [...]}`:

```json
{
  "name": "myapp-nightly",
  "cron": "0 0 2 * * *",
  "timezone": "Europe/Rome",
  "status": "ACTIVE",
  "application": "myapp",
  "environment": "test",
  "requested_by": "oidc:bob",
  "last_fired_at": "2026-10-09T00:00:00Z"
}
```

`pause`, `resume` and `DELETE` respond `204 No Content` or `404`.

## Governance

See [Release governance](governance.md) for the meaning of each operation.

### Approve or reject

```http
POST /api/v1/deployments/{id}/approve
Content-Type: application/json

{"comment": "CHG-1234"}
```

The body is optional. Responds `204`; `403` without the `deployment.approve` permission or if the approver requested the deploy; `422` if the deploy is not awaiting approval or the identity has already decided. `/reject` has the same shape.

### Promote

```http
POST /api/v1/promotions

{"application": "myapp", "from": "preprod", "to": "prod", "version": "1.4.0",
 "deploy": {"targets": [{"host": "prod-1"}]}, "id": "myapp-1.4.0-prod", "dry_run": false, "freeze_override": ""}
```

Only `application`, `from` and `to` are required. `deploy` replaces the deploy section of the source descriptor, whose targets are never copied. Responds like `POST /deployments`.

### Manual rollback

```http
POST /api/v1/rollbacks

{"application": "myapp", "environment": "prod", "version": "1.3.2"}
```

Without `version` it goes back to the version before the live one. It also accepts `id`, `dry_run` and `freeze_override`. Responds like `POST /deployments`; `422` if there is no version to go back to or the artifact type does not allow rollback.

### Inventory and freezes

```http
GET /api/v1/inventory?application=myapp
GET /api/v1/freezes?environment=prod
```

They return `{"inventory": [...]}` and `{"freezes": [...]}`. Freeze windows are visible to every authenticated identity.

```json title="Freeze window"
{"name": "weekend", "applications": ["*"], "environments": ["prod"], "active": false,
 "start": "2026-10-16T18:00:00+02:00", "end": "2026-10-19T08:00:00+02:00", "recurring": true}
```

### Freezes at runtime and policies

```http
POST /api/v1/freezes

{"name": "incident-881", "reason": "incident 881", "applications": ["*"], "environments": ["prod"],
 "to": "2026-10-18T18:00:00Z", "hard": true}
```

`from` defaults to now. Responds `201` with the window; `403` without `freeze.write` on every application/environment of the window; `422` for an invalid name, a duplicate or a window in the past. `DELETE /api/v1/freezes/{name}` responds `204`, `404` for a window not created at runtime.

```http
POST /api/v1/policies

{"files": {"governance.rego": "package manent.governance\n…", "data.yaml": "…"}, "comment": "DBA approval"}
```

Responds `201` with the draft revision and its test results; `422` with the compilation error or the failed tests. `{revision}` is a number or `current` (the revision in force). See [Policies in Rego](policy.md#revisioni).

## Composite releases { #release-composte }

`POST /api/v1/releases` submits a [composite release](catene.md#release-composte). The body is the release in JSON with the descriptors included (`descriptor`; `file` is only for the CLI):

```json
{
  "apiVersion": "manent/v1", "kind": "Release", "id": "shop-2026-10", "onFailure": "rollback",
  "deploys": [
    {"name": "db",  "descriptor": {"apiVersion": "manent/v1", "application": "shop-db", "environment": "prod", "artifact": {"type": "migration", "name": "shop-db", "version": "42", "source": "https://…"}}},
    {"name": "api", "after": ["db"], "descriptor": {"apiVersion": "manent/v1", "application": "shop-api", "environment": "prod", "artifact": {"type": "application", "name": "shop-api", "version": "2.4.0", "source": "https://…"}}}
  ]
}
```

It responds `202` with the state of the release: `id`, `state` (`PENDING`, `SUCCESS`, `ERROR`, `CANCELLED`), `on_failure`, `requested_by`, `error` and `deploys`, in topological order, with `name`, `deployment_id`, `application`, `environment`, `version`, `after`, `state`, `error`, `rollback_id` and `rollback_state`. An invalid release (cycles, unknown dependencies, `schedule` or `dryRun` in a deploy) is `400`; a deploy refused by policies or governance is `422`. In the response the deploys are `ENQUEUED`: the release workflow queues them right after, and the following reads report their actual state. Submitting the same `id` again returns the existing release.

`GET /api/v1/releases[?limit=]` returns `{"releases": [...]}`, `GET /api/v1/releases/{id}` a single release, `POST /api/v1/releases/{id}/cancel` cancels it (`204`).

## Logs, timeline and DORA metrics

```sh
curl -H "Authorization: Bearer $TOKEN" https://manent.example.com/api/v1/deployments/7c0e…/steps
curl -H "Authorization: Bearer $TOKEN" "https://manent.example.com/api/v1/deployments/7c0e…/logs?offset=0"
```

```json
{
  "lines": [
    {"time": "2026-10-10T12:03:12Z", "level": "INFO", "step": "artifacts", "message": "artifact retrieved",
     "attrs": {"sha256": "2d27…"}}
  ],
  "next": 1,
  "done": false
}
```

To follow a deploy, repeat the request with `offset` equal to `next` until `done` is `true`. `GET /api/v1/metrics/dora?application=&environment=&since=` accepts for `since` an RFC 3339 date, a duration (`720h`) or a number of days (`30d`, default). All of them require `deployment.read`. See [Observability](../operazioni/osservabilita.md).

### Live events { #eventi-in-tempo-reale }

`GET /api/v1/deployments/{id}/events` sends the status and the new log lines of a deploy as Server-Sent Events, with the same permissions as status and log:

| Event | Data |
|---|---|
| `status` | The deploy status, as `GET /api/v1/deployments/{id}`, every time it changes. |
| `log` | An array of new log lines. The event `id` is the next offset: a client that reconnects with `Last-Event-ID` (or `?offset=`) resumes from there. |
| `end` | The deploy is over and all its lines have been sent; the server closes the stream. |
| `error` | Reading status or log failed. |

A `: keep-alive` comment every 15 seconds keeps the connection open through proxies. `EventSource` cannot send an `Authorization` header: with authentication, read the stream with `fetch` (as the web interface does) or with a client that supports headers (`curl -N`).

### Identity and approvals

`GET /api/v1/me` returns the identity of the request (`subject`, `name`, `method`, `groups`), whether authentication is enabled and the roles that apply to it, with applications and environments (`grants`). `GET /api/v1/approvals` returns `{"approvals": [...]}`: the deploys waiting for a decision, before the start or between two waves, that the caller has `deployment.approve` for and did not request. The CLI exposes them as `manent whoami` and `manent approvals`.

### Activity and workers

`GET /api/v1/activity[?application=&environment=]` returns the deploys not yet finished, from the most recent, in five lists: `running` (with `executor`, `started_at`, `last_step`, the last recorded step, and `rollout_progress`), `awaiting_approval`, `waiting_dependency` (with `after`), `queued` (oldest first: they leave the queue in that order) and `delayed` (by start time). `truncated` is `true` if there were more active workflows than examined (1000). It contains only deploys the caller can read.

`GET /api/v1/workers` returns `{"workers": [...]}`: for each worker `id`, `state` (`alive`, `stale`, `stopped`, `unregistered`), `version` (workflow version), `build`, `hostname`, `region`, `started_at`, `heartbeat_at`, `recovered_by`, `pending` (pending workflows per version) and `tasks`, the running workflows (`kind`: `deploy`, `pass`, `gate`, `approval`, `schedule`, `after`, `release`, with `deployment_id`, application, environment, region and `started_at`). It requires `platform.read`; `hidden_tasks` counts the workflows of applications the caller cannot read. Each worker also carries `queues`, the queues it dequeues from, and `resources`, the sample it sends at every heartbeat: `cpus` and `cpu_limit` (CPUs of the host and usable by the process, which follows the container quota), `process_cpu_cores` (CPU used since the previous sample), `memory_total_bytes`, `memory_available_bytes` and `memory_limit_bytes` (host and container), `process_memory_bytes`, `load1`/`load5`/`load15`, `workdir_free_bytes`, `concurrency`, `goroutines` and `sampled_at`; values the operating system does not provide are missing.

`GET /api/v1/queues` returns `{"queues": [...]}`: for each queue `name`, `kind` (`deployments`, `approvals`, `region`, `other`), `region`, the limits `worker_concurrency`, `partition_concurrency` and `global_concurrency`, the counts `enqueued`, `running` and `delayed`, `oldest_enqueued` and `workers`, the live workers that listen to it. It requires `platform.read`.

The CLI exposes them as `manent activity`, `manent workers` and `manent queues`.

Requests propagate the W3C tracing context (`traceparent`): a client's trace, for example a CI pipeline, continues into the API and the worker's steps.

## Errors

Errors always have the shape:

```json
{"error": "message"}
```

| Status | Meaning |
|---|---|
| `400` | Malformed request or descriptor. |
| `401` | Missing or invalid token. |
| `403` | Permission denied. |
| `404` | Deploy, schedule, revision or window not found. |
| `422` | Request not acceptable (policy, governance, past time, ...). |
| `503` | Temporarily unavailable: for example no governance policy loaded. Retry. |

## Access log

Every request is recorded in the application log and hence also forwarded to the SIEM, with method, path, status, duration, client IP and identity (`actor`). Relevant actions also produce an [audit](../operazioni/accesso.md#audit) entry.
