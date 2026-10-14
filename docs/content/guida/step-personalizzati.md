# Custom steps

Custom steps add phases to the pipeline without writing Go: an **HTTP request** (invalidate a CDN, notify a system, start a job and wait for it) or a **command on the targets** over SSH (a database migration, a cache warm-up). They get the same guarantees as the standard steps: each run is a DBOS step with retries, a deploy resumed after a crash continues from where it stopped, the output goes to the deploy log, and every run is recorded in the audit.

Commands and URLs are written **only in `manent.yml`**. The environment configuration provides the parameters for each application, and the descriptor can only turn optional steps on or off and, if the policy allows it, set a few parameters.

```yaml title="manent.yml"
pipeline:
  custom:
    spec_key_refs: ["vault:secret/data/manent/pipeline#spec_key"]
  steps:
    - {name: secrets, mode: mandatory}
    - {name: artifacts, mode: mandatory, max_retries: 3}
    - name: db-migrate
      mode: optional
      enabled: false                 # turned on per application by the descriptor
      type: command
      max_retries: 0
      timeout: 15m
      secrets: [db-admin]            # logical names, from the secrets section
      ssh: {user: migrator, key_secret: ssh-migrate}
      params:
        database: {pattern: "[a-z][a-z0-9_]{0,62}", required: true}
      outputs:
        applied: {pattern: "[0-9]{1,6}"}
      run:
        run_once: true               # one target only, not all of them
        command: ["/opt/tools/migrate", "up", "--to", "{{ .Version }}", "--db", "{{ .Params.database }}"]
      dry_run:
        command: ["/opt/tools/migrate", "plan", "--to", "{{ .Version }}", "--db", "{{ .Params.database }}"]
        secrets: [db-readonly]       # the dry-run has its own, read-only secrets
    - {name: deploy, mode: mandatory, max_retries: 2}
    - {name: activation, mode: optional, enabled: true}
    - name: cdn-purge
      mode: optional
      enabled: true
      type: http
      secrets: [cdn-token]
      params:
        zone: {pattern: "[a-f0-9]{32}", required: true}
      outputs:
        purge_id: {pattern: "[a-z0-9-]{1,64}", path: "$.result.id"}
      run:
        method: POST
        url: "https://api.cdn.example.com/zones/{{ .Params.zone }}/purge"
        auth: {type: bearer, secret: cdn-token}
        json: {tags: ["{{ .Application }}"]}
        poll:
          url: "https://api.cdn.example.com/purges/{{ .Response.result.id }}"
          until: {path: "$.status", in: [done]}
          fail: {path: "$.status", in: [failed]}
```

```yaml title="envconfig/shop/prod.yml"
step_params:
  db-migrate: {database: shop}
  cdn-purge: {zone: 0f3a9c1e5b7d42a8a1c3e5f7091b2d4c}
secrets:
  - {name: db-admin, ref: "vault:secret/data/shop/prod#db_admin"}
  - {name: db-readonly, ref: "vault:secret/data/shop/prod#db_readonly"}
  - {name: ssh-migrate, ref: "vault:secret/data/shop/prod#ssh_migrate"}
  - {name: cdn-token, ref: "vault:secret/data/shop/prod#cdn_token"}
```

The position in `pipeline.steps` decides when the step runs. With [wave rollouts](ondate.md), a step between `deploy` and `activation` runs for every batch, with the batch's targets; a step before `deploy` or after `activation` runs once.

## Who decides what

A custom command step is remote code execution: whoever writes `manent.yml` can run commands on every target. That is accepted, since it is the same trust as configuring plugins. Everything else is built so that **nobody else** gets the same power.

| Element | `manent.yml` | Environment configuration | Descriptor |
|---|---|---|---|
| Command, URL, method, position, type, `mode` | yes | no | no |
| Parameter and output declarations (patterns) | yes | no | no |
| Turning an optional step on or off | default | no | yes |
| Parameter values | defaults | yes (`step_params`) | only those allowed by `descriptor_policy.step_params` (`stepParams`) |
| Secret references | logical names | yes | according to `descriptor_policy.secret_refs` |

A step that acts as a control (a scan, a signature check) must be `mandatory`: the descriptor can turn off an optional step.

```yaml title="manent.yml"
descriptor_policy:
  step_params:
    db-migrate: [database]   # the descriptor may set stepParams.db-migrate.database
```

```yaml title="Descriptor"
steps: {db-migrate: true}
stepParams:
  db-migrate: {database: shop}
```

## Parameters and placeholders

Every parameter is declared with a `pattern` (a regular expression the whole value must match), and optionally `required` and `default`. A value that does not match, an undeclared parameter or a missing required one rejects the deploy when it is submitted, if the environment configuration can be read, or at the latest when the step runs.

Placeholders use the Go template syntax with these values only:

