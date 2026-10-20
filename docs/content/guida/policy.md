# Policies in Rego

With `governance.engine: opa` the [governance rules](governance.md) are written in **Rego** and evaluated by OPA embedded in manent: no sidecar, no network call. The policy can be changed **at runtime**: a new revision is pushed, tested, measured on the real deploys and approved by another person, and every process picks it up within seconds, without restarting.

The rules of `manent.yml` keep working: without a revision, the default policy applies them unchanged (`governance.rules` become the data `data.manent.rules`).

```yaml title="manent.yml"
governance:
  engine: opa                 # builtin (default) | opa
  refresh: 10s                # how often every process reads policy and runtime freezes
  max_staleness: 10m          # beyond this without a successful read, real deploys are refused
  start: deny                 # deny | strict: what the deploy start checks again (see below)
  opa:
    source: store             # store (default) | file | bundle | consul
    store:
      review: true            # approval by an identity other than the author (four eyes)
      tests: run              # run (default) | required: revisions without tests are refused
    # query: data.manent.governance.decision
    # eval_timeout: 200ms
  rules: [...]                # data of the default policy
  freezes: [...]
```

## The decision

The policy receives an **input** describing the deploy and returns a **decision**: denials and obligations. manent enforces the obligations as it does with the YAML rules: the policy says *what* is needed, manent checks it (the same artifact already released in `requires`, the approvals, the change ticket).

### Input

```json
{
  "schema": 2,
  "phase": "submit",
  "now": "2026-10-18T09:30:00Z",
  "application": "shop",
  "environment": "prod",
  "region": "",
  "deployment": {"id": "ci-42", "kind": "deploy", "source": "", "dry_run": false, "schedule": {},
                 "freeze_override": "", "after": [], "release": ""},
  "artifact": {"name": "shop", "version": "1.4.0", "type": "application", "sha256": "…", "source": "oci://…"},
  "requested_by": {"id": "oidc:alice", "method": "oidc", "name": "alice", "groups": ["team-shop"]},
  "plugin": "kubernetes",
  "rollout": {"strategy": "waves", "waves": 3, "regions": ["eu", "us"], "approval_points": 1},
  "targets": [{"host": "k8s-eu", "labels": {"region": "eu"}}],
  "current": {"id": "…", "version": "1.3.2", "sha256": "…", "deployed_at": "2026-10-10T12:00:00Z"},
  "freezes": {"active": [{"name": "year-end", "end": "2027-01-07T00:00:00+01:00", "hard": false}]}
}
```

| Field | Notes |
|---|---|
| `phase` | `submit` (enqueuing), `validate`, `start` (deploy start on the worker), `stage` (start of a rollout stage, once per region), `tick` (run of a recurring schedule). |
| `now` | The instant of the evaluation: the submission (or `schedule.at`) or the time read by the step that evaluates. Policies use it instead of the clock, which is not available. |
| `deployment.kind` | `deploy`, `promotion`, `rollback`. |
| `targets`, `plugin`, `rollout` | From the effective deploy (environment configuration merged), when it is known. |
| `current` | Release in service in the environment; absent on the first release. |
| `freezes.active` | Windows active at `now`, computed by manent. |
| `tenant`, `tenants_run` | The [tenant](tenant.md) of the deploy (`name`, `tags`) and the multi-tenant release it belongs to (`id`); absent without tenant. Added in schema 2. |

The input contains no secrets and no plugin parameters. The schema is versioned (`schema`): fields are only added.

### Decision

The query `data.manent.governance.decision` must always produce an object (give a `default`): an undefined decision refuses the deploy.

