# Deploy chains

Sometimes a deploy must start only after another one has succeeded: the database migration before the backend, the backend before the frontend, a shared service before the applications that use it. manent offers two levels:

- **dependencies (`after`)**: a single deploy waits for other deploys, identified by their ID, to succeed;
- **composite releases (`kind: Release`)**: a single request with several deploys and the dependencies between them, followed as a whole, with a policy for failures.

The second level is built on the first: every deploy of a release is a normal deploy that waits for its predecessors.

## Dependencies: `after`

```yaml title="api.yml"
apiVersion: manent/v1
id: shop-api-2.4.0-prod
application: shop-api
environment: prod
after:
  - shop-db-42-prod          # IDs of deploys that must succeed first
afterTimeout: 2h             # maximum wait, default 24h (from 1m to 30 days)
artifact: {type: application, name: shop-api, version: 2.4.0, source: https://…}
```

From the CLI the dependencies can also be added with `--after`, repeatable:

```sh
manent deploy -f api.yml --after shop-db-42-prod
```

### How it works

```mermaid
flowchart LR
    S[Submit] --> W[WAITING_DEPENDENCY]
    W -->|all dependencies SUCCESS| A{Approval required?}
    A -->|yes| AA[AWAITING_APPROVAL] --> Q[ENQUEUED → deploy]
    A -->|no| Q
    W -->|a dependency fails, is cancelled or rejected| F[DEPENDENCY_FAILED]
    W -->|afterTimeout elapsed| F
```