| Placeholder | Value |
|---|---|
| `{{ .Application }}`, `{{ .Environment }}` | Application and environment. |
| `{{ .Version }}`, `{{ .Artifact }}` | Artifact version and name. The version must match `^[0-9A-Za-z][0-9A-Za-z.+_-]{0,127}$`, otherwise the step fails. |
| `{{ .DeploymentID }}` | Deploy ID. |
| `{{ .SHA256 }}` | SHA-256 of the artifact. |
| `{{ .DryRun }}` | `true` in dry-run. |
| `{{ .Params.<name> }}` | A declared parameter. |
| `{{ .Outputs.<step>.<key> }}` | An output of a previous step (`{{ index .Outputs "post-test" "key" }}` for names with `-`). |
| `{{ .Response.<field> }}` | Only in `poll.url`: a field of the first response. |

The values come from whoever deploys, from the environment configuration or from other targets, so they are **not trusted** and are checked for the context they end up in:

- an unknown placeholder or an undeclared parameter is a configuration error, found at startup;
- a missing value is an error, never `<no value>` or an empty string;
- in a command, a substituted value cannot make the argument empty, add control characters, or start with `-` when the written argument does not (`-oProxyCommand=…` would be read as an option);
- in a URL every substituted value is percent-encoded, so it stays inside one path segment or query value, and `.` or `..` segments are rejected; scheme and host must be written in full, without placeholders;
- the JSON body is a structure that manent serializes: placeholders are allowed only in string values and a value cannot change the structure.

## Command steps

`type: command` runs a command on the deploy targets over SSH, one target after the other, or on the first target only with `run_once`. The command is a list of arguments whose first element is an absolute path without placeholders: it is never a string handed to a shell.

| Key | Description |
|---|---|
| `ssh.key_secret`, `ssh.password_secret` | Logical names of the SSH credentials (one is required). |
| `ssh.user` | User for the command instead of the target's: prefer a dedicated user with only the privileges the step needs. |
| `run.run_once` | Run on the first target only. Required when the step declares outputs. |
| `run.command` | The command and its arguments. |
| `run.sha256` | SHA-256 of `command[0]`, checked on the target before every run (`sha256sum` or `shasum -a 256`). It protects against a wrong binary, not against a compromised target. |

On every target and attempt manent:

1. creates `<secrets_dir>/<deploy>/<step>/<random>` with `umask 077`, removing the directories of earlier attempts (also those left by a crash); `secrets_dir` is `/dev/shm/manent` by default, so the secrets live in memory;
2. writes the environment file and one file per secret (`0600`), over the SSH channel, never on the command line;
3. runs `/bin/sh -c '<script>' manent <dir> <arguments…>`: every argument is quoted for a POSIX shell and passed as a positional parameter, the `MANENT_*` variables are loaded from the file, so they do not show up in `ps`, and with `timeout(1)` available the command is stopped when the step times out;
4. reads the outputs and removes the directory.

The target's login shell must be POSIX-compatible (`sh`, `bash`, `dash`, `zsh`); Windows targets are not supported. The host key is always checked against `pipeline.custom.known_hosts`.

### Contract

| Variable | Value |
|---|---|
| `MANENT_DEPLOYMENT_ID`, `MANENT_STEP` | Deploy ID and step name: together they are the idempotency key. |
| `MANENT_APPLICATION`, `MANENT_ENVIRONMENT`, `MANENT_VERSION`, `MANENT_TARGET` | The release and the current target. |
| `MANENT_ARTIFACT_SHA256` | SHA-256 of the verified artifact. |
| `MANENT_DRY_RUN` | `true` when `dry_run` is running. |
| `MANENT_PARAM_<NAME>` | Step parameters (`database` → `MANENT_PARAM_DATABASE`). |
| `MANENT_OUTPUT_<STEP>_<KEY>` | Outputs of previous steps (`post-test` → `POST_TEST`). Untrusted data: they may come from another target. |
| `MANENT_SECRETS_DIR` | Directory with one file per declared secret. |
| `MANENT_OUTPUT` | File where the step writes `key=value` lines. |

| Result | Effect |
|---|---|
| exit `0` | Success. The lines of `$MANENT_OUTPUT` become the step output, **only** for keys declared in `outputs` and only if the value matches their pattern; anything else fails the step. At most 16 KiB. |
| exit `75` (`EX_TEMPFAIL`) | Temporary error: retried up to `max_retries`. |
| other exit codes | Permanent error: no retries, rollback if the policy says so. |
| timeout, connection lost | Temporary error. |

stdout and stderr go to the deploy log line by line (at most 1 MiB per run and 4 KiB per line), with the values of the declared secrets replaced by `***`, also in base64, URL-encoded and JSON-escaped form. It is an extra defense, not a guarantee: a secret split across lines, truncated or transformed by the script is not recognized, and the log can be read by anyone with `deployment.read`.

!!! warning "At least once"
    `max_retries: 0` limits retries, not re-runs: a step interrupted by a worker crash runs again during the recovery, because DBOS did not record its result. Scripts that are not idempotent (a migration, a drain) must use `MANENT_DEPLOYMENT_ID` and `MANENT_STEP` as a deduplication key.

## HTTP steps

`type: http` sends a request from the worker.

