# HashiCorp Consul

The [environment configuration](../guida/configurazione-ambiente.md) of each application and environment (plugin, targets, parameters, secret references, rollout strategy) can live in Consul's KV store. manent reads it at the start of every deploy and records which revision it used, so each deploy can be traced back to the exact configuration that produced it.

!!! abstract "What you need"
    A Consul cluster with ACLs enabled · permission to create policies and tokens · the `consul` CLI

## 1. Key layout

| Key | Content |
|---|---|
| `manent/<application>/<environment>` | Environment configuration, YAML or JSON. |

The template can be changed (`envconfig.consul.key`).

!!! danger "References, never values"
    The configuration manent reads is saved in the deploy history. It must contain only secret references (`vault:…`, `file:…`), never passwords or keys.

## 2. ACLs: manent reads, owners write

```hcl title="manent-read.hcl"
key_prefix "manent/" {
  policy = "read"
}
```

```sh
consul acl policy create -name manent-read -rules @manent-read.hcl
consul acl token create -description "manent workers and API" -policy-name manent-read
```

Writers get their own, narrower policies, for example one per application owner:

```hcl title="shop-owners.hcl"
key_prefix "manent/shop/" {
  policy = "write"
}
```

## 3. Configure manent

```yaml title="manent.yml"
envconfig:
  provider: consul
  required: true               # an environment without configuration fails instead of using only the descriptor
  max_retries: 3               # extra attempts if Consul does not answer
  consul:
    address: https://consul.example.com:8501    # default CONSUL_HTTP_ADDR or http://127.0.0.1:8500
    datacenter: ""
    key: "manent/{application}/{artifact}/{environment}"
    token_file: /run/secrets/consul-token        # or CONSUL_HTTP_TOKEN
```

The ACL token is never written in `manent.yml`: it comes from `CONSUL_HTTP_TOKEN` or `token_file`. Both the API (to check submissions) and the workers (to run deploys) need it.

## 4. Write a configuration

```sh
consul kv put manent/shop/shop/prod @envconfig/shop/shop/prod.yml
consul kv get -detailed manent/shop/shop/prod | grep ModifyIndex
```

```sh
manent validate -f deploy/prod.yml | jq .config
```

```json
{"source": "consul:manent/shop/shop/prod", "revision": "4127", "targets": "6"}
```

`revision` is the key's `ModifyIndex`. The output of the `config` step of every deploy carries the same pair, so "which configuration did deploy X use?" has a precise answer.

## 5. Configuration as code

Keep the configuration in a repository and let a pipeline write it, after manent has checked it:

1. **Merge request:** `manent env validate -a shop -e prod -f envconfig/shop/shop/prod.yml` runs every submission check (merge with the deploy in service, descriptor policy, rollout plan, plugins) and writes nothing. See [GitHub Actions](github-actions.md) or [GitLab CI](gitlab-ci.md).
2. **Merge:** the pipeline runs `consul kv put`, or Terraform with `consul_keys` gated by the [`manent_environment_check`](terraform.md) data source.

## When it is read

- **At submission**, to reject at once a conflict, a forbidden parameter or an environment without targets (`422`). If Consul does not answer, the check is left to the worker.
- **At the start of the deploy**, in the `config` step, with `max_retries`. A scheduled deploy therefore uses the configuration valid when it runs. Once read, the configuration is saved with the deploy: a deploy resumed after a crash reuses it even if the key has changed meanwhile.

Reference: [Environment configuration](../guida/configurazione-ambiente.md).