- **At submit time** every dependency must exist, must not be a dry-run and must not have already ended without success: otherwise the request is refused (`422`). If they have all already succeeded, the deploy is queued straight away.
- **While waiting** the deploy is `WAITING_DEPENDENCY` and **does not take a slot of the environment queue**: like an approval, the wait runs on the approval queue. A waiting deploy therefore never blocks the other deploys of the environment, and two deploys waiting for each other cannot deadlock the queue.
- The wait is a durable DBOS workflow: it survives worker restarts. When a dependency ends (succeeded, failed, cancelled, rejected) it **immediately notifies** the waits that follow it, which read the state again with a single query. The wait also reads the state every `chains.poll_interval` (default 5s), doubling the interval up to 5 minutes: this covers the cases without a notification, such as a deploy deleted by retention.
- **When all dependencies have succeeded** the approval is requested, if the [governance](governance.md#approvazioni) rule requires one (so approvers are asked only for a deploy that can really start), then the deploy is queued with the ID already returned. With `schedule.at`, the deploy starts at the later of the time and the success of the dependencies.
- **If a dependency does not succeed** (error, cancelled, rejected, expired, dependency failed in turn) or the wait expires, the deploy is not run and ends as `DEPENDENCY_FAILED`, with the reason in `error` and in `after.reason`. The outcome is recorded in the audit log (`after.finish`).
- A waiting deploy can be cancelled like any other (`manent cancel <id>`).

The deploy status reports the dependencies:

```json
{
  "id": "shop-api-2.4.0-prod",
  "state": "WAITING_DEPENDENCY",
  "after": {
    "status": "waiting",
    "expires_at": "2026-10-12T14:00:00Z",
    "dependencies": [{"id": "shop-db-42-prod", "state": "PENDING"}]
  }
}
```

### Rules

- `after` is not allowed with `dryRun` (a dry-run runs immediately) nor with `schedule.cron` (every run would depend on the same deploys). At most 20 dependencies, a deploy cannot depend on itself.
- The dependencies may belong to other applications and environments, but the requester needs `deployment.read` on them: otherwise the outcome of the wait would reveal the state of other teams' deploys.
- A [promotion](governance.md#promozione) or a [manual rollback](governance.md#rollback-manuale) does not inherit the dependencies of the source deploy.

## Composite releases { #release-composte }

A release groups several deploys with the dependencies between them. Independent deploys run in parallel, the others after their predecessors.

```yaml title="release.yml"
apiVersion: manent/v1
kind: Release
id: shop-2026-10            # required: it is the idempotency key
onFailure: rollback         # stop (default) | continue | rollback
deploys:
  - name: db
    file: release/db.yml    # deploy descriptor, relative to this file
  - name: api
    file: release/api.yml
    after: [db]             # names of other deploys of the release
  - name: worker
    file: release/worker.yml
    after: [db]
  - name: web
    file: release/web.yml
    after: [api]
```

```mermaid
flowchart LR
    db --> api --> web
    db --> worker
```

```sh
manent release submit -f release.yml      # submits and follows the progress (--no-wait, --timeout)
manent release status shop-2026-10
manent release list
manent release cancel shop-2026-10
```

Through the API the descriptors are included in the request (`descriptor` instead of `file`): see [HTTP API](api.md#release-composte).

### How it works

- **Checks first, then submission.** The release is validated as a whole (unique names, existing dependencies, **no cycles**, no `schedule` or `dryRun` in the deploys), then plan, policies, environment configuration, governance rules (e.g. a freeze) and dependencies outside the release are checked for **every** deploy: an error refuses the whole request immediately. Only then is the release accepted.
- **Queuing the deploys.** The release workflow queues the deploys, in topological order, as its first step: a server crash right after the submission does not leave deploys without the release that follows them. Newly accepted deploys are `ENQUEUED` until the workflow has queued them. If a deploy is refused at that moment (e.g. a freeze that started in the meantime), those already queued are cancelled: the refused one is `ERROR`, the following ones `CANCELLED`, the release `ERROR`.
- **IDs.** Each deploy gets the ID of its descriptor, if it has one, otherwise `<release>.<name>` (e.g. `shop-2026-10.api`). The dependencies inside the release become `after` between these IDs; `after` already in a descriptor (dependencies outside the release) is kept.
- **Each deploy is a normal deploy**: its own plan, permissions, governance (approvals, freeze, mandatory promotion), automatic rollback and waves. In lists and in the status it carries `release`.
- **Following.** The `manent.release` workflow, on the approval queue, reads the state of all the deploys with a single query, is notified when one of them (or its rollback) ends, and applies `onFailure`. Submitting the same release again returns the existing one. Cancelling the release (`manent release cancel`) stops the workflow first, then cancels the deploys that have not finished.

### `onFailure`

| Value | When a deploy does not succeed |
|---|---|
| `stop` (default) | Deploys not yet started (waiting, awaiting approval, queued) are cancelled. Those already running finish. |
| `continue` | Deploys that do not depend on the failed one continue; those that depend on it end as `DEPENDENCY_FAILED`. |
| `rollback` | Like `stop`; then the deploys that **succeeded** are rolled back to their previous version, from the most dependent to the least dependent (e.g. `web`, then `api`, then `db`). |

The rollbacks of `onFailure: rollback` are [manual rollbacks](governance.md#rollback-manuale) with ID `<release>.rollback.<name>`, run with the identity of whoever submitted the release: they follow the same rules, including the [artifact type policy](rollback.md) (a `migration` cannot be rolled back, and the sequence stops there). They proceed one at a time and stop at the first that does not succeed.

### Release state

| `state` | Meaning |
|---|---|
| `PENDING` | At least one deploy has not finished. |
| `SUCCESS` | All deploys succeeded. |
| `ERROR` | At least one deploy did not succeed: `error` lists them, `deploys[].rollback_state` reports the rollbacks. |
| `CANCELLED` | The release was cancelled. |

The outcome is recorded in the audit log (`release.finish`). The web interface shows releases in the **Releases** view, with the state of each deploy and of its rollback.

### Permissions

- Submitting a release requires, for every deploy, the same permissions as a single deploy (`deployment.submit`, `deployment.freeze-override` if the descriptor uses it, `deployment.read` on external dependencies) and, with `onFailure: rollback`, `deployment.rollback`.
- Reading a release requires `deployment.read` on all its deploys; lists show only fully readable releases. Cancelling requires `deployment.cancel` on all its deploys.