| Key | Description |
|---|---|
| `run.method` | `GET`, `POST` (default), `PUT`, `PATCH`, `DELETE`. |
| `run.url` | URL with literal scheme and host; `https` only, unless `pipeline.custom.allow_http`. |
| `run.headers` | Extra headers, with placeholders in the values. `Host`, `Content-Type` and the other headers handled by manent are not allowed. |
| `run.auth` | `bearer`, `basic` (`username`) or `header` (`header`) with a secret listed in `secrets`. |
| `run.json` | Request body. |
| `run.expect` | Accepted statuses; default any 2xx. Outside `expect`: 5xx, 408 and 429 are temporary, the others permanent. |
| `run.poll` | Wait for an asynchronous operation: `url`, `interval` (default 10s), `timeout` (default the step's), `until` and `fail` with `path` and `in`. |
| `outputs.<key>.path` | Where to read the output in the JSON response (`$.a.b[0]`, `$["a.b"]`). |

Every request carries `Idempotency-Key: <deploy>:<step>`. Redirects are not followed, and responses are read up to 1 MiB. The wait happens inside the step: after a crash the step starts again and repeats the first request with the same key. The log shows method, host, path and status, never the query or the body.

## Dry-run

A step with a `dry_run` section runs it during [dry-runs](dry-run.md); without it, the step is skipped. Dry-runs are not subject to [governance](governance.md) (no approvals, no freeze checks), yet `dry_run` runs on production targets. So:

- `dry_run` receives **only its own** `secrets`, never those of `run`: give it read-only credentials;
- during a freeze, command steps are skipped in dry-run too (`dry_run: skipped`, `reason: freeze`), unless the descriptor has a `freezeOverride`;
- manent cannot check that `dry_run` is really read-only: that is up to the author.

## The specification in the plan

When a deploy is submitted, the plan stores the **complete specification** of every custom step, in canonical form, with its SHA-256 (`spec_hash`). A deploy resumed after a crash, a deploy waiting for approval and the runs of a [recurring schedule](pianificazione.md) all execute the definition they started with, even if `manent.yml` changes in the meantime. The plan shown by `manent validate`, by the API and by the web interface has the type and `spec_hash` of each step, never commands and URLs.

**Signature.** With `pipeline.custom.spec_key_refs` the specification is signed with HMAC-SHA256 when the plan is computed, and the worker checks the signature before running it: whoever can write to the DBOS database cannot turn a plan into arbitrary commands. The first key signs, all of them verify, so a key can be rotated by adding the new one first and removing the old one once no deploy or schedule signed with it is left. Without keys the specifications are not signed and the server warns at startup.

**Revocation.** A fix to `manent.yml` does not reach plans already stored, especially those of cron schedules. To stop a specification, add its hash to `pipeline.custom.revoked_specs`: the worker refuses to run it and the step fails with a permanent error, without changing the sequence of steps of running workflows. The hash is in the plan (`manent validate`, `POST /api/v1/deployments/plan`) and in the audit entries.

```yaml title="manent.yml"
pipeline:
  custom:
    revoked_specs:
      - 3b0f4c…e91a   # db-migrate before the fix of 2026-10-14
```

## Audit and visibility

Every run of a custom step, including failed attempts and dry-runs, writes an audit entry `step.custom.run` with the identity that requested the deploy, the step, the specification hash, the targets and the outcome. The outputs stay in the deploy status, readable with `deployment.read`; the [callbacks](callback.md), which the descriptor defines, receive the outcome of the step but **not its outputs**.

## Reference

| `pipeline.steps[]` | Default | Description |
|---|---|---|
| `type` | | `command` or `http`. Without it the step is a standard one. |
| `timeout` | `10m` | Maximum duration of a run (1s to 6h). |
| `secrets` | | Logical names of the secrets passed to `run`. |
| `params.<name>` | | `pattern` (required), `required`, `default`. |
| `outputs.<key>` | | `pattern` (required), `path` for HTTP steps. |
| `ssh` | | `user`, `key_secret`, `password_secret` (command only). |
| `run`, `dry_run` | | The run and the dry-run; `dry_run.secrets` lists the dry-run's secrets. |

| `pipeline.custom` | Default | Description |
|---|---|---|
| `spec_key_refs` | | References to the HMAC keys (at least 32 bytes) that sign the specifications. |
| `revoked_specs` | | SHA-256 of specifications that must not run any more. |
| `known_hosts` | `~/.ssh/known_hosts` | Host keys of the targets. |
| `insecure_ignore_host_key` | `false` | Development only. |
| `secrets_dir` | `/dev/shm/manent` | Secrets directory on the targets, on tmpfs. |
| `allow_http` | `false` | Allow `http` URLs. |

!!! note "In this version"
    Commands run only on the targets (`on: targets`). Running inside a Kubernetes Job, checkpoints and compensations for the [rollback](rollback.md), and steps written as external plugins are planned for the next phases. Running commands on the worker is not supported: a process with the worker's user could read its configuration and its database.
