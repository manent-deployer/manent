# Git repository

The [environment configuration](../guida/configurazione-ambiente.md) can live in a Git repository. manent reads each file at a commit and records its SHA on the deploy. A release can then be rebuilt exactly from the artifact `sha256` plus `repository@sha:path`. Pull requests, CODEOWNERS and signed commits govern every change to the configuration.

!!! abstract "What you need"
    A Git server reachable from API and workers (HTTPS, SSH or `git://`) · a read-only token or deploy key · optionally the keys that sign the commits

## 1. Repository layout

| Path | Content |
|---|---|
| `<application>/<artifact>/<environment>.yml` | Environment configuration, same format as the other providers. |

The template can be changed (`envconfig.git.path`). It never comes from the request: a descriptor cannot point to another application's file.

!!! danger "References, never values"
    The configuration is saved in the deploy history, like with Consul. It must contain only secret references (`vault:…`, `file:…`), never passwords or keys.

## 2. Configure manent

```yaml title="manent.yml"
envconfig:
  provider: git
  required: true
  git:
    repository: https://git.example.com/platform/envconfig.git
    ref: refs/heads/main                      # default ref
    path: "{application}/{artifact}/{environment}.yml"
    username: x-access-token                  # HTTPS
    password_ref: vault:secret/manent/git#token
    # ssh_key_ref: file:/run/secrets/git-key  # ssh:// instead of HTTPS
    # known_hosts: /etc/manent/known_hosts    # required with ssh_key_ref
    # ca_file: /etc/manent/git-ca.pem
    allowed_refs: [refs/tags/env-*]           # other refs a descriptor may name
    require_signed: true
    signers:
      ssh_allowed_signers: /etc/manent/allowed_signers
      # openpgp_keyring: /etc/manent/git-keys.asc
    max_age: 0                                # maximum age of a commit (0 = none)
    cache_dir: /var/lib/manent/git            # default <workdir>/.git-cache
    ref_ttl: 30s                              # how long a ref resolution is reused
```

| Key | Meaning |
|---|---|
| `repository` | URL (`https://`, `ssh://`, `git://`, `file://`) or local path. Only from `manent.yml`. |
| `ref` | The ref read when the descriptor names none. Always allowed. |
| `allowed_refs` | Other refs (patterns with `*`) a descriptor may name. A requested SHA must be reachable from the default ref or from one of these. |
| `password_ref`, `ssh_key_ref` | Secret references, resolved at every fetch and kept only in memory. |
| `require_signed`, `signers` | Commits without a valid OpenPGP or SSH signature (namespace `git`) from these keys are refused. The files are read at every check: a rotated key needs no restart. |
| `max_age` | Refuses commits older than this. A manual rollback is exempt: it goes back to a recorded revision. |
| `cache_dir` | Local bare copy, one per repository, on each worker. No shared storage needed. |
| `ref_ttl` | Within this time a ref is resolved without fetching again. A requested SHA missing locally always triggers a fetch. |

manent reads Git with a library built into the binary: the server image needs no `git` executable.

## 3. Which revision a deploy uses

The revision is resolved in the `config` step, when the deploy starts. A scheduled deploy reads the ref at run time, like with the other providers. From then on retries and recoveries reuse the recorded SHA, even if the branch moves.

| Deploy | Revision |
|---|---|
| Deploy | The default ref, or `config.ref` of the descriptor if `descriptor_policy.config_ref` allows it. |
| Promotion | The default ref of the destination; `--config-ref` (or `config_ref` in the API) chooses another. |
| Manual rollback | The SHA recorded by the deploy it goes back to, whatever the policy says. |
| Reconciliation | The SHA of the release in service. |

The `config` step output reports `source` (`git:<repository>//<path>`), `revision` (the SHA) and `ref`. The SHA is also in `Status.config_revision`, in the inventory, in the deploy page of the web interface and in the CI outputs (`config_revision`).

## 4. Let the descriptor choose a revision

By default the descriptor cannot choose (`config_ref: none`). To allow it:

```yaml title="manent.yml"
descriptor_policy:
  config_ref: sha        # none | ref (only refs) | sha (refs and commit SHAs)
```

```yaml title="deploy.yml"
config:
  ref: refs/tags/env-42  # or a commit SHA
```

```sh
manent deploy -f deploy.yml --config-ref 4f9c2e1a7b3d...     # e.g. from CI, the commit tested with the artifact
manent promote -a shop --from test --to prod --config-ref refs/tags/env-42
```

A ref outside `ref` and `allowed_refs`, or a SHA not reachable from them, is refused at submission (422), before the deploy is queued. So are a missing file at a requested revision, an invalid signature and a commit older than `max_age`. An unreachable server is a temporary error: the worker retries.

## 5. Security

| Risk | Mitigation |
|---|---|
| An old revision chosen to skip a constraint added later | `config_ref: none` by default; reachability from allowed refs; optional `max_age` and signatures. |
| A file of another application | The path comes only from the template. |
| An unprotected branch | Only `ref` and `allowed_refs` are read. |
| An arbitrary repository (SSRF) | The repository comes only from `manent.yml`. |
| Exposed credentials | Secret references, never on disk nor in the step outputs. |
| A deleted allowed branch | The fetch removes the local refs gone from the server: its commits stop being reachable. |
