# Deploy descriptor

The descriptor declares **what** to release and **where**. The CLI reads it as YAML, the API receives it as JSON: the fields are the same. Unknown fields are an error, so a typo is never silently ignored.

## Complete example

=== "YAML (CLI)"

    ```yaml
    apiVersion: manent/v1
    id: myapp-1.2.3-test               # optional: idempotency key
    application: myapp
    environment: test

    artifact:
      type: application
      name: myapp
      version: 1.2.3
      source: https://artifacts.example.com/myapp/myapp-1.2.3.tar.gz
      sha256: 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08

    secrets:
      - name: deploy-key
        ref: env:DEPLOY_SSH_KEY

    deploy:
      plugin: remote-copy
      targets:
        - host: 192.168.1.10
          port: 22
          user: deploy
      params:
        dest_dir: /opt/myapp/releases
        ssh_key_secret: deploy-key

    steps:
      post-test: true

    postTest:
      http:
        - url: http://192.168.1.10:8080/health
          expectStatus: 200
    ```

=== "JSON (API)"

    ```json
    {
      "apiVersion": "manent/v1",
      "id": "myapp-1.2.3-test",
      "application": "myapp",
      "environment": "test",
      "artifact": {
        "type": "application",
        "name": "myapp",
        "version": "1.2.3",
        "source": "https://artifacts.example.com/myapp/myapp-1.2.3.tar.gz"
      },
      "secrets": [{"name": "deploy-key", "ref": "env:DEPLOY_SSH_KEY"}],
      "deploy": {
        "plugin": "remote-copy",
        "targets": [{"host": "192.168.1.10", "port": 22, "user": "deploy"}],
        "params": {"dest_dir": "/opt/myapp/releases", "ssh_key_secret": "deploy-key"}
      },
      "steps": {"post-test": true},
      "postTest": {"http": [{"url": "http://192.168.1.10:8080/health", "expectStatus": 200}]}
    }
    ```

## Fields

### Identification

| Field | Required | Description |
|---|---|---|
| `apiVersion` | yes | Always `manent/v1`. |
| `id` | no | Deploy ID and idempotency key: the same `id` is never run twice. Not allowed with `schedule.cron`. |
| `application` | yes | Application name. |
| `environment` | yes | Target environment (e.g. `test`, `prod`). |

`id`, `application`, `environment` and the schedule name become directory names and keys: letters, digits, `.`, `_`, `:` and `-` are allowed, starting with an alphanumeric character, up to 128 characters.

### `artifact`

| Field | Required | Description |
|---|---|---|
| `type` | yes | Artifact type (e.g. `application`). Determines which plugins can handle it. |
| `name` | yes | Artifact name. |
| `version` | no | Version, passed to plugins and shown in logs. |
| `source` | yes | Where to download it from: local path, `file://`, `http(s)://` or a container image `oci://registry/repository:tag` (see the [container](../plugins/container.md) plugin). |
| `sha256` | no | Expected hash, checked by the `qa` step. |

### `secrets`

A list of secrets with a logical name and a **reference**. The value never appears in the descriptor: the worker reads it only when needed.

```yaml
secrets:
  - name: deploy-key
    ref: env:DEPLOY_SSH_KEY      # worker environment variable
  - name: db-password
    ref: file:/run/secrets/db    # contents of a file on the worker
```

Supported schemes: `env:`, `file:` and, if configured, `vault:` (e.g. `vault:secret/data/myapp/prod#ssh_key`). See [Secrets and artifact sources](../operazioni/segreti-artefatti.md). Logical names must be unique. Plugins, callbacks and parameters refer to secrets by logical name (e.g. `ssh_key_secret: deploy-key`).

### `deploy`

