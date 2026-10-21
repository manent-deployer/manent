# HashiCorp Vault

manent reads secrets from Vault's KV engine, version 1 or 2, through the HTTP API: SSH keys, cloud credentials, chat tokens, ITSM passwords, the audit HMAC key. Descriptors, environment configurations and `manent.yml` contain only references like `vault:secret/data/shop/prod#ssh_key`; the worker resolves them at the moment of use and never stores the value.

!!! abstract "What you need"
    A Vault server (Community, Enterprise or HCP) · permission to create a policy and an auth role · the KV engine mounted, `secret/` (v2) in the examples

## 1. Organize the paths

A layout per application and environment lets the Vault policy and manent's `descriptor_policy` work together:

```text
secret/manent/…                    # manent's own credentials: slack, servicenow, nexus, audit
secret/<application>/<environment> # one secret per environment, one field per credential
```

```sh
vault kv put secret/shop/prod ssh_key=@deploy_key db_password='…'
vault kv put secret/manent/slack bot_token='xoxb-…' signing_secret='…'
```

## 2. A read-only policy for manent

```hcl title="manent.hcl"
# KV v2: the API path includes data/
path "secret/data/shop/*"   { capabilities = ["read"] }
path "secret/data/manent/*" { capabilities = ["read"] }
```

```sh
vault policy write manent manent.hcl
```

Grant `read` only on what manent must read. For many applications use a glob such as `secret/data/+/prod` or one policy per team; manent never needs `list`, `create` or `update`.

## 3. Choose how manent authenticates

=== "Kubernetes (manent in a cluster)"

    ```sh
    vault auth enable kubernetes
    vault write auth/kubernetes/config kubernetes_host=https://kubernetes.default.svc
    vault write auth/kubernetes/role/manent \
      bound_service_account_names=manent bound_service_account_namespaces=manent \
      token_policies=manent token_ttl=1h
    ```

    ```yaml title="manent.yml"
    secrets:
      vault:
        enabled: true
        address: https://vault.example.com:8200
        auth: kubernetes
        kubernetes:
          role: manent
          # jwt_file: /var/run/secrets/kubernetes.io/serviceaccount/token   # default
          # mount: kubernetes                                                # default
    ```

    The Helm chart creates the `manent` service account the role is bound to.

=== "AppRole (VMs)"

    ```sh
    vault auth enable approle
    vault write auth/approle/role/manent token_policies=manent token_ttl=1h secret_id_ttl=720h
    vault read -field=role_id auth/approle/role/manent/role-id
    vault write -f -field=secret_id auth/approle/role/manent/secret-id > /etc/manent/vault-secret-id
    chmod 600 /etc/manent/vault-secret-id; chown manent /etc/manent/vault-secret-id
    ```

    ```yaml title="manent.yml"
    secrets:
      vault:
        enabled: true
        address: https://vault.example.com:8200
        auth: approle
        approle:
          role_id: <role_id>
          secret_id_file: /etc/manent/vault-secret-id
          # mount: approle      # default
    ```

=== "Token (Vault Agent)"

    ```yaml title="manent.yml"
    secrets:
      vault:
        enabled: true
        address: https://vault.example.com:8200
        auth: token
        token_file: /run/vault/token   # reread at every use; or the VAULT_TOKEN variable
    ```

    Let Vault Agent (or another sidecar) log in and keep the token file fresh: manent rereads it every time.

With `approle` and `kubernetes` manent logs in, keeps the token in memory and renews it at 80% of its lease; if Vault rejects it (revocation), it logs in once more. Other options: `namespace` (Vault Enterprise and HCP), `ca_file` for a private CA, `timeout` (default 10s).

## 4. Use the references

```yaml title="envconfig/shop/shop/prod.yml"
secrets:
  - name: deploy-key
    ref: "vault:secret/data/shop/prod#ssh_key"     # KV v2: data/ in the path
  # - name: legacy
  #   ref: "vault:kv/shop/prod#password"            # KV v1: no data/
params:
  ssh_key_secret: deploy-key
```

The same syntax works in `manent.yml` (`bot_token_ref`, `password_ref`, `secret_ref`, `hmac_key_ref`…). manent recognizes a KV v2 answer and reads the field from `data.data`; the field must be a string (an SSH key or a certificate is fine as a multi-line string).

Limit what descriptors can reference, so that one application cannot ask for another's secrets:

```yaml title="manent.yml"
descriptor_policy:
  secret_refs: ["vault:secret/data/{application}/*"]
```

## 5. Check it

```sh
manent deploy -f deploy/prod.yml --dry-run
```

The `secrets` step resolves every declared secret and reports how many (`count`), never the values. A missing field, a denied path or a failed login fails the step with Vault's status and message, for example a `403` with `permission denied`.

## Rotation

Values are read at every use, so rotating a secret in Vault takes effect from the next read, even within a running deploy, with no restart and no change to references. To rotate the AppRole `secret_id`, write a new one to `secret_id_file`: it is read at the next login.

Reference: [Secrets and artifact sources](../operazioni/segreti-artefatti.md#hashicorp-vault).
