# Security

## Production checklist

The default configuration is meant for development. At startup, `serve` and `worker` log a warning (`configurazione non adatta alla produzione`) for every setting that is still permissive. In production:

- [ ] `auth.mode: enabled`, with OIDC for users and static tokens for CI, the latter in `auth.tokens_file` and with `expires_at`. See [Authentication, permissions and audit](accesso.md).
- [ ] Users and CI use the API or the `manent` client; `manent-server` and the database credentials only for server processes and platform operators.
- [ ] `rbac.bindings` with the minimum needed: for example `deployer` on non-production environments, `tester` (dry-run only) in production, `operator` for release managers.
- [ ] `audit.file` with `hmac_key_ref` and `max_size_mb`, checked periodically with `manent-server audit verify`, in addition to keeping the entries in the SIEM.
- [ ] `governance` with mandatory promotion and approvals for production, and the company freeze windows. The `approver` role for a group different from those who release; `deployment.freeze-override` only for those who handle emergencies. See [Release governance](../guida/governance.md).
- [ ] `descriptor_policy` set: no commands from the descriptor, limited secret references, targets from the environment configuration.
- [ ] Secrets in Vault (`secrets.vault`), not in the worker's environment variables. See [Secrets and artifact sources](segreti-artefatti.md).
- [ ] `artifacts.allow_unlisted: false`, with the sources and their credentials in `artifacts.sources`.
- [ ] `callbacks.allowed_hosts` set.
- [ ] With [custom steps](../guida/step-personalizzati.md): `pipeline.custom.spec_key_refs` set, a dedicated SSH user (`ssh.user`) with minimal privileges, read-only secrets for `dry_run`, controls declared `mandatory`, and no `insecure_ignore_host_key`.
- [ ] `sha256` for every external plugin, binaries in a directory not writable by the worker.
- [ ] TLS terminated in front of `serve` (reverse proxy or load balancer).
- [ ] Access to the DBOS database only for workers, `serve` and platform operators.
- [ ] `/metrics`, `/healthz` and `/readyz` require no authentication: `observability.listen` reachable only from the monitoring network and, in front of the API, `/metrics` not exposed externally.
- [ ] `retention.max_age` consistent with retention requirements, and the audit kept separately (file and SIEM).

## Secrets

The principle is that **a secret value is never persisted**. DBOS saves in the database the descriptor, the plan, the environment configuration that was read and the output of every step, so none of these may contain secrets.

- **Descriptor and environment configuration:** contain only references (`env:`, `file:`, `vault:`) associated with a logical name.
- **`secrets` step:** checks that the references can be resolved but does not return the values.
- **Plugins:** read a secret by logical name, through `req.Secrets`, only at the moment it is needed.
- **Callbacks and artifact sources:** authentication refers to a secret, read only at send or download time.
- **Consul:** the token is read from `CONSUL_HTTP_TOKEN` or `token_file`, never from the configuration file.
- **API tokens:** the configuration holds only their sha256.

## What a descriptor can ask for { #cosa-puo-chiedere-un-descrittore }

Whoever can send a descriptor is not necessarily whoever administers the environments. The environment configuration is authoritative, but what it does not define can be added by the descriptor. Without limits, a descriptor could:

- set `post_command` and have arbitrary commands run on the targets, with the environment's credentials;
- declare an `env:` reference to a worker variable (for example `env:CONSUL_HTTP_TOKEN`) and have a plugin use it;
- name its own targets and receive the artifact with the environment's credentials.

`descriptor_policy` decides what the descriptor can add:

```yaml title="manent.yml"
descriptor_policy:
  params:                       # per plugin; "*" applies to all plugins
    remote-copy: [dest_dir, verify_checksum, keep_releases]
  secret_refs:                  # patterns with {application} and {environment}
    - "vault:secret/data/{application}/*"
  targets: false
```

| Key | Default | Description |
|---|---|---|
| `params` | `{"*": ["*"]}` | Patterns of the `deploy.params` parameters the descriptor may set, per plugin. The entries of the selected plugin (`deploy.plugin`, from the environment or the descriptor) and those of `"*"` apply. |
| `secret_refs` | `["*"]` | Patterns of the references the descriptor may declare in `secrets`. |
| `targets` | `true` | Allows `deploy.targets` in the descriptor when the environment does not define them. |

What comes from the environment configuration is not subject to the policy: commands, secrets and targets stay under the control of whoever administers it. If the section is present, omitted fields keep the default; in production set all three.

The policy is checked at enqueue time (`422`), again in the `config` step together with the merge with the environment configuration, and by `manent validate`. In the `config` step a violation makes the deploy fail immediately, without the step's retries.

## Callbacks

Callback URLs are chosen by whoever requests the deploy. To prevent a descriptor from making the worker call internal services (SSRF):

- by default only `https` URLs are allowed (`callbacks.allow_http`);
- `callbacks.allowed_hosts` limits the reachable hosts;
- redirects are not followed;
- a URL cannot contain credentials.

See [Callbacks](../guida/callback.md#limiti-sugli-url).

## External plugins

An external plugin is a binary run by the worker with its permissions. manent applies these protections:

| Protection | How |
|---|---|
| Binary integrity | With `external.sha256`, the binary is verified at activation and at every process start. |
| Isolated environment | The process does not inherit the worker's variables, except `PATH`, `HOME`, `TMPDIR`, `TZ` and `LANG`. Others must be passed explicitly with `external.env`. |
| Secrets on demand | Secrets arrive only through the gRPC secrets channel, active for the duration of a single `Deploy`, `DryRun`, `CurrentRelease` or `Rollback` call. |
| Encrypted channel | Communication with the process uses mTLS with certificates generated at every start. |

!!! warning
    Without `sha256` manent logs a warning: whoever can replace the binary can run code with the worker's permissions. In production always set the hash and install the binaries in a directory not writable by the worker's user.

## SSH

The `remote-copy` plugin verifies the targets' host key with `known_hosts`. `insecure_ignore_host_key: true` disables the check and is meant for development only.

## API

With `auth.mode: enabled` the API requires a bearer token (OIDC or static) and enforces permissions. manent does not terminate TLS: expose it behind a reverse proxy or load balancer that does. With `auth.mode: none` the API has no authentication: use it only locally.

## Database

The DBOS system database contains the deploy history, the queue, the deploy logs and the worker registry. Whoever has write access to it can enqueue, cancel or modify workflows without going through manent's permissions. It must therefore be treated as an administrative component: credentials only for workers, `serve` and platform operators, encrypted connection (`sslmode=verify-full`).