| Field | Description |
|---|---|
| `policy` | Plugin selection policy (default: `default`). |
| `plugin` | Plugin to use. If absent, the policy picks the first active plugin that supports `artifact.type`. |
| `targets` | Destinations: `host` (required), `port`, `user`, `labels` (labels; `region` and `az` are used by [wave rollouts](ondate.md)). |
| `params` | Plugin-specific parameters, as a string map. |
| `targetsFrom` | Targets taken from `targets` exports of the imports: `[{import: infra, export: vms}]`. See [Exports and imports](catene.md#export-import). |
| `paramsFrom` | Parameters taken from exports of the imports: `{db_host: {import: infra, export: db_host}}`. |

All these fields can come from the [environment configuration](configurazione-ambiente.md): the descriptor can only add what is not defined there. After the merge there must be at least one target.

### `steps`

Enables or disables optional steps by name. See [Pipeline and steps](../concetti/pipeline.md#il-piano).

### `stepParams`

Parameters of the [custom steps](step-personalizzati.md), per step (`stepParams: {db-migrate: {database: shop}}`). Usually they come from the environment configuration: the descriptor can set only those allowed by `descriptor_policy.step_params`, and every value must match the pattern declared by the step.

### `postTest`

HTTP checks run by the `post-test` step:

```yaml
postTest:
  http:
    - url: https://myapp.example.com/health
      expectStatus: 200   # default 200
```

### `schedule`

Schedules the deploy instead of running it immediately: `at` for a single time, `cron` for recurring deploys. See [Scheduled deploys](pianificazione.md).

### `dryRun`

`true` runs the deploy as a simulation. Not allowed with `schedule`. See [Dry-run](dry-run.md).

### `callbacks`

Up to 5 HTTP endpoints that receive the outcome of steps and of the deploy. See [Callbacks](callback.md).

### `rollback`

Enables or disables automatic rollback, within the limits of the artifact type's policy. See [Rollback](rollback.md).

```yaml
rollback:
  enabled: true              # default: the artifact type's policy
  on: [deploy, post-test]    # steps whose failure triggers the rollback
  locator: plugin            # how to find the release to go back to
```

### `rollout`

[Wave rollout](ondate.md) strategy: waves, bake time, health gate, behaviour on failure. It normally lives in the environment configuration; in the descriptor it is allowed only if the environment does not define one, or if it is identical.

### `after` and `afterTimeout`

IDs of deploys that must succeed before this one starts, and the maximum wait (default `24h`). Until then the deploy is `WAITING_DEPENDENCY` and does not take a slot of the environment queue. See [Deploy chains](catene.md).

```yaml
after: [shop-db-42-prod]
afterTimeout: 2h
```

### `imports` and `exports`

`imports` are the deploys whose exports this deploy reads when it starts (`deployment: <id>`, among `after`, or `current: {application, environment}`, the release in service); `exports` are the outputs this deploy offers to the others (`from: <step>.<key>`, `format: string|targets`, `pattern`). See [Exports and imports](catene.md#export-import).

```yaml
imports:
  infra: {deployment: infra-42}
exports:
  fqdn: {from: deploy.fqdn, pattern: '[a-z0-9.-]+'}
```

### `freezeOverride`

Reason for which the deploy bypasses a [freeze window](governance.md#finestre-di-freeze) (up to 500 characters, not allowed with `schedule.cron`). Requires the `deployment.freeze-override` permission and is recorded in the audit log.

```yaml
freezeOverride: "INC-4711: VAT calculation fix"
```

## Validation

The descriptor is validated as soon as it is read, by both the CLI and the API. All errors are returned together, not just the first one. Some checks happen later, because they depend on other things:

- conflicts with the environment configuration, the presence of at least one target and the [descriptor policy](../operazioni/sicurezza.md#cosa-puo-chiedere-un-descrittore) (parameters, secrets and targets the descriptor may add), at enqueue time (`422`) and again in the `config` step. If the environment configuration cannot be reached at enqueue time, the check is left to the `config` step;
- [governance](governance.md) rules (freezes, mandatory promotion, approvals), at enqueue time; freezes also in the `config` step;
- the artifact source, at enqueue time and on download (`artifacts.sources`);
- the user's permissions on the application and environment, at enqueue time;
- that secrets can be resolved, in the `secrets` step;
- the callback URL policy and the rollback policy, at enqueue time. The final rollback decision is made in the `config` step, after the merge with the environment configuration.

`manent validate` runs the validation and shows the effective deploy without enqueuing anything.
