# script

The `script` plugin runs a script **on the worker** to release an artifact on a system no other plugin covers: a vendor CLI, an appliance with its own API, an in-house tool. The script gets the same deploy as any other plugin: waves, gates, approvals, freezes, rollback, reconciliation and audit.

The scripts are a **catalog in `manent.yml`**, written by the operators. The environment configuration chooses a script by name and passes the parameters the catalog declares, checked against their patterns. It never names a command: the worker holds the database connection, the secret provider and the artifacts of other deploys, so whoever can write the configuration of an application must not be able to run code there.

## Configuration

```yaml title="manent.yml"
plugins:
  script:
    artifact_types: [tgz]          # required: the types this plugin releases
    queue: scripts                 # recommended: only the workers with the scripts run them
    max_parallel: 4
    work_dir: /var/lib/manent/script   # default <temp dir>/manent-script
    env:                           # fixed variables for every script; never secrets
      HTTPS_PROXY: http://proxy.example.com:3128
    scripts:
      f5-legacy:
        timeout: 20m               # default 10m, at most 6h
        per_target: false          # true: one run per target
        params:
          partition: {pattern: "[a-z0-9-]+", required: true}
          mode: {pattern: "fast|safe", default: safe}
        secrets: [f5-password]
        outputs:
          vip: {pattern: "[0-9.]+"}
        run:
          command: [/opt/manent/scripts/f5-deploy.sh, --verbose]
          sha256: "3b1f…"          # checked before every run
        dry_run:  {command: [/opt/manent/scripts/f5-deploy.sh, --check]}
        current:  {command: [/opt/manent/scripts/f5-current.sh]}
        rollback: {command: [/opt/manent/scripts/f5-rollback.sh]}

dbos:
  plugins: [script]                # on the workers that have the scripts
```

