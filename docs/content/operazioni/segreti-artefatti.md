# Secrets and artifact sources

## Secret providers

Descriptor, environment configuration and manent configuration contain only **references** to secrets, in the form `<scheme>:<reference>`. The value is read by the worker only when needed and is never saved.

| Scheme | Example | Value |
|---|---|---|
| `env:` | `env:DEPLOY_SSH_KEY` | Worker environment variable. |
| `file:` | `file:/run/secrets/db` | Contents of a file on the worker. |
| `vault:` | `vault:secret/data/myapp/prod#ssh_key` | Field of a HashiCorp Vault secret. |

### HashiCorp Vault

```yaml title="manent.yml"
secrets:
  vault:
    enabled: true
    address: https://vault.example.com:8200   # default VAULT_ADDR
    namespace: ""                            # Vault Enterprise / HCP
    auth: approle
    approle:
      role_id: manent
      secret_id_file: /etc/manent/vault-secret-id
    ca_file: /etc/ssl/certs/corp-ca.pem
    timeout: 10s
```

The reference is `vault:<API path>#<field>`:

- **KV v2:** the path includes `data/`, as in the Vault API: `vault:secret/data/myapp/prod#ssh_key`;
- **KV v1:** `vault:kv/myapp/prod#password`.

manent recognizes the KV v2 response and reads the field from `data.data`. The field must be a string.

| `auth` | Credentials |
|---|---|
| `token` (default) | `token_file`, reread at every use (for example refreshed by Vault Agent), or the `VAULT_TOKEN` variable. |
| `approle` | `approle.role_id` and `approle.secret_id_file`. `approle.mount` default `approle`. |
| `kubernetes` | `kubernetes.role` and the service account token (`kubernetes.jwt_file`, default `/var/run/secrets/kubernetes.io/serviceaccount/token`). `kubernetes.mount` default `kubernetes`. |

With `approle` and `kubernetes` manent logs in, keeps the token in memory and replaces it at 80% of its lifetime. If Vault rejects the token (revocation), it logs in again once. Errors report Vault's status and messages, never the secret values.

The Vault policy of manent's role must grant `read` only on the paths needed. Combined with `descriptor_policy.secret_refs` it limits what each application can read:

```yaml
descriptor_policy:
  secret_refs: ["vault:secret/data/{application}/*"]
```

## Artifact sources

The worker downloads the artifact from `artifact.source`: a local path, `file://` or `http(s)://`. The `artifacts` section decides which sources are allowed and with which credentials.

```yaml title="manent.yml"
artifacts:
  timeout: 30m
  allow_unlisted: false
  sources:
    - prefix: https://nexus.example.com/repository/releases/
      auth:
        type: basic
        username: manent
        secret_ref: vault:secret/data/manent/nexus#password
    - prefix: https://artifactory.example.com/artifactory/
      auth:
        type: header
        header: X-JFrog-Art-Api
        secret_ref: vault:secret/data/manent/artifactory#api_key
      ca_file: /etc/ssl/certs/corp-ca.pem
    - prefix: file:///srv/artifacts/
```

| Key | Description |
|---|---|
| `timeout` | Maximum duration of a download (default `30m`). |
| `allow_unlisted` | `true` (default): sources not listed are allowed too, without credentials. `false`: only those matching a `prefix`. |
| `sources[].prefix` | URL prefix: `https://`, `http://` or `file:///`. If several prefixes match, the longest wins. |
| `sources[].auth.type` | `basic` (with `username`), `bearer`, `header` (with `header`). |
| `sources[].auth.secret_ref` | Reference of the secret (password, token or API key), read at every download. |
| `sources[].ca_file` | Additional CAs for the source. |

Credentials live in manent's configuration, not in the descriptor: whoever requests a deploy only gives the artifact URL.

- **Local paths.** A path is compared as `file://` plus the cleaned absolute path, so `../` cannot escape an allowed prefix.
- **When it is checked.** The source is checked at enqueue time (`422` if not allowed) and again by the worker on download.
- **Redirects.** If the source redirects to another host, credentials are not forwarded.

!!! warning "Why limit the sources"
    With `allow_unlisted: true` a descriptor can make the worker download any URL reachable from its network, or copy to the targets a local file of the worker, for example `/etc/passwd`. In production set `allow_unlisted: false`.
