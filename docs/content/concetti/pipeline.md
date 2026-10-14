# Pipeline and steps

## Standard steps

| Step | What it does | Main outputs |
|---|---|---|
| `config` | Reads the [environment configuration](../guida/configurazione-ambiente.md) and merges it with the descriptor. It is not part of the plan: it always runs first. | `source`, `revision`, `targets` |
| `secrets` | Checks that all declared secrets can be resolved, without returning their values. It also checks that the secrets used by callbacks are declared. | `count` |
| `artifacts` | Downloads the artifact into the deploy's working directory and computes its sha256. | `local_path`, `sha256`, `size` |
| `qa` | Quality checks: non-empty artifact and, if `artifact.sha256` is set, matching hash. | outcome of each check |
| `deploy` | Picks the plugin, validates the request and runs the deploy on the targets. | `plugin`, `result` |
| `post-test` | Calls the `postTest.http` endpoints and checks the expected HTTP status (default 200). | outcome of each URL |
| `activation` | Makes the release operational, when needed: for many artifacts the deploy is enough, which is why it is optional. The initial implementation only records the activation. | `status` |

With [rollback](../guida/rollback.md) enabled the worker adds further DBOS steps, which are not part of the plan:

| Step | When | Main outputs |
|---|---|---|
| `checkpoint:deploy` | Before `deploy`: checks rollback support and records the live release. | `plugin`, `locator`, `previous_version` |
| `rollback:<step>` | After a step in `rollback.on` fails, for every already-started step that can undo itself (`deploy`, `activation`), in reverse order. | `rollback` (`completed` or `skipped`), `version`, `result` |

## Custom steps

Besides the standard steps, `pipeline.steps` can contain **custom steps**: phases added without writing Go, identified by a `type`.

| Type | What it does | Typical uses |
|---|---|---|
| `command` | Runs a command on the deploy targets over SSH, on all of them or on the first only (`run_once`). | Database migrations, cache warm-up, maintenance scripts. |
| `http` | Sends an HTTP request from the worker and, if needed, waits for the outcome by polling a URL. | CDN invalidation, notifying external systems, starting a job and waiting for it. |

```yaml title="manent.yml"
pipeline:
  steps:
    - {name: deploy, mode: mandatory}
    - name: cdn-purge
      mode: optional
      enabled: true
      type: http
      secrets: [cdn-token]
      params:
        zone: {pattern: "[a-f0-9]{32}", required: true}
      run:
        url: "https://api.cdn.example.com/zones/{{ .Params.zone }}/purge"
        auth: {type: bearer, secret: cdn-token}
```

They follow the same rules as the standard steps (mode, order, retries, dry-run) with a few differences:

- **Who defines them.** Commands and URLs are written only in `manent.yml`, managed by the manent administrators. The environment configuration provides parameter values (`step_params`); the descriptor can turn optional steps on or off and set only the parameters allowed by `descriptor_policy.step_params`.
- **Declared inputs and outputs.** Every parameter and every output key is declared with a pattern; substituted values are checked for the context they end up in (command arguments, URLs, JSON bodies).
- **Stored in the plan.** The complete specification is stored in the plan, optionally signed: a deploy resumed after a crash runs the definition it started with, and a faulty specification can be revoked.
- **Position.** With [wave rollouts](../guida/ondate.md), a custom step between `deploy` and `activation` runs for every batch with the batch's targets.

See [Custom steps](../guida/step-personalizzati.md) for the full reference.

## The plan { #il-piano }

The plan is the ordered list of steps to run. It is computed **before** the deploy is enqueued, by combining the configuration (`pipeline.steps` in `manent.yml`) with the descriptor's `steps` section, and it is passed to the workflow as input.

```yaml title="configs/manent.yml"
pipeline:
  steps:
    - name: secrets
      mode: mandatory
    - name: artifacts
      mode: mandatory
      max_retries: 3
    - name: qa
      mode: optional
      enabled: true
    - name: deploy
      mode: mandatory
      max_retries: 2
    - name: post-test
      mode: optional
      enabled: false
      max_retries: 5
    - name: activation
      mode: optional
      enabled: true
```

Rules:

- `mandatory` steps always run: the descriptor cannot disable them;
- `optional` steps start from the `enabled` value and the descriptor can change it;
- the order always follows the configuration, never the descriptor;
- an unknown name in the descriptor's `steps` section is an error.

```yaml title="Descriptor"
steps:
  qa: false
  post-test: true
```

The plan can be inspected without running anything, with `manent validate` or `POST /api/v1/deployments/plan`.

## Retries

`max_retries` is the number of additional attempts of a step on error. Retries are handled by DBOS with an increasing interval. Once the last attempt is exhausted, the deploy stops in state `ERROR` and the following steps are not run. If the policy says so, the worker performs a [rollback](../guida/rollback.md) before finishing.

## Compensable steps

A step that changes environments can declare how to undo its effects by implementing:

```go
// Compensator undoes the step's effects during rollback. It must be idempotent.
type Compensator interface {
	Compensate(ctx context.Context, in Input, checkpoint Output) (Output, error)
}

// Checkpointer records, before the step, the state to return to. It must have no effects.
type Checkpointer interface {
	Checkpoint(ctx context.Context, in Input) (Output, error)
}
```

The checkpoint is a DBOS step separate from the one it protects: the step's retries do not repeat it, so it always records the state before any change. A step that does not implement `Compensator` is not undone.

## Data between steps

Steps exchange data only through their output, a `string → string` map that DBOS saves in the database. The `deploy` step, for example, reads `local_path` and `sha256` from the output of `artifacts`.

!!! danger "Outputs never contain secrets"
    Outputs are persisted and visible in the deploy status. The `secrets` step only checks that secrets can be resolved; plugins read their values only at the moment of use.

## Working directory

Each deploy uses the `workdir/<deployment-id>` directory of the worker running it. `workdir` does not need to be shared between workers: if a deploy is resumed on a worker that does not have the artifact, the `deploy` step downloads it again and verifies the sha256 recorded by `artifacts`. When the deploy ends, on success or error, the directory is removed.

## Plugin selection

The `deploy` step picks the plugin through a **policy**, given by `deploy.policy`. The default policy:

1. uses `deploy.plugin`, if set, and checks that it supports the artifact type;
2. otherwise uses the first active plugin, in alphabetical order, that supports `artifact.type`.
