# Dry-run

A dry-run runs the pipeline on a worker like a real deploy, but **without changing the environments**. It answers the question: if I launched this deploy now, would it succeed?

```sh
manent deploy -f descriptor.yml --dry-run
```

```sh
curl -X POST 'localhost:8080/api/v1/deployments?dry_run=true' \
  -H 'Content-Type: application/json' -d @descriptor.json
```

Alternatively, write `dryRun: true` in the descriptor.

## Differences from `validate`

| | `validate` | Dry-run |
|---|---|---|
| Where it runs | On the server (API), without enqueuing anything | On a worker, from the queue |
| Plan and descriptor validation | ✓ | ✓ |
| Descriptor policy, callbacks, artifact source | ✓ | ✓ |
| Environment configuration | ✓ (if unreachable, only a warning) | ✓ |
| Secret resolution | | ✓ |
| Artifact download and verification | | ✓ |
| QA checks | | ✓ |
| Target checks through the plugin | | ✓ |
| Rollback policy | ✓ | ✓ |
| Live release and plugin rollback support | | ✓ |

## What each step does

In a dry-run a step runs only if it declares that it can simulate itself. A step that does not declare it is **skipped**: a new step never has effects in a dry-run by oversight.

| Step | In a dry-run |
|---|---|
| `config` | Runs normally. |
| `secrets` | Runs normally: checks the secrets. |
| `artifacts` | Runs normally: the artifact is downloaded only into the worker's working directory, which is then removed. |
| `qa` | Runs normally. |
| `checkpoint:deploy` | Only with [rollback](rollback.md) enabled. Runs normally: checks that the plugin supports rollback and reads the live release. |
| `deploy` | Picks and validates the plugin as in a real deploy, then asks it to **check** the targets without changing them. |
| `post-test` | Skipped. |
| `activation` | Skipped. |

In the result, skipped steps have output `{"dry_run": "skipped"}`. The `deploy` step reports `"dry_run": "checked"` with the outcome per target, or `"dry_run": "validated"` if the plugin cannot check targets and the run stopped at request validation.

For example, the [remote-copy](../plugins/remote-copy.md) plugin connects to each target, checks that the destination directory is writable (or can be created) and that `sha256sum` is available, but copies nothing and does not run `post_command`.

## Rules

- **ID:** with an `id` in the descriptor, the dry-run uses `<id>.dry-run` as workflow ID. This way it does not consume the idempotency key and the real deploy with the same `id` can still run.
- **Queue:** the dry-run uses the same queue as deploys and respects `environment_concurrency`. While it is running, a real deploy of the same environment waits.
- **Schedule:** a dry-run is not allowed together with `schedule`.
- **Callbacks:** [callbacks](callback.md) receive events during a dry-run too, with `"dry_run": true`.
- **Rollback:** a failed dry-run never runs compensations, even with rollback enabled.

## Supporting dry-run in a plugin

See [Writing a plugin](../plugins/index.md#dry-run).
