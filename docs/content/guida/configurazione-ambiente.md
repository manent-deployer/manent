# Environment configuration

Plugin, targets, parameters and secret references of each application in each environment can be managed in a configuration system instead of the descriptor. Whoever requests the deploy only states application, environment and artifact:

```yaml title="Minimal descriptor"
apiVersion: manent/v1
application: myapp
environment: test
artifact:
  type: application
  name: myapp
  version: 1.2.3
  source: https://artifacts.example.com/myapp/myapp-1.2.3.tar.gz
```

## Format

An environment's configuration is a YAML or JSON document:

```yaml title="manent/untenanted/myapp/myapp/test"
plugin: remote-copy
targets:
  - host: 192.168.1.10
    port: 22
    user: deploy
params:
  dest_dir: /opt/myapp/releases
  ssh_key_secret: deploy-key
  post_command: "systemctl restart myapp"
secrets:
  - name: deploy-key
    ref: env:DEPLOY_SSH_KEY
rollback:                 # optional: see Rollback
  enabled: true
```

!!! danger "Secret references only"
    The configuration that is read is saved in the deploy history. It must contain only **references** to secrets (`env:…`, `file:…`), never the values.

## Providers

Configured in `manent.yml`, section `envconfig`: each entry of `providers` has a `name`, a `type` and the section of the type. With one provider, call it as you like (`default` below); for several see [Several providers](#piu-provider).

=== "Consul"

    ```yaml
    envconfig:
      required: true
      max_retries: 3
      providers:
        - name: default
          type: consul
          consul:
            address: http://consul:8500            # default: CONSUL_HTTP_ADDR or http://127.0.0.1:8500
            datacenter: ""
            key: "manent/{tenant}/{application}/{artifact}/{environment}"
            # token_file: /run/secrets/consul-token
    ```

    The configuration is the value of the KV key. The ACL token is read from `CONSUL_HTTP_TOKEN` or from `token_file`, never from the configuration file.

    ```sh
    consul kv put manent/untenanted/myapp/myapp/test @examples/envconfig/untenanted/myapp/myapp/test.yml
    ```

=== "Git"

    ```yaml
    envconfig:
      providers:
        - name: default
          type: git
          git:
            repository: https://git.example.com/platform/envconfig.git
            ref: refs/heads/main
            path: "{tenant}/{application}/{artifact}/{environment}.yml"
            password_ref: vault:secret/manent/git#token
    ```

    The file is read at a commit: its SHA is recorded on the deploy, and a descriptor can choose a revision within `descriptor_policy.config_ref`. See [Git repository](../integrazioni/git.md).

=== "File"

    ```yaml
    envconfig:
      providers:
        - name: default
          type: file
          file:
            path: "./envconfig/{tenant}/{application}/{artifact}/{environment}.yml"
    ```

    Useful for development and testing.

=== "None"

    ```yaml
    envconfig:
      providers: []
    ```

    Everything comes from the descriptor.

| Option | Description |
|---|---|
| `required` | `true`: the deploy fails if there is no configuration for the environment. `false`: only the descriptor is used. |
| `max_retries` | Additional attempts if the provider does not respond (e.g. Consul unreachable). |

Templates (`consul.key`, `file.path`, `git.path` of each provider) contain `{tenant}` (the [tenant](tenant.md) of the release, `untenanted` for the applications no tenant lists) and `{artifact}`, the descriptor's `artifact.name`: one document per artifact of an application of a tenant. See [Applications with several artifacts](artefatti.md).

## Several providers { #piu-provider }

One installation can read from several systems at once: for example the payments team from its own Git repository, the applications not migrated yet from the old Consul, everyone else from the default Consul. Every system is an entry of `envconfig.providers`:

```yaml title="manent.yml"
envconfig:
  required: true
  providers:
    - name: payments-git
      type: git
      git:
        repository: https://git.example.com/payments/envconfig.git
        password_ref: vault:secret/manent/payments-git#token
      match: {applications: ["payments/*"]}
    - name: legacy-consul
      type: consul
      consul:
        address: https://consul-old.example.com
        key: "manent/{application}/{environment}"
        token_file: /etc/manent/consul-old.token
      match: {environments: [dev, test]}
      on_missing: next                     # not migrated yet? read the next one
    - name: retail-git
      type: git
      git: {repository: https://git.example.com/retail/envconfig.git}
      tenant_only: true
    - name: default
      type: consul
```

```yaml title="tenants.yml"
tenants:
  retail:
    applications: {shop: {}}
    envconfig: retail-git                  # read first, whatever the match rules say
```

- **Which provider.** A deploy reads from the provider its tenant names in the registry (`envconfig`), then from the providers whose `match` applies, in order. `match` takes patterns on `applications` (qualified with the tenant, as in permissions and rules: `payments/*`), `artifacts` and `environments`; a provider without `match` serves everything, one with `tenant_only: true` only the tenants that name it. The first one that has the document wins; with no candidate the deploy has no environment configuration (an error with `required: true`).
- **A missing document.** With `on_missing: fail` (default) the deploy stops at that provider, as with a single one. With `on_missing: next` it moves to the next candidate: useful to migrate one application at a time. Any other error, such as an unreachable system or an invalid document, never moves on: the deploy retries, and never silently reads another configuration.
- **Own settings.** Each provider has its template, address and credentials. `CONSUL_HTTP_TOKEN` and `CONSUL_HTTP_ADDR` are only the defaults of the Consul providers without `token_file` and `address`.
- **Traceability.** The `config` step records `provider` (and `skipped`, the providers passed over with `on_missing: next`) next to `source` and `revision`; with Git the deploy also records `config_provider`, shown in the status next to `config_revision`.
- **Revisions.** `config.ref` requires that the provider the deploy reads from is Git. A manual rollback and the reconciliation read the recorded revision from the provider that recorded it, even if the rules now choose another one: a commit SHA belongs to one repository. If that provider is no longer configured, the deploy fails with a permanent error.
- **Unknown names.** A tenant naming a provider that does not exist is logged at startup, and its deploys fail.
- **Earlier versions.** `envconfig.provider` with the `consul`, `git` or `file` section next to it is no longer accepted: manent does not start and the error shows the `providers` form. Move the section into an entry, e.g. `providers: [{name: default, type: consul, consul: {...}}]`.

## When it is read

The configuration is read at the start of every deploy, in the `config` step. Two consequences follow:

- a scheduled deploy uses the configuration valid at run time, not the one at the time it was scheduled;
- once read, it is saved by DBOS. If the deploy is resumed after a crash, it reuses the same configuration even if it has changed in the meantime.

The output of the `config` step reports the source (`source`, e.g. `consul:manent/untenanted/myapp/myapp/test`) and the revision (`revision`: Consul's `ModifyIndex`, or the commit SHA with Git), so every deploy can be traced back to the exact configuration it used.

## Merge rules { #regole-di-fusione }

The environment configuration is **authoritative**: the descriptor can only add what is not defined there.

| Field | Rule |
|---|---|
| `policy`, `plugin`, `parallel` | If defined in both they must match. `parallel` is `deploy.parallel`: the targets of a batch updated at the same time ([parallel targets](ondate.md#parallel-targets)); a value set here is not subject to `descriptor_policy.max_parallel`. |
| `targets` | If the configuration defines them, the descriptor can omit them or repeat them identically, labels included. |
| `params` | The descriptor can add keys; a key present in both must have the same value. |
| `secrets` | The descriptor can add secrets; the same name must have the same reference. |
| `step_params` | Parameters of the [custom steps](step-personalizzati.md), per step: the descriptor (`stepParams`) can add keys allowed by `descriptor_policy.step_params`; a key present in both must have the same value. |
| `rollout` | The environment's [wave rollout](ondate.md) strategy is authoritative: the descriptor can omit it or repeat it identically. |
| `traffic` | Load balancer and router of the environment ([draining](svuotamento.md), [percentage rollouts](traffico.md)): only in the environment configuration, a descriptor that defines it is rejected. |
| `imports`, `targets_from`, `params_from` | Targets and plugin parameters taken from the exports of the release in service of another application (only `current` imports), resolved when every deploy starts: see [Exports and imports](catene.md#export-import). With `targets_from` the descriptor cannot add its own targets; a parameter in `params_from` cannot also be set by the descriptor. |
| `exports` | Outputs of the environment's deploys that other deploys can import; the descriptor can add others, the same name must have the same definition. |
| `rollback` | `enabled`, `on` and `locator` follow the same rule: if defined in both they must match. This way an environment can enforce [rollback](rollback.md) without the descriptor being able to disable it. The artifact type's `allowed` limit still applies. |

A conflicting value makes the deploy fail: it is never silently overwritten. After the merge (and the imports) there must be at least one target.

On top of these rules, `descriptor_policy` in `manent.yml` limits what the descriptor can add: for example it can forbid commands (`post_command`), secret references outside the application's scope and targets not defined by the environment. See [Security](../operazioni/sicurezza.md#cosa-puo-chiedere-un-descrittore).

## Checking the effective deploy

```sh
manent validate -f examples/descriptor-envconfig.yml
```

The server shows the plan, the source and revision of the configuration (`config`), the effective `deploy` section and the names of the secrets. The `deployment.read` permission on the application and environment is required.

To try a manent configuration before installing it, without a running server, there is `manent-server -c manent.yml validate -f <descriptor>`, which reads the environment configuration locally (`--offline` to skip it). See [manent-server](../operazioni/server.md#validate).
