# Glossary

**Activation**
:   The last standard step (`activation`): it makes a release operational when copying it is not enough. Optional, and compensable when the activator supports it.

**Adoption**
:   A deploy of kind `adoption` that records in the inventory a release already running, without touching the targets. Used to move from an environment per customer to [tenants](../guida/tenant.md).

**After (dependency)**
:   A deploy with `after` waits in `WAITING_DEPENDENCY` until the listed deploys succeed. See [Deploy chains](../guida/catene.md).

**Approval**
:   A decision by an identity other than the requester, required by a governance rule before a deploy or between rollout waves. See [Release governance](../guida/governance.md#approvazioni).

**Artifact**
:   The immutable file or image a deploy releases, identified by `type`, `name`, `version`, `source` and its sha256.

**Artifact type**
:   The `artifact.type` field (`application`, `container`, `kubernetes`, `war`, `migration`…). It selects the plugins able to handle the artifact and the rollback policy.

**Bake**
:   The observation time after a rollout stage or a traffic step, during which the health gate is read repeatedly.

**Batch**
:   Within a rollout pass, the group of at most `maxUnavailable` targets updated together.

**Callback**
:   An HTTP endpoint that receives an event for every step and for the outcome of a deploy. See [Callbacks](../guida/callback.md).

**Candidate, stable**
:   In a [percentage rollout](../guida/traffico.md), the new version being released and the one currently in service.

**Canary**
:   A small first share of the release, a few nodes in a wave rollout or a few percent of traffic, observed before going on.

**Checkpoint**
:   A DBOS step (`checkpoint:<step>`) that records, before a compensable step, the state to go back to in a rollback.

**Compensation**
:   The undoing of a step's effects during a rollback (`rollback:<step>`).

**Composite release**
:   A `kind: Release` document grouping several deploys with dependencies and an `onFailure` policy. See [Deploy chains](../guida/catene.md#release-composte).

**`config` step**
:   The first step of every deploy: it reads the environment configuration, merges it with the descriptor and checks freezes and policy again.

**DBOS**
:   The durable workflow library manent is built on. It records the input of every workflow and the result of every step in Postgres (or SQLite in development).

**Deploy**
:   One run of the pipeline for an application, an environment and an artifact. It has an ID, a state, a kind and an outcome.

**Descriptor**
:   The YAML (CLI) or JSON (API) document that requests a deploy. See [Deploy descriptor](../guida/descrittore.md).

**Descriptor policy**
:   The `descriptor_policy` section of `manent.yml`: what a descriptor may add to the environment configuration (parameters, secret references, targets).

**Drain**
:   Taking a batch of targets out of a load balancer before the deploy and putting it back afterwards. See [Load balancer draining](../guida/svuotamento.md).

**Drift**
:   An environment where the targets do not all run the version manent released: after a rollout stopped halfway (`drift`) or after changes outside manent (`out_of_sync`).

**Dry-run**
:   A deploy run as a simulation: same workflow, no changes to the targets. See [Dry-run](../guida/dry-run.md).

**Effective deploy**
:   The descriptor merged with the environment configuration (and the tenant): what plugins actually receive. `manent validate` shows it.

**Environment**
:   Where an application runs (`test`, `prod`). Regions and AZs are target labels inside one environment.

**Environment configuration**
:   Per-application, per-environment document with plugin, targets, parameters, secret references and strategy, read from Consul or files. See [Environment configuration](../guida/configurazione-ambiente.md).

**Executor ID**
:   The stable identifier of a worker (`dbos.executor_id`, default hostname). After a restart a worker resumes only the workflows with its own ID.

**Freeze window**
:   A period in which real deploys are blocked, configured or created at runtime. `hard` windows cannot be overridden. See [Release governance](../guida/governance.md#finestre-di-freeze).

**Gate (health gate)**
:   The check after a rollout stage or traffic step: HTTP probes, metric queries and alerts. Red means the rollout stops.

**Governance**
:   The rules on what may be released, where and when: mandatory promotion, approvals, freezes, change tickets. Written in YAML or [Rego](../guida/policy.md).

**Idempotency key**
:   The descriptor's `id`. A second request with the same `id` returns the existing deploy instead of running it again.

**Inventory**
:   The release in service per application, environment (and tenant), rebuilt from the deploy history. See [Release governance](../guida/governance.md#inventario).

**Kind**
:   How a deploy came about: `deploy`, `promotion`, `rollback` or `adoption`.

**Locator**
:   How the release to go back to is found during a rollback; `plugin` reads it from the targets.

**Pass**
:   The work on one region or AZ within a rollout stage, named `<wave>/<region>/<az>`.

**Plan**
:   The ordered list of steps of a deploy, computed at submission from `pipeline.steps` and the descriptor's `steps`.

**Plugin**
:   The component that releases an artifact type on a kind of target: built in, or an [external](../plugins/esterni.md) process over gRPC.

**Promotion**
:   Releasing in one environment the exact artifact (same sha256) already released successfully in another.

**Reconciliation**
:   Periodic reading of the release actually running on the targets, to report those changed outside manent. See [Real state of the targets](../guida/governance.md#stato-reale).

**Recovery**
:   Resuming the workflows of a worker that stopped, on the same worker after a restart or on another one.

**Region routing**
:   With `dbos.region_routing`, every rollout pass runs on a worker of the pass's region.

**Rollback**
:   Going back to the previous release: automatic inside a failed deploy, or manual as a new deploy of kind `rollback`. See [Rollback](../guida/rollback.md).

**Rollout**
:   The strategy that splits a deploy into stages: [waves](../guida/ondate.md) or [traffic percentages](../guida/traffico.md).

**Schedule**
:   A recurring deploy created by a descriptor with `schedule.cron`. Each tick enqueues a deploy `<schedule>@<UTC time>`.

**Secret reference**
:   A pointer to a secret (`vault:…`, `file:…`, `env:…`), never its value. See [Secrets and artifact sources](../operazioni/segreti-artefatti.md).

**Slot**
:   In percentage rollouts, one of the two places (`a`, `b`, or production and a staging slot) where the candidate and the stable version run side by side.

**Stage**
:   A group of rollout passes run together and followed by the same bake and gate.

**Step**
:   One phase of the pipeline, run as a DBOS step with its own retries and recorded result. Standard steps: `secrets`, `artifacts`, `qa`, `deploy`, `post-test`, `activation`; custom steps of type `command` or `http`.

**Target**
:   A destination of the deploy: a host, a cluster, a cloud region or service, a management endpoint. It carries labels.

**Tenant**
:   A customer or instance with its own parameters, secrets and targets inside a shared environment. See [Tenants](../guida/tenant.md).

**Wave**
:   A group of targets selected by labels in a wave rollout, released after the previous wave's gate is green.

**Worker**
:   A `manent-server` process (`serve` or `worker`) that runs deploys from the queue.