| Key | Description |
|---|---|
| `artifact_types` | Artifact types the plugin releases. Required: there is no default. |
| `work_dir` | Directory of the runs, created with mode `0700`. |
| `env` | Fixed variables for every script. Names starting with `MANENT_` and `TMPDIR` are set by manent. |
| `scripts.<name>.timeout` | Maximum duration of a run. Then the script and its children are stopped and the step is retried. |
| `scripts.<name>.per_target` | `false`: one run for all the targets of the batch. `true`: one run per target, up to the deploy's [parallelism](../guida/ondate.md#parallel-targets) at the same time. |
| `scripts.<name>.params` | Accepted parameters: `pattern` (a regular expression matching the whole value, required), `required`, `default`. |
| `scripts.<name>.secrets` | Logical names of the secrets passed to the script as files. They must be declared by the descriptor or by the environment configuration. |
| `scripts.<name>.outputs` | The only keys the script can write in its outputs, with their `pattern`. |
| `scripts.<name>.run` | The deploy: `command` (absolute path and arguments, run without a shell) and optional `sha256` of the file. Required. |
| `scripts.<name>.dry_run` | Checks without changes, for [dry-runs](../guida/dry-run.md). Without it a dry-run only validates the parameters. |
| `scripts.<name>.current` | Prints the release in service: needed by automatic rollback with the `plugin` locator and by the [reconciliation](../guida/governance.md#stato-reale). |
| `scripts.<name>.rollback` | Brings the targets back to the previous release. |

The keys are checked when the worker starts: an unknown key, a relative path or an invalid pattern stops it.

!!! warning "The image and the scripts"
    The `manent-server` image is distroless: it has no shell and no interpreter. Run the scripts on workers of an image of your own, with the scripts and the tools they need, and give the plugin its [own queue](../operazioni/scalabilita.md#plugin-queues), as the `server-ansible` image does for Ansible.

## Environment configuration

```yaml title="envconfig/payments/shop/shop-vip/prod.yml"
plugin: script
targets:
  - {host: bigip1.example.com, labels: {region: eu-1}}
  - {host: bigip2.example.com, labels: {region: eu-1}}
params:
  script: f5-legacy
  partition: shop-prod
secrets:
  - {name: f5-password, ref: "vault:secret/data/payments/shop/f5#password"}
```

The `script` parameter is required. Any other parameter must be declared by the script and match its pattern, otherwise the deploy is refused before it starts.

To keep the descriptor from choosing the script or its parameters, do not list them in [`descriptor_policy.params`](../operazioni/configurazione.md): then they come only from the environment configuration.

## What the script receives

The command runs without a shell, with the run directory as working directory and no standard input. Its environment holds only `PATH`, `HOME`, `TZ` and `LANG` of the worker, the catalog's `env` and these variables. The worker's other variables are not passed, because they can contain credentials.

| Variable | Content |
|---|---|
| `MANENT_ACTION` | `deploy`, `dry-run`, `current` or `rollback`. |
| `MANENT_DRY_RUN` | `true` in a dry-run. |
| `MANENT_SCRIPT` | Name of the script in the catalog. |
| `MANENT_DEPLOYMENT_ID` | ID of the deploy. Use it as an idempotency key. |
| `MANENT_TENANT`, `MANENT_APPLICATION`, `MANENT_ARTIFACT`, `MANENT_ENVIRONMENT` | The release. `MANENT_TENANT` is `untenanted` for the applications of no tenant. |
| `MANENT_VERSION`, `MANENT_ARTIFACT_TYPE` | Version and type of the artifact. |
| `MANENT_ARTIFACT_PATH`, `MANENT_ARTIFACT_SHA256` | The downloaded artifact, already checked against its sha256. Empty for `current`. |
| `MANENT_TARGETS` | JSON file with the targets of the run: `host`, `port`, `user`, `labels`. |
| `MANENT_TARGET`, `MANENT_TARGET_PORT`, `MANENT_TARGET_USER`, `MANENT_TARGET_LABEL_<NAME>` | With `per_target`: the target of the run. |
| `MANENT_PARALLELISM` | How many targets the run may update at the same time, for a script without `per_target` that handles them itself. |
| `MANENT_PARAM_<NAME>` | The parameters, defaults included. The name is in uppercase with `_` instead of other characters (`db.name` → `MANENT_PARAM_DB_NAME`). |
| `MANENT_SECRETS_DIR` | Directory with a file per secret, named after the secret. |
| `MANENT_OUTPUT` | File where the script writes its outputs. |
| `MANENT_WORK_DIR`, `TMPDIR` | Run directory and its `tmp`. |
| `MANENT_PREVIOUS_VERSION`, `MANENT_PREVIOUS_<KEY>` | For `rollback`: the release to go back to, with the details printed by `current`. |

The run directory is removed at the end of the run, secrets included. A directory left by a crash is removed by a later run after 24 hours. When the script ends, the processes it left in the background are stopped.

## Outcome and outputs

| Exit code | Outcome |
|---|---|
| `0` | Success. |
| `75` | Temporary failure: the step is retried (`max_retries` of the `deploy` step). |
| timeout, signal | Temporary failure. |
| any other | Permanent failure: no retry, and the [automatic rollback](../guida/rollback.md) starts if the policy provides for it. |

The script writes its outputs in `$MANENT_OUTPUT`, one `key=value` per line. Only the keys declared in `outputs` are accepted, with a value that matches their pattern and contains no secret: they are persisted with the deploy and can become [exports](../guida/catene.md#export-import) for other deploys. With `per_target` the outputs go in the details of each target and are not exported.

Standard output and standard error go to the deploy log line by line, with the values of the secrets replaced by `***`, up to 256 KiB per run.

!!! note "Idempotency"
    A retry or a recovery after a crash can run the script again for the same deploy, also after it changed the targets. The script must reach the same result when run twice, for example by checking whether the version is already in service.

## Rollback and release in service

The `current` command writes `version=<version>` in `$MANENT_OUTPUT`, plus the declared outputs as details. If it writes nothing, there is no release in service (first deploy). With `per_target` all the targets must have the same release.

The `rollback` command receives the release to go back to in `MANENT_PREVIOUS_VERSION`. As for every plugin, automatic rollback also needs the artifact type in [`rollback.artifact_types`](../guida/rollback.md#per-artifact-type) with `allowed: true`. A deploy that asks for rollback with a script without `rollback` or `current` fails at the checkpoint, before changing anything.

!!! note "Reconciliation and the plugin queue"
    The reconciliation runs on the worker that holds its lease, not on the plugin's queue. If that worker does not serve the plugin, it cannot run `current`, and the environment is recorded as not readable. Without a plugin queue every worker runs the scripts, and the reconciliation reads them.

## Example script

```sh title="/opt/manent/scripts/f5-deploy.sh"
#!/bin/sh
set -eu
pass=$(cat "$MANENT_SECRETS_DIR/f5-password")
for host in $(jq -r '.[].host' "$MANENT_TARGETS"); do
  # Same version already active: nothing to do (retries and recoveries).
  if f5ctl --host "$host" --partition "$MANENT_PARAM_PARTITION" active-version | grep -qx "$MANENT_VERSION"; then
    continue
  fi
  F5_PASSWORD=$pass f5ctl --host "$host" --partition "$MANENT_PARAM_PARTITION" \
    upload "$MANENT_ARTIFACT_PATH" --version "$MANENT_VERSION" || exit 75
done
first=$(jq -r '.[0].host' "$MANENT_TARGETS")
echo "vip=$(f5ctl --host "$first" --partition "$MANENT_PARAM_PARTITION" vip)" >> "$MANENT_OUTPUT"
```
