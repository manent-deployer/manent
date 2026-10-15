# Environment configuration

Plugin, policy, targets, parameters and secret references of each application in each environment can be managed in a configuration system instead of the descriptor. Whoever requests the deploy only states application, environment and artifact:

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

```yaml title="manent/myapp/test"
plugin: remote-copy
policy: default
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

Configured in `manent.yml`, section `envconfig`:

=== "Consul"

    ```yaml
    envconfig:
      provider: consul
      required: true
      max_retries: 3
      consul:
        address: http://consul:8500            # default: CONSUL_HTTP_ADDR or http://127.0.0.1:8500
        datacenter: ""
        key: "manent/{application}/{environment}"
        # token_file: /run/secrets/consul-token
    ```

    The configuration is the value of the KV key. The ACL token is read from `CONSUL_HTTP_TOKEN` or from `token_file`, never from the configuration file.

    ```sh
    consul kv put manent/myapp/test @examples/envconfig/myapp/test.yml
    ```

=== "File"

    ```yaml
    envconfig:
      provider: file
      file:
        path: "./envconfig/{application}/{environment}.yml"
    ```

    Useful for development and testing.

=== "None"

    ```yaml
    envconfig:
      provider: none
    ```

    Everything comes from the descriptor.

| Option | Description |
|---|---|
| `required` | `true`: the deploy fails if there is no configuration for the environment. `false`: only the descriptor is used. |
| `max_retries` | Additional attempts if the provider does not respond (e.g. Consul unreachable). |

## When it is read

The configuration is read at the start of every deploy, in the `config` step. Two consequences follow:

- a scheduled deploy uses the configuration valid at run time, not the one at the time it was scheduled;
- once read, it is saved by DBOS. If the deploy is resumed after a crash, it reuses the same configuration even if it has changed in the meantime.

The output of the `config` step reports the source (`source`, e.g. `consul:manent/myapp/test`) and the revision (`revision`, Consul's `ModifyIndex`), so every deploy can be traced back to the exact configuration it used.

## Merge rules

The environment configuration is **authoritative**: the descriptor can only add what is not defined there.

| Field | Rule |
|---|---|
| `policy`, `plugin` | If defined in both they must match. |
| `targets` | If the configuration defines them, the descriptor can omit them or repeat them identically, labels included. |
| `params` | The descriptor can add keys; a key present in both must have the same value. |
| `secrets` | The descriptor can add secrets; the same name must have the same reference. |
| `step_params` | Parameters of the [custom steps](step-personalizzati.md), per step: the descriptor (`stepParams`) can add keys allowed by `descriptor_policy.step_params`; a key present in both must have the same value. |
| `rollout` | The environment's [wave rollout](ondate.md) strategy is authoritative: the descriptor can omit it or repeat it identically. |
| `traffic` | Load balancer and router of the environment ([draining](svuotamento.md), [percentage rollouts](traffico.md)): only in the environment configuration, a descriptor that defines it is rejected. |
| `rollback` | `enabled`, `on` and `locator` follow the same rule: if defined in both they must match. This way an environment can enforce [rollback](rollback.md) without the descriptor being able to disable it. The artifact type's `allowed` limit still applies. |

A conflicting value makes the deploy fail: it is never silently overwritten. After the merge there must be at least one target.

On top of these rules, `descriptor_policy` in `manent.yml` limits what the descriptor can add: for example it can forbid commands (`post_command`), secret references outside the application's scope and targets not defined by the environment. See [Security](../operazioni/sicurezza.md#cosa-puo-chiedere-un-descrittore).

## Checking the effective deploy

```sh
manent validate -f examples/descriptor-envconfig.yml
```

The server shows the plan, the source and revision of the configuration (`config`), the effective `deploy` section and the names of the secrets. The `deployment.read` permission on the application and environment is required.

To try a manent configuration before installing it, without a running server, there is `manent-server -c manent.yml validate -f <descriptor>`, which reads the environment configuration locally (`--offline` to skip it). See [manent-server](../operazioni/server.md#validate).
