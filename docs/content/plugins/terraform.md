# terraform

The `terraform` plugin releases infrastructure: the artifact is a Terraform root module, every target of the environment is a Terraform state, and a deploy is `init`, `plan` and `apply` of that plan. Infrastructure changes then go through the same pipeline as applications: governance, approvals, freezes, audit, dry-run, inventory and [deploy chains](../guida/catene.md). [OpenTofu](https://opentofu.org) is the default binary; `terraform` works too.

It is an [external plugin](esterni.md), developed in its own repository: [manent-deployer/manent-plugin-terraform](https://github.com/manent-deployer/manent-plugin-terraform). The `manent-server` image contains both the plugin binary and `tofu`.

## How a deploy runs

For every target, one after the other:

1. `init -input=false -lockfile=readonly`, with `-backend-config` from the `backend.*` parameters;
2. `workspace select -or-create`, when `workspace` is set;
3. `plan -detailed-exitcode -out=<file>` and `show -json` of the plan: the step reports how many resources are added, changed and destroyed, and their addresses, never their values;
4. if the plan destroys more resources than `max_destroy` (default 0), the deploy fails **without retries**, before anything is applied;
5. `apply` of that exact plan file, only if it has changes;
6. `output -json`: the non-sensitive outputs become plugin outputs.

A [dry-run](../guida/dry-run.md) stops after the check of step 4: whoever approves the deploy sees the planned changes in the `deploy` step of the dry-run. Running the same version again plans no changes and applies nothing, so DBOS retries and recoveries are safe.

## The artifact

A `.tgz` with the root module and its `.terraform.lock.hcl`: providers are installed only with the versions and checksums the lock file pins, and an artifact without it is refused (`require_lock_file: false` allows it).

```sh
tofu init -backend=false
tofu providers lock -platform=linux_amd64 -platform=linux_arm64
tar -czf network-1.4.0.tgz --exclude=.terraform *.tf .terraform.lock.hcl
```

```yaml title="deploy.yml"
apiVersion: manent/v1
application: infra
environment: prod
artifact:
  type: terraform
  name: network
  version: 1.4.0
  source: https://nexus.example.com/repository/infra/network-1.4.0.tgz
```

The module can sit at the root of the archive, in its only top-level directory, or in the directory named by `module_dir`.

## Configuration

```yaml title="manent.yml"
plugins:
  terraform:
    external:
      command: /usr/local/bin/manent-plugin-terraform
      sha256: "<sha256 of the binary>"
    artifact_types: [terraform]               # default
    binary: tofu                              # default; or terraform, or an absolute path
    plugin_cache_dir: /var/cache/manent/tf    # optional provider cache shared by the runs
    provider_mirror: https://mirror.example.com/providers/   # optional, for isolated environments
```

The state never passes through manent: every target uses a remote backend with locking (S3, `azurerm`, `gcs`, `pg`, `consul`, `http`), encrypted and readable only by the workers' identities. Credentials of the backend and of the providers arrive from secrets, as environment variables of the process.

```yaml title="Environment configuration: infra/network/prod"
plugin: terraform
targets:                                       # one target = one state
  - {host: prod-eu, labels: {region: eu-1}}
  - {host: prod-us, labels: {region: us-1}}
params:
  workspace: "{host}"
  backend.bucket: acme-tfstate
  backend.key: network/{environment}/{host}.tfstate
  backend.region: eu-west-1
  var.cidr: 10.20.0.0/16
  secret_var.db_password: db-password          # TF_VAR_db_password from a secret
  env.AWS_ACCESS_KEY_ID: aws-id                # name of a secret
  env.AWS_SECRET_ACCESS_KEY: aws-secret
  max_destroy: "0"
secrets:
  - {name: aws-id, ref: "vault:secret/data/infra/aws#id"}
  - {name: aws-secret, ref: "vault:secret/data/infra/aws#secret"}
  - {name: db-password, ref: "vault:secret/data/infra/db#password"}
exports:
  vms: {from: deploy.vm_hosts, format: targets}
```

| Parameter | Description |
|---|---|
| `workspace` | Workspace of each target, created if missing. Empty: the default workspace. |
| `backend.<key>` | Backend setting passed as `-backend-config=<key>=<value>`. |
| `var.<name>` | Variable passed as `TF_VAR_<name>`. |
| `secret_var.<name>` | Variable passed as `TF_VAR_<name>`, with the value of the named secret. |
| `env.<NAME>` | Environment variable with the value of the named secret (provider and backend credentials). |
| `max_destroy` | Maximum number of resources a plan may destroy (default `0`). A replacement counts as a destroy. |
| `module_dir` | Directory of the root module inside the artifact. |
| `parallelism` | `-parallelism` of `plan` and `apply` (1-256). |
| `timeout` | Maximum duration of the run of one target (default `1h`). |
| `require_lock_file` | `false` allows a module without `.terraform.lock.hcl` (default `true`). |

`workspace`, `backend.*` and `var.*` accept the placeholders `{host}`, `{application}`, `{artifact}`, `{environment}` and `{tenant}`. Two targets that would share the same state (same workspace and backend settings) are refused at validation. Unknown parameters are refused too.

!!! tip "What descriptors may set"
    The plugin runs code of the artifact with powerful credentials. Keep `backend.*` and `env.*` in the environment configuration and allow descriptors at most `var.*`, with [`descriptor_policy.params`](../operazioni/sicurezza.md#cosa-puo-chiedere-un-descrittore).

## Isolation

- Every run has its own working directory, `HOME`, `TF_DATA_DIR` and CLI configuration file, removed at the end: nothing comes from the worker's home or from a host CLI configuration.
- The process gets only `PATH`, `TZ` and `LANG` from the worker, plus the variables the plugin sets.
- Secrets are resolved at every run and passed only in the environment of the process; they are redacted from error messages and never appear in step outputs. Sensitive outputs are never exported.

## Outputs and chains

With one target the plugin outputs are the Terraform output names; with several, each output is `<host>.<name>`, plus `<name>` when every target has the same value. Lists and objects become compact JSON, so a list of hosts can be exported with `format: targets` and become the targets of the next deploy:

```yaml title="Environment configuration of the application that runs on those VMs"
imports:
  infra: {current: {application: infra, artifact: network}}
targets_from: [{import: infra, export: vms}]
```

See [Exports and imports](../guida/catene.md#export-import) for the whole chain.

## Waves, rollback and limits

- **Wave rollouts:** targets with `region` and `az` labels work with [wave rollouts](../guida/ondate.md): each stage receives only its states, with bake time, health gate and approvals in between.
- **Rollback:** manual only. `manent rollback` applies the previously released version of the module. Leave the `terraform` type out of `rollback.artifact_types`: a failed apply leaves the state as Terraform left it, and recreating infrastructure automatically can destroy data.
- **Not supported:** `destroy` as a manent operation. Applying exactly the plan approved in a dry-run, and drift detection in the [reconciliation](../guida/governance.md#stato-reale), are planned.