| Key | Description |
|---|---|
| `rule` | Descriptive name, shown in the status and in `validate`. |
| `deny` | Set of `{"code": …, "message": …}` (or strings): a non-empty set refuses the deploy with the messages. |
| `requires` | Environments where the same artifact must already have been released. |
| `approvals` | `required`, `allow_self`, `timeout` (`"72h"`), `when` (`before`/`rollout`) and `approvers` (`groups`, `subjects`): who can approve, besides the `deployment.approve` permission. `null` or `required: 0`: no approval. |
| `change` | `approval`, `timeout`: change ticket in the ITSM ([change management](integrazioni.md#change-management)). |
| `freeze_exempt` | Names of active windows the policy exempts. A policy can only exempt: active windows block by default, and the `hard` ones are never exempted. |
| `notes` | Explanations shown by `validate`, the CLI and the web interface. |

Unknown keys, wrong types and invalid durations are errors.

### Example

```rego title="governance.rego"
package manent.governance

import rego.v1

import data.manent.lib

prod if lib.match_any(["prod", "prod-*"], input.environment)

requires contains "preprod" if {
	prod
	input.deployment.kind != "rollback" # a rollback must never wait for preprod
}

default approvals := null

approvals := {"required": 2, "timeout": "72h", "approvers": {"groups": ["dba"]}} if {
	prod
	input.artifact.type == "migration"
} else := {"required": 1} if prod

default change := null

# Out of business hours, the change approved in the ITSM too.
change := {"approval": true, "timeout": "24h"} if {
	prod
	not lib.business_hours(input.now, "Europe/Rome")
}

# Hotfixes with an incident pass the weekend freeze (never a hard one).
freeze_exempt contains "weekend" if {
	endswith(input.artifact.version, "-hotfix")
	startswith(input.deployment.freeze_override, "INC-")
}

deny contains {"code": "downgrade", "message": "downgrades in production go through manent rollback"} if {
	prod
	input.deployment.kind == "deploy"
	lib.semver_less(input.artifact.version, input.current.version)
}

decision := {
	"rule": "custom",
	"deny": deny,
	"requires": requires,
	"approvals": approvals,
	"change": change,
	"freeze_exempt": freeze_exempt,
}
```

### Library and restrictions

`import data.manent.lib` gives deterministic functions:

| Function | Description |
|---|---|
| `match_any(patterns, value)` | Patterns with `*` as in `manent.yml` (`glob.match` of OPA has another syntax). |
| `semver_less(a, b)` | Versions compared by number; `v` prefix allowed, a pre-release precedes its release. |
| `local_time(t, tz)` | `year`, `month`, `day`, `weekday` (0 = Sunday), `hour`, `minute` of an instant in a time zone. |
| `business_hours(t, tz)` | 9:00–18:00, Monday to Friday. |
| `tenant_matches(selector, tenant)` | A tenant selector on `input.tenant` (`{}` without tenant: never). |
| `duration_ns(d)` | A duration (`"72h"`) in nanoseconds. |

`data.manent.lib.rules` implements the semantics of the YAML rules on `data.manent.rules`: a policy can delegate to it the cases it does not handle (`decision := rules.decision if not mine`).

An evaluation depends only on input and data, so the same input with the same revision always gives the same decision:

- the built-ins reading the clock, the network or the runtime (`time.now_ns`, `http.send`, `net.lookup_ip_addr`, `rand.intn`, `uuid.rfc4122`, `opa.runtime`, `trace` and the other nondeterministic ones) do not compile;
- Rego v1 in strict mode;
- every evaluation lasts at most `eval_timeout` (default 200ms);
- the `manent.lib` packages are reserved.

### Data

A revision can contain data: `data.json` or `data.yaml` in a directory `a/b/` go under `data.a.b`. Two special paths:

- `data.manent.rules`: rules with the schema of `governance.rules` (durations as strings). Without them in the revision, those of `manent.yml` are used.
- `data.manent.freezes`: freeze windows with the schema of `governance.freezes` (and `hard`), for the sources other than the store (file, bundle, Consul), versioned with the policy.

### Tests

Modules `*_test.rego` are the revision's tests (`opa test`, with `with input as …`). They run at every push: a failing test refuses the revision; with `store.tests: required` a revision without tests is refused too. Every revision must also produce a valid decision on a set of sample inputs (every phase and kind).

```sh
manent-server policy test -d policies/                # compile and test locally, without server
manent-server -c manent.yml policy eval -d policies/ -f deploy.yml [--phase start] [--kind rollback]
manent-server -c manent.yml policy export-rbac -d policies/   # bindings of manent.yml as data, rbac.engine: opa
```

With `-c` the rules of the configuration become `data.manent.rules`, as on the server.

## Revisions managed by manent { #revisioni }

With `source: store` (default) the revisions live in the manent database and are managed with the CLI, the API or the [web interface](interfaccia-web.md) (Policy page).

```sh
manent policy push -d policies/ -m "DBA approval for migrations"   # draft: compiled and tested by the server
manent policy diff 12 13                                           # differences between two revisions
manent policy impact 13 --since 30d                                # decisions of the real deploys recomputed
manent policy eval 13 -f deploy.yml                                # decision on a descriptor
manent policy approve 13                                           # activation, by another identity with review: true
manent policy activate 12                                          # back to a revision already active in the past
manent policy list | show 13 | pull 13 -d out/
```

- **Draft, then active.** A pushed revision is a draft: it applies only once approved. The author cannot approve it with `store.review: true` (`403`).
- **Impact.** `impact` rebuilds the inputs of the deploys of the period from their recorded requests (with `now` equal to their submission) and compares the obligations recorded then with those of the revision. It shows, before the activation, a revision that would block everything or remove approvals.
- **Immutability.** A revision never changes: a fix is a new revision. The sha256 of the files identifies it.
- **Propagation.** Every process reads the active revision every `refresh`; on Postgres a notification (`LISTEN/NOTIFY`) brings it in under a second. An invalid revision never replaces the one in force.
- **Back.** `activate` goes back to any revision already active in the past. To go back to the YAML rules, push a revision with only `data.json` containing `{}`.

Without any active revision the default policy applies the rules of `manent.yml`.

## Other sources

| Source | Configuration | When it is read again |
|---|---|---|
| `file` | `opa.file.dir`: *.rego and data files, hidden files excluded (e.g. a Kubernetes ConfigMap) | names, sizes or modification times changed |
| `bundle` | `opa.bundle.url` (OPA bundle, tar.gz), `ca_file`, `credentials_ref` (bearer token), `signing.public_key_ref`, `algorithm` (default RS256), `key_id`, `scope` | the ETag changed (`If-None-Match`) |
| `consul` | `opa.consul.prefix` (default `manent/policies`): every key under the prefix is a file; address and token of `envconfig.consul` | the index of the prefix changed |

The **signature** of the bundle (`.signatures.json`, as produced by `opa build --signing-key`) is required: an unsigned bundle is accepted only with `insecure_skip_verify: true`, for development. With these sources revisions are versioned outside manent (Git, CI, `opa test`): `manent policy push` and `approve` are not available, `policy list` shows the revision in force.

## Freezes at runtime { #freeze-a-runtime }

Freeze windows can also be created and closed at runtime, with both engines, without changing `manent.yml`:

```sh
manent freeze add --name incident-881 --reason "incident 881" -e prod [-a 'shop-*'] [--region us-1] --for 4h [--hard]
manent freeze add --name year-end-close -e prod --from 2026-12-31T18:00:00+01:00 --until 2027-01-02T08:00:00+01:00
manent freeze list
manent freeze delete incident-881
```

- **Effect.** A real deploy in the window is refused at once; a deploy already queued fails at its start; a running wave rollout pauses before its next stage and resumes when the window closes.
- **Hard.** With `--hard` the window is not exempted by the policy and `freezeOverride` does not bypass it.
- **Permission.** `freeze.write` on every application/environment pattern of the window: a `*` pattern requires the permission on `*`. The predefined `release-manager` role grants it.
- **Listing.** `manent freezes` and the Calendar page show them with their author (`dynamic`, `created_by`). Only the windows created at runtime can be deleted.

## Determinism and recording { #determinismo-e-registrazione }

A decision is taken once, at a recorded point, and reused:

| Phase | Where | Effect | Recorded in |
|---|---|---|---|
| `submit` | enqueuing (deploy, promotion, rollback, every deploy of a release) | denials, freeze, `requires`, obligations | the request: revision, rule, input hash (`policy` in the status) |
| `start` | step `config` | denials and freezes arrived after the submission | output of the step (`policy_revision`) |
| `stage` | step `policy@<stage>#<n>` before every rollout stage | freeze per region (pause) or denial (the rollout stops) | output of the step |
| `tick` | step `policy` of every run of a recurring schedule | the obligations of that run | the request of the run |

A recovery reads the recorded decisions again and never evaluates again: a revision changed meanwhile does not change the sequence of the steps. Workflows enqueued before the introduction of the decision keep the previous behaviour.

**Obligations are fixed at submission:** an approval already given is not invalidated by a stricter revision. With `governance.start: strict` the deploy start also refuses a deploy whose approvals or change management grew after the submission (it must be submitted again).

## Unavailability

| Situation | Effect |
|---|---|
| No revision loadable at startup | `/readyz` answers `503`; real deploys are refused with `503`, the `config` step is retried. Dry-runs, reads and cancellations work. |
| Source unreachable, revision already loaded | the loaded one stays in force; beyond `max_staleness` as above. |
| Evaluation error (runtime error, timeout, invalid decision, undefined decision) | refusal, never a green light: `422` at submission, retried in the steps. |

## Permissions and audit

| Action | Allows |
|---|---|
| `policy.read` | Listing and reading the revisions. |
| `policy.write` | Pushing, evaluating and measuring the impact of revisions. |
| `policy.approve` | Approving drafts and going back to previous revisions. |
| `freeze.write` | Creating and deleting freezes at runtime. |

The `policy.*` actions do not depend on application and environment, like `platform.read`; only `admin` grants them among the predefined roles. They always come from `rbac.bindings` in `manent.yml`, even when the other permissions are decided by the revisions ([Permissions as a policy](permessi.md)): a revision cannot grant itself the power to change the policy.

Every operation is in the [audit](../operazioni/accesso.md#audit): `policy.push`, `policy.activate`, `freeze.create`, `freeze.delete`. Every decision is logged (and sent to the SIEM) with revision and input hash.

**Break-glass.** If a revision blocks everything or a second approver is missing during an incident, an operator with access to the database can act outside the permissions, recorded in the audit:

```sh
manent-server admin policy-activate 12 --reason "INC-881: revision 13 blocks the hotfix"
manent-server admin freeze-delete incident-881 --reason "freeze closed by the incident manager"
```

## Metrics

| Metric | Description |
|---|---|
| `manent_policy_revision_info{engine,source,revision}` | Revision in force in the process. |
| `manent_policy_age_seconds` | Seconds since the last successful read of policy and freezes. |
| `manent_policy_load_errors_total` | Failed reads (revision refused or source unreachable). |
| `manent_policy_evaluations_total{phase,outcome}` | Evaluations by phase and outcome (`allowed`, `denied`, `frozen`, `error`, `unavailable`). |
| `manent_policy_eval_duration_seconds{phase}` | Duration of the evaluations. |

An alert on `manent_policy_age_seconds` approaching `max_staleness` comes before the refusals.

## API

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/v1/policies` | Revisions and policy in force. |
| `POST` | `/api/v1/policies` | Push a draft (`{"files": {...}, "comment": ...}`). |
| `GET` | `/api/v1/policies/{revision}` | A revision with its files (`current`: the one in force). |
| `POST` | `/api/v1/policies/{revision}/evaluate` | Decision on a descriptor (`phase`, `kind`). |
| `POST` | `/api/v1/policies/{revision}/access` | Permissions of an identity, with `rbac.engine: opa` ([simulation](permessi.md#simulation-and-impact)). |
| `GET` | `/api/v1/policies/{revision}/impact?since=30d` | Decisions of the real deploys recomputed. |
| `POST` | `/api/v1/policies/{revision}/approve` | Approve a draft. |
| `POST` | `/api/v1/policies/{revision}/activate` | Back to a previous revision. |
| `POST` | `/api/v1/freezes` | Create a freeze at runtime. |
| `DELETE` | `/api/v1/freezes/{name}` | Delete a freeze created at runtime. |
