# Configuring manent

manent reads a YAML file given with `-c` (or the `MANENT_CONFIG` variable). Every value not set takes the default listed below; unknown keys are an error. The complete example file is `configs/manent.yml`.

## What each process uses { #processes }

Every [`manent-server` process](server.md) loads and checks the whole file and builds all its components at startup. The simplest choice is one file shared by API, workers and operators, with only the per-process keys changed (see below). The processes differ in which sections they actually use:

- `api` accepts and checks requests, computes the plan and enqueues it; it never runs a deploy.
- `worker` runs the deploys it dequeues; it has no API, so it does not authenticate anyone.
- `serve` is both in one process and uses the union of the two columns.
- `admin` works on the database without the API (see [Platform operations](server.md#platform-operations)).

| Section | `api` | `worker` | `admin` |
|---|---|---|---|
| [`app`](#app) | `name` | ✓ | ✓ (`version` is the default of `recover --to-version`) |
| [`dbos`](#dbos) | `database_url`, `queue` and the concurrency limits (shown by `manent queues`) | ✓ | `database_url` |
| [`http`](#http) | ✓ | — | — |
| [`logging` and `siem`](#logging-and-siem) | ✓ | ✓ | ✓ |
| [`workdir`](#workdir) | only as the default `cache_dir` of the `git` provider | ✓ | — |
| [`tenants`](#tenants) | ✓ | ✓ | — |
| [`envconfig`](#envconfig) | ✓ (check at submission, `env validate`, imports) | ✓ (`config` step) | — |
| [`callbacks`](#callbacks) | `allow_http`, `allowed_hosts` | ✓ | — |
| [`rollback`](#rollback) | ✓ | ✓ | — |
| [`auth`, `rbac`](#auth-rbac-audit) | ✓ | — (permissions are checked at submission) | — (identity `cli:<user>`, no permissions) |
| [`audit`](#auth-rbac-audit) | ✓ (actions and denials) | ✓ (outcome of deploys, custom step runs) | ✓ (`audit verify`, break-glass actions) |
| [`governance`](#governance) | ✓ | ✓ (checks at start, freezes during rollout, schedule ticks) | `policy-activate`, `freeze-delete` |
| [`descriptor_policy`](#descriptor_policy) | ✓ | ✓ | — |
| [`secrets`](#secrets-and-artifacts) | ✓ | ✓ | ✓ (references resolved at startup) |
| [`artifacts`](#secrets-and-artifacts) | ✓ (allowlist, `--check-artifact`) | ✓ (download) | — |
| [`pipeline`](#pipeline) | `steps`, `custom.spec_key_refs` (plan and signature of custom steps) | ✓ | `custom.revoked_specs` (`admin specs`) |
| [`plugins`](#plugins) | `artifact_types`, `queue`: plugin choice and routing; plugins without their own queue are also activated with their configuration | ✓ (a plugin with its own queue only if listed in `dbos.plugins`, see [plugin queues](scalabilita.md#plugin-queues)) | — |
| [`queues`](#queues) | ✓ (routing) | the queues listed in `dbos.queues` | — |
| [`rollout`](#rollout) | ✓ (a rollout reading a system that is not configured is refused) | ✓ (gate readings) | — |
| [`integrations`](#integrations) | `notifications` (destinations chosen at submission), `slack.signing_secret_ref`, `identities`, `email_identity` (approvals from Slack) | ✓ (sending messages, change tickets) | — |
| [`chains`](#chains) | — | ✓ | — |
| [`observability`](#observability) | ✓ (`/metrics` also on the API port) | ✓ (`listen` is the only way to expose probes and metrics) | — |
| [`recovery`](#recovery) | `stale_after` (`manent workers`) | ✓ | `stale_after` (`admin executors`) |
| [`retention`](#retention) | — | ✓ | `max_age`, `keep_releases` (`admin prune`) |
| [`reconcile`](#reconcile) | — | ✓ | — |

**Values that must be the same everywhere.** The API checks a request with its own configuration and the worker checks it again with its own, so different values give deploys accepted and then refused, or queued where nobody picks them up:

- `app.name` and `dbos.database_url`: the DBOS application and its database.
- `dbos.queue`, `queues`, `plugins.<name>.queue` and `artifact_types`: the API chooses the queue, the worker must serve it.
- `pipeline.steps` and `pipeline.custom.spec_key_refs`: the API signs the custom steps of the plan, the worker verifies the signature. To rotate a key, first add it to every process, then make it the first one.
- `tenants`, `envconfig`, `descriptor_policy`, `governance`, `rollback`, `callbacks.allow_http`/`allowed_hosts`, the systems of `rollout`.

**Values set per process.**

- Workers: `dbos.executor_id` (unique and stable), `dbos.region`, `dbos.tenants`, `dbos.plugins`, `dbos.queues`, `dbos.exclusive`, `dbos.concurrency`, `dbos.drain_timeout`, `workdir`.
- API: `http.listen` (or `serve --listen`).
- All: `observability.listen` and `audit.file`, one file per process: two processes on the same disk need different paths.

The secrets referenced by `pipeline.custom.spec_key_refs` and `audit.hmac_key_ref` are read at startup: every process with the key in its configuration, `admin` included, must be able to resolve them.

## Reference

### `app`

| Key | Default | Description |
|---|---|---|
| `name` | `manent` | Name of the DBOS application and `APP-NAME` of the messages sent to the SIEM. |
| `version` | `0.2.0` | Workflow version recorded by DBOS: a worker resumes only the workflows of its own version. It is not the binary version and should be changed only when the release notes say so. See [Versions and upgrades](aggiornamenti.md). |

### `dbos`

| Key | Default | Description |
|---|---|---|
| `database_url` | `sqlite:manent.db` | DBOS system database. In production Postgres: `postgres://user:pass@host:5432/manent`. |
| `executor_id` | hostname | Worker ID: must be unique among nodes and stable across restarts. See [Scalability](scalabilita.md). |
| `queue` | `deployments` | Name of the deploy queue. |
| `concurrency` | `4` | Deploys running concurrently on each worker. |
| `global_concurrency` | `0` | Deploys running across all workers (`0` = no limit). |
| `environment_concurrency` | `1` | Concurrent deploys of the same application/environment across all workers. Cannot exceed `global_concurrency`. |
| `schedule_poll_interval` | `30s` | How often workers reload recurring schedules. |
| `region` | | Worker region: with `region_routing` it runs the [wave rollout](../guida/ondate.md#esecuzione-nelle-region) passes for that region. |
| `region_routing` | `false` | Runs rollout passes on the workers of the targets' region, as child workflows. In multi-region setups it must be enabled on all workers. |
| `tenants` | `[]` | Restricts the worker to the deploy queues of these [tenants](../guida/tenant.md#queues-and-workers-per-tenant) (the default queue for those without one). Empty: every queue of the registry. |
| `plugins` | `[]` | Plugins with their own queue (`plugins.<name>.queue`) this worker serves: it registers and dequeues their queues besides its own. See [plugin queues](scalabilita.md#plugin-queues). |
| `queues` | `[]` | Queues of the [`queues`](#queues) section this worker serves: it registers and dequeues them besides its own. See [routed queues](scalabilita.md#routed-queues). |
| `exclusive` | `false` | The worker dequeues only the queues of `queues` and `plugins` (and the approvals), not the default and tenant queues: a pool reserved to those deploys. Requires `queues` or `plugins`; not with `tenants`. |
| `drain_timeout` | `2m` | On shutdown the worker stops picking up deploys and waits for those in progress up to this time (`0` = does not wait). See [drain](affidabilita.md#arresto-ordinato-drain). |

### `http`

| Key | Default | Description |
|---|---|---|
| `listen` | `:8080` | API address (can be overridden with `serve --listen`). |
| `ui.enabled` | `true` | Serves the [web interface](../guida/interfaccia-web.md) at `/`. |
| `trusted_proxies` | `[]` | Addresses or CIDRs of the reverse proxies whose `X-Forwarded-For` gives the client address in logs and audit. Empty: the header is ignored, so nobody can forge the address. |

### `logging` and `siem`

| Key | Default | Description |
|---|---|---|
| `logging.level` | `info` | `debug`, `info`, `warn`, `error`. |
| `logging.format` | `text` | `text` or `json`. |
| `siem.enabled` | `false` | Forwards every log to the structured log destinations. |
| `siem.destinations` | `[]` | The destinations, at least one with `enabled`; each forwards every message. |
| `siem.destinations[].network` | | `udp`, `tcp`, `unixgram` (local syslog socket) or `journald` (native protocol). |
| `siem.destinations[].address` | | `host:port` of the collector; for `unixgram` and `journald` the socket path (default `/dev/log` and `/run/systemd/journal/socket`). |

See [Logs and SIEM](log-siem.md).

### `workdir`

Default `./work`. Local directory into which each deploy downloads the artifact (`workdir/<deployment-id>`). It does not need to be shared between workers.

### `tenants`

| Key | Default | Description |
|---|---|---|
| `file` | | The [tenant registry](../guida/tenant.md) (`tenants.yml`). Empty: no registry, every application is `untenanted`. |
| `cache` | `30s` | How long the registry is kept before reading it again. |
| `untenanted` | `allow` | The applications no tenant lists: `allow` deploys them as the tenant `untenanted`, `reject` refuses them ([details](../guida/tenant.md#is-a-tenant-required-to-deploy)). |

### `envconfig`

| Key | Default | Description |
|---|---|---|
| `providers` | `[]` | The named providers, none = no environment configuration ([Several providers](../guida/configurazione-ambiente.md#piu-provider)): `name`, `type` (`consul`, `file`, `git`), the section of the type with the keys below, `match` (`applications`, `artifacts`, `environments`) or `tenant_only`, and `on_missing` (`fail`, `next`). |
| `required` | `false` | Makes the deploy fail if there is no configuration for the environment. |
| `max_retries` | `3` | Additional read attempts. |
| `providers[].consul.address` | `CONSUL_HTTP_ADDR` or `http://127.0.0.1:8500` | Address of the Consul agent. |
| `providers[].consul.datacenter` | | Consul datacenter. |
| `providers[].consul.key` | `manent/{tenant}/{application}/{artifact}/{environment}` | KV key template. |
| `providers[].consul.token_file` | | File with the ACL token (alternative to `CONSUL_HTTP_TOKEN`). |
| `providers[].file.path` | `./envconfig/{tenant}/{application}/{artifact}/{environment}.yml` | Path template for the `file` provider. |
| `providers[].git.repository` | | Git repository of the configurations (`https://`, `ssh://`, `git://`, `file://` or a path). See [Git repository](../integrazioni/git.md). |
| `providers[].git.ref` | `refs/heads/main` | Default ref. |
| `providers[].git.path` | `{tenant}/{application}/{artifact}/{environment}.yml` | File template in the repository. |
| `providers[].git.allowed_refs` | `[]` | Other refs (patterns with `*`) `config.ref` may name; a SHA must be reachable from them or from `providers[].git.ref`. |
| `providers[].git.username`, `providers[].git.password_ref` | | HTTPS credentials (the password is a secret reference). |
| `providers[].git.ssh_key_ref`, `providers[].git.known_hosts` | | SSH key (secret reference) and known hosts file, required together. |
| `providers[].git.ca_file` | | Additional CA bundle for HTTPS. |
| `providers[].git.require_signed` | `false` | Refuses commits without a valid signature of `providers[].git.signers` (`openpgp_keyring`, `ssh_allowed_signers`). |
| `providers[].git.max_age` | `0` | Maximum age of a commit (`0` = none); manual rollbacks are exempt. |
| `providers[].git.cache_dir` | `<workdir>/.git-cache` | Local copies of the repository. |
| `providers[].git.ref_ttl` | `30s` | How long a ref resolution is reused before fetching again. |

See [Environment configuration](../guida/configurazione-ambiente.md).

### `callbacks`

| Key | Default | Description |
|---|---|---|
| `timeout` | `10s` | Timeout of each call. |
| `max_retries` | `3` | Additional attempts of each delivery. |
| `allow_http` | `false` | Allows `http` callbacks. |
| `allowed_hosts` | `[]` | Allowed hosts, including `*.domain`; empty = all. |

See [Callbacks](../guida/callback.md).

### `rollback`

| Key | Default | Description |
|---|---|---|
| `locator` | `plugin` | Default strategy to find the release to go back to. |
| `artifact_types.<type>.allowed` | `application`, `container`, `kubernetes`, `ecs`, `lambda`, `cloudrun`: `true`; `migration`: `false` | Rollback is possible for the type. Types not listed do not allow it. |
| `artifact_types.<type>.enabled` | `false` | Default for deploys of the type. Requires `allowed`. |
| `artifact_types.<type>.on` | `[deploy, post-test, activation]` | Steps whose failure triggers rollback. |
| `artifact_types.<type>.locator` | | Type-specific locator. |

See [Rollback](../guida/rollback.md).

### `auth`, `rbac`, `audit`

| Key | Default | Description |
|---|---|---|
| `auth.mode` | `none` | `none`: no authentication or permissions (development only). `enabled`: bearer token required and RBAC. |
| `auth.oidc` | | `issuer`, `audience`, `username_claim`, `groups_claim`, `ca_file`, `policy_claims` (default `[email, name]`: claims exposed to the [permission policy](../guida/permessi.md)). |
| `auth.tokens` | `[]` | Static tokens: `name`, `sha256` (never the token), `groups`, `expires_at`. |
| `auth.tokens_file` | | YAML file with more tokens (same format, `tokens` key), reloaded when it changes: revocation without restart. |
| `rbac.engine` | `builtin` | `builtin` (the bindings below) or `opa` (the [permission policy](../guida/permessi.md) of the revisions; requires `governance.engine: opa`). `policy.*` always come from `rbac.bindings`. |
| `rbac.opa.cache` | `30s` | How long the grants of an identity are reused with `opa`. |
| `rbac.opa.cache_size` | `10000` | Identities in the grants cache. |
| `rbac.roles` | `{}` | Additional roles: name → list of actions. |
| `rbac.bindings` | `[]` | `subjects` or `groups`, `role`, `applications` (qualified patterns: `shop` only untenanted, `payments/*`, `*/*` every [tenant](../guida/tenant.md#qualified-application-patterns)), `environments` (patterns with `*`), `artifacts`, `tenants` (selector on the owner tenant) and `selector` (selector on the application tags). |
| `rbac.tenant_roles` | `{members: deployer, admins: admin}` | Implicit roles of the `members` and `admins` of a [tenant](../guida/tenant.md) on its applications. Never `policy.*`. |
| `rbac.selector_keys` | `[]` | Keys the `selector` of bindings and grants can read (empty: no selector in permissions). |
| `rbac.cross_tenant_inventory` | `true` | Whoever belongs to a tenant reads the inventory of every tenant. |
| `audit.file` | | JSON Lines file of audit entries, with a hash chain, in addition to the log and the SIEM. |
| `audit.max_size_mb` | `0` | Size beyond which the file is rotated (`0` = no rotation). |
| `audit.hmac_key_ref` | | Reference to the key (at least 16 bytes) that turns the chain into an HMAC. |

See [Authentication, permissions and audit](accesso.md).

### `governance`

| Key | Default | Description |
|---|---|---|
| `governance.engine` | `builtin` | `builtin`: the rules and windows of this file. `opa`: Rego policies evaluated by OPA, changeable at runtime. |
| `governance.rules` | `[]` | Rules per application and environment (the first match applies): `name`, `applications`, `environments`, `requires` (environments to promote from), `approvals` (`required`, `allow_self`, `timeout`, default `72h`, `when`, `approvers` with `groups` and `subjects`), `change` (`approval`, `timeout`, default `72h`: change ticket, see [Chat and change management](../guida/integrazioni.md#change-management)). With `opa` they are the data of the default policy. |
| `governance.freezes` | `[]` | Freeze windows: `name`, `reason`, `applications`, `environments` and `from`/`to` or `cron` (5 fields), `duration`, `timezone`, `hard`. Windows can also be created at runtime (`manent freeze add`). |
| `governance.refresh` | `10s` | How often every process reads policy and runtime freezes again. |
| `governance.max_staleness` | `10m` | Beyond this without a successful read, real deploys are refused (`503`). |
| `governance.start` | `deny` | What the deploy start checks again: `deny` (denials and freezes) or `strict` (also approvals and change grown after the submission). |
| `governance.opa.source` | `store` | Revisions: `store` (managed with `manent policy`), `file`, `bundle`, `consul`. |
| `governance.opa.store.review` | `false` | A draft is approved by an identity other than its author. |
| `governance.opa.store.tests` | `run` | `run`: the revision's tests run if present; `required`: revisions without tests are refused. |
| `governance.opa.file.dir` | | Directory of the policy with `source: file`. |
| `governance.opa.bundle.*` | | `url`, `ca_file`, `credentials_ref`, `signing` (`public_key_ref`, `algorithm`, `key_id`, `scope`), `insecure_skip_verify`. |
| `governance.opa.consul.prefix` | `manent/policies` | KV prefix with `source: consul` (address and token of `governance.opa.consul.address`/`token_file`, otherwise of the first `consul` provider of `envconfig.providers`). |
| `governance.opa.consul.address`, `datacenter`, `token_file` | | Consul agent of the policies; empty: that of the first `consul` provider of `envconfig.providers`. |
| `governance.opa.query` | `data.manent.governance.decision` | Query of the decision. |
| `governance.opa.eval_timeout` | `200ms` | Limit of one evaluation. |

See [Release governance](../guida/governance.md) and [Policies in Rego](../guida/policy.md).

### `descriptor_policy`

| Key | Default | Description |
|---|---|---|
| `params` | `{"*": ["*"]}` | Parameters the descriptor may set, per plugin. |
| `secret_refs` | `["*"]` | Secret references the descriptor may declare. |
| `targets` | `true` | Targets given by the descriptor. |
| `post_test_hosts` | `["*"]` | Hosts the descriptor's `postTest.http` checks may reach (patterns). |
| `step_params` | `{}` | Parameters of [custom steps](../guida/step-personalizzati.md) the descriptor may set, per step (`"*"` = all). Default: none. |
| `config_ref` | `none` | Revision of the environment configuration the descriptor may choose with `config.ref` ([git provider](../integrazioni/git.md)): `none`, `ref` (allowed refs only) or `sha` (also commit SHAs). |
| `max_parallel` | `0` (no limit) | Maximum [parallelism](../guida/ondate.md#parallel-targets) a descriptor may ask for (`deploy.parallel`, `rollout.waves[].parallel`); a percentage is resolved against all the targets. The environment configuration is not subject to it. |

See [Security](sicurezza.md#cosa-puo-chiedere-un-descrittore).

### `secrets` and `artifacts`

| Key | Default | Description |
|---|---|---|
| `secrets.vault.enabled` | `false` | Enables the `vault:` scheme. |
| `secrets.vault.address`, `namespace`, `auth`, `token_file`, `approle`, `kubernetes`, `ca_file`, `timeout` | | Vault connection and login. |
| `artifacts.timeout` | `30m` | Maximum duration of a download. |
| `artifacts.allow_unlisted` | `true` | Allows sources not listed in `artifacts.sources`. |
| `artifacts.sources` | `[]` | `prefix`, `auth` (`type`, `username`, `header`, `secret_ref`), `ca_file`. |

See [Secrets and artifact sources](segreti-artefatti.md).

### `pipeline`

Ordered list of steps, with `name`, `mode` (`mandatory` or `optional`), `enabled` (default of optional steps) and `max_retries`. See [Pipeline and steps](../concetti/pipeline.md).

A step with `type` (`command` or `http`) is a [custom step](../guida/step-personalizzati.md), defined by `timeout`, `secrets`, `params`, `outputs`, `ssh`, `run` and `dry_run`. They are configured by `pipeline.custom`:

| Key | Default | Description |
|---|---|---|
| `pipeline.custom.spec_key_refs` | `[]` | HMAC keys (at least 32 bytes) that sign the specifications stored in plans; the first signs, all verify. |
| `pipeline.custom.revoked_specs` | `[]` | SHA-256 of specifications that must not run any more. |
| `pipeline.custom.known_hosts` | `~/.ssh/known_hosts` | Host keys of the targets for command steps. |
| `pipeline.custom.insecure_ignore_host_key` | `false` | Skips the host key check: development only. |
| `pipeline.custom.secrets_dir` | `/dev/shm/manent` | Secrets directory on the targets. |
| `pipeline.custom.allow_http` | `false` | Allows `http` URLs in HTTP steps. |

### `plugins`

Plugins to enable, each with its own configuration:

```yaml
plugins:
  remote-copy:                       # plugin compiled into manent
    known_hosts: ~/.ssh/known_hosts
    artifact_types: [application]
  k8s-deploy:                        # external plugin
    external:
      command: /opt/manent/plugins/k8s-deploy
      sha256: "3f1c…"
    artifact_types: [container]
```

Two keys are read by manent and not passed to the plugin: `queue` (the plugin's [own queue](scalabilita.md#plugin-queues)) and `max_parallel`, the cap on the targets of a batch the plugin updates at the same time, whatever the deploy asks for (default `16`): it protects workers and destinations, such as SSH connections or the rate limits of a cloud API. See [parallel targets](../guida/ondate.md#parallel-targets).

Plugins compiled into manent: [remote-copy](../plugins/remote-copy.md), [container](../plugins/container.md), [kubernetes](../plugins/kubernetes.md), [ecs](../plugins/ecs.md), [lambda](../plugins/lambda.md), [cloudrun](../plugins/cloudrun.md), [jboss](../plugins/jboss.md), [windows](../plugins/windows.md), [appservice](../plugins/appservice.md), [containerapps](../plugins/containerapps.md), [script](../plugins/script.md) (scripts on the worker, from a catalog). See also [External plugins](../plugins/esterni.md).

### `queues` { #queues }

Dedicated queues for the deploys of some applications, artifacts and environments, decided by the operators: neither the descriptor nor the environment configuration can choose a queue. The first matching entry wins; only the workers that list the queue in `dbos.queues` run its deploys.

```yaml
queues:
  - name: gpu
    match:
      applications: ["ml/*"]         # tenant/application patterns, as in permissions
      artifacts: [trainer]           # patterns on artifact.name
      environments: [prod]
    concurrency: 2
```

| Key | Default | Description |
|---|---|---|
| `name` | | Queue name: letters, digits, `.`, `_`, `-`. Not `dbos.queue`, its approvals queue or a plugin's queue. |
| `match` | | `applications`, `artifacts`, `environments`: at least one pattern; an empty list matches everything. |
| `concurrency` | `0` | Global limit of the queue (`0`: `dbos.global_concurrency`). It also caps the queue's deploys on each worker. |

A plugin's queue wins over a routed queue, which wins over the tenant's. Not supported with `dbos.region_routing` yet. See [routed queues](scalabilita.md#routed-queues).

### `rollout`

Health gate of [wave rollouts](../guida/ondate.md#health-gate).

| Key | Default | Description |
|---|---|---|
| `prometheus_url` | | Prometheus API for `rollout.health.metrics` (e.g. `http://prometheus:9090`). Without it, a rollout with Prometheus metrics is rejected; the same holds for the other systems. |
| `alertmanager_url` | | Alertmanager API for `rollout.health.alerts`. |
| `datadog` | | Datadog for metrics with `provider: datadog`: `url` (API site, default `https://api.datadoghq.com`), `api_key_ref`, `app_key_ref`. |
| `newrelic` | | New Relic NerdGraph: `url` (default `https://api.newrelic.com/graphql`), `account_id`, `api_key_ref` (user key). |
| `elastic` | | Elasticsearch ES\|QL: `url`, `api_key_ref` or `username` and `password_ref`, `ca_file`. |
| `cloudwatch` | | Amazon CloudWatch: `region`, `endpoint` (optional), credentials from the SDK chain or `access_key_id_ref` and `secret_access_key_ref`, `role_arn` and `external_id` to assume a role. |
| `azure_monitor` | | Azure Monitor Logs (`provider: azure-monitor`): `workspace_id`, a service principal with `tenant_id`, `client_id` and `client_secret_ref` or, without them, the identity of the worker (workload or managed identity); `endpoint` and `authority` for sovereign clouds. |
| `check_timeout` | `30s` | Maximum time of each gate reading. |

### `integrations`

Notifications in Slack and Teams, approvals from Slack and change tickets. See [Chat and change management](../guida/integrazioni.md).

| Key | Default | Description |
|---|---|---|
| `integrations.public_url` | | Address of the web interface, for links in messages and tickets. |
| `integrations.language` | `en` | Message language: `en` or `it`. |
| `integrations.timeout` | `15s` | Timeout of calls to Slack, Teams and the ITSM. |
| `integrations.slack.bot_token_ref`, `signing_secret_ref` | | Secrets of the Slack app: updatable messages and approval buttons. |
| `integrations.slack.identities` | | Slack user ID → `subject` (`oidc:alice`) and `groups` of the manent identity. |
| `integrations.slack.email_identity` | | Identity of unlisted users from their email, e.g. `oidc:{email}`. |
| `integrations.notifications` | `[]` | Destinations: `name`, `applications`, `environments`, `events`, `dry_run` and `slack` (`channel` or `webhook_ref`) or `teams` (`webhook_ref`). |
| `integrations.change.provider` | | `servicenow` or `jira`. |
| `integrations.change.poll_interval` | `1m` | Initial interval for reading the change approval (up to 5 minutes). |
| `integrations.change.servicenow`, `jira` | | Connection and fields of the ITSM system. |

### `chains`

[Deploy chains](../guida/catene.md): dependencies between deploys and composite releases.

| Key | Default | Description |
|---|---|---|
| `poll_interval` | `5s` | First interval at which a waiting deploy or a release reads the state of the deploys it waits for again; it doubles at each reading up to 5 minutes. Usually the reading happens earlier, because a deploy that ends notifies whoever waits for it. |

### `observability`

| Key | Default | Description |
|---|---|---|
| `listen` | | Address of a server with `/metrics`, `/healthz` and `/readyz` (e.g. `:9090`). Needed by workers, which have no API. |
| `metrics` | `true` | Prometheus metrics. |
| `tracing.enabled` | `false` | Exports OpenTelemetry traces over OTLP/HTTP. |
| `tracing.endpoint` | | Collector, e.g. `otel-collector:4318`; empty: `OTEL_EXPORTER_OTLP_ENDPOINT`. |
| `tracing.insecure` | `false` | Uses `http` instead of `https` (endpoint without scheme). |
| `tracing.sample_ratio` | `1` | Fraction of traces sampled, if the request does not already carry a decision. |

See [Observability](osservabilita.md).

### `recovery`

| Key | Default | Description |
|---|---|---|
| `heartbeat_interval` | `15s` | How often a worker updates the registry. |
| `stale_after` | `1m` | Heartbeat delay beyond which a worker is considered lost (at least twice `heartbeat_interval`). |
| `auto_recover_after` | `0s` | If greater than zero, the pending workflows of a worker of the same version that has stopped, or has sent no heartbeat for this long, are resumed by another worker. At least `stale_after`. |

See [Recovery, shutdown and retention](affidabilita.md).

### `retention`

Like all durations in the configuration, values are written with the unit (`0s`, `10m`, `2160h`): a number without a unit is not valid.

| Key | Default | Description |
|---|---|---|
| `max_age` | `0s` | Workflows finished more than this long ago are deleted (`0s` = never; otherwise at least `24h`). |
| `keep_releases` | `10` | Successful deploys always kept per application and environment. |
| `interval` | `1h` | How often a worker runs the cleanup. |

### `reconcile` { #reconcile }

Periodic reading of the release in service on the targets, to report in the inventory those changed outside manent (see [real state of the targets](../guida/governance.md#stato-reale)).

| Key | Default | Description |
|---|---|---|
| `interval` | `0s` | How often the workers read the targets of every environment (`0s` = disabled; otherwise at least `10s`). |
| `timeout` | `30s` | Maximum time to read one target. |
| `concurrency` | `8` | Targets of an environment read together. |

## Environment variables

| Variable | Effect |
|---|---|
| `MANENT_CONFIG` | Configuration file, alternative to `-c`. |
| `DBOS_SYSTEM_DATABASE_URL` | Overrides `dbos.database_url`. |
| `DBOS__VMID` | Overrides `dbos.executor_id`. |
| `CONSUL_HTTP_ADDR` | Consul address, if the `address` of a Consul provider is empty. |
| `CONSUL_HTTP_TOKEN` | Consul ACL token. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Trace collector, if `observability.tracing.endpoint` is empty. The other `OTEL_EXPORTER_OTLP_*` variables (headers, certificates) also apply. |

Secrets with an `env:NAME` reference are read from the worker's environment variables.
