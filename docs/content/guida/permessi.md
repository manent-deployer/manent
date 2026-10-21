# Permissions as a policy

By default the permissions are the `rbac.bindings` of `manent.yml` ([Permissions](../operazioni/accesso.md#permessi-rbac)). With `rbac.engine: opa` they are decided by the same Rego revisions as the [governance policy](policy.md): pushed as drafts, tested, approved by a second person, activated without restarting the servers and recorded in the audit.

A policy can express what bindings cannot:

- **temporary access**: an on-call grant that stops applying at the end of the shift (`until`);
- **ownership**: a team gets the applications of the tenants tagged `owner=<team>`, without a binding per application;
- **conditions on the request**: contractors never change production, migrations in production only by the DBA group, a deploy cancelled only by whoever requested it.

```yaml title="manent.yml"
governance:
  engine: opa                 # required: the two policies share revisions and lifecycle
auth:
  oidc:
    policy_claims: [email, name, employee_type]   # claims the policy sees (default [email, name])
rbac:
  engine: opa                 # builtin (default) | opa
  opa:
    cache: 30s                # how long the grants of an identity are reused
    cache_size: 10000         # identities in the cache
  bindings:                   # policy.* always from here
    - {groups: [platform-release], role: admin, applications: ["*"], environments: ["*"]}
```

With `rbac.engine: opa` and no Rego written, nothing changes: a revision without `package manent.authz` receives a default module that applies `rbac.bindings` exactly as the `builtin` engine (a test checks the equivalence).

## Two parts: grants and denials

```text
data.manent.authz.grants  →  the permissions of an identity (array or set of objects)
data.manent.authz.deny    →  reasons that refuse a change (strings or {code, message})
```

- **Grants** are computed once per identity and kept in a cache for `rbac.opa.cache`: reads, lists, the web interface and `GET /me` never evaluate Rego.
- **Denials** are evaluated only for the actions that change something, after the grants allowed them: `deployment.submit`, `deployment.dry-run`, `deployment.cancel`, `deployment.rollback`, `deployment.approve`, `deployment.freeze-override`, `schedule.write`, `freeze.write`.

### Grants

Input:

```json
{
  "schema": 1,
  "now": "2026-10-20T09:00:00Z",
  "identity": {
    "subject": "oidc:alice", "method": "oidc", "name": "alice",
    "groups": ["team-shop", "oncall"],
    "claims": {"email": "alice@example.com", "employee_type": "contractor"}
  }
}
```

`claims` holds only the claims listed in `auth.oidc.policy_claims`: tokens can carry other personal data the policy must not see. Static tokens have `method: token`, their groups and no claims.

Output, one object per grant:

| Field | Notes |
|---|---|
| `name` | Label shown in `manent whoami`, in the web interface and in the audit. |
| `role` or `actions` | A role (predefined or from `rbac.roles`/`data.manent.rbac.roles`) or a list of actions. `policy.*` actions are **discarded** with a warning: only `rbac.bindings` grants them. |
| `applications`, `environments` | Patterns with `*`, required. |
| `tenants` | Selector on the tags and the name of the owner [tenant](tenant.md); absent: every application. |
| `artifacts` | Patterns on `artifact.name`; absent: every artifact. |
| `selector` | Application selector on the [tags](tenant.md), with the keys of `rbac.selector_keys`; absent: every application. |
| `until` | Optional, RFC 3339: the grant stops applying at that instant, even while it is in the cache. The instant is compared with the clock of the API server. |

An invalid grant (unknown role or action, missing patterns, invalid selector) is discarded on its own, with a warning in the log and in `manent_authz_invalid_grants_total`; at compilation, on the sample inputs, it is an error.

### Denials

Input:

```json
{
  "schema": 1,
  "now": "2026-10-20T09:00:00Z",
  "identity": {"subject": "oidc:alice", "method": "oidc", "name": "alice", "groups": ["team-shop"], "claims": {}},
  "action": "deployment.submit",
  "resource": {
    "application": "shop",
    "environment": "prod",
    "tenant": {"name": "payments", "tags": {"division": "commerce", "owner": "team-shop"}},
    "deployment": {
      "id": "shop-1.4.0-prod", "kind": "deploy", "dry_run": false,
      "artifact": {"type": "application", "name": "shop", "version": "1.4.0"},
      "requested_by": "oidc:alice"
    }
  }
}
```

`resource.deployment` is the request for a submission and the existing deploy for cancellation, rollback and approval; it is absent for the actions without a deploy (schedules, freezes). The input never contains secrets or plugin parameters.

A denial wins over every grant. The `403` response and the audit entry carry the reasons and the revision:

```text
permission denied: oidc:alice cannot perform deployment.submit on shop/prod: contractors cannot change production (policy 14:3f2a9c1e5b7d)
```

## Example

```rego title="authz.rego"
package manent.authz

import rego.v1

import data.manent.lib.authz

# The bindings of manent.yml (data.manent.rbac), plus the rules below.
grants := authz.bindings | owners | oncall

# A team owns the applications of the tenants tagged with its name.
owners contains g if {
	some group in input.identity.groups
	startswith(group, "team-")
	g := {
		"name": sprintf("owner %s", [group]),
		"role": "deployer",
		"applications": ["*"],
		"environments": ["*"],
		"tenants": sprintf("owner=%s", [group]),
	}
}

# On-call in production until the end of the shift (oncall/data.json).
oncall contains g if {
	some shift in data.oncall.shifts
	shift.subject == input.identity.subject
	g := {"name": "oncall", "role": "operator", "applications": ["*"], "environments": ["prod"], "until": shift.until}
}

# Contractors never change production.
deny contains "contractors cannot change production" if {
	input.identity.claims.employee_type == "contractor"
	startswith(input.resource.environment, "prod")
}

# Database migrations in production: DBA only.
deny contains {"code": "dba", "message": "migrations in production are released by the DBA group"} if {
	input.action == "deployment.submit"
	input.resource.deployment.artifact.type == "migration"
	startswith(input.resource.environment, "prod")
	not "dba" in input.identity.groups
}
```

`data.manent.lib.authz` provides `bindings` (the grants of `data.manent.rbac.bindings` for the identity of the input) and `binding_matches(binding, identity)`. The restrictions are those of the governance policy: Rego v1, no non-deterministic built-ins, no `http.send`. Tests `*_test.rego` cover `manent.authz` too.

### Starting from the bindings

```sh
manent-server -c manent.yml policy export-rbac -d policy/
manent-server -c manent.yml policy test -d policy/
manent policy push -d policy/ -m "permissions from manent.yml"
```

`export-rbac` writes `manent/rbac/data.json` (the bindings as `data.manent.rbac`, which then no longer comes from `manent.yml`) and a starting `authz.rego` with commented examples. Existing files are kept unless `--force`.

## Separation of powers

| Action | Decided by |
|---|---|
| `policy.read`, `policy.write`, `policy.approve` | Only `rbac.bindings` in `manent.yml`, whatever the engine. |
| `manent-server admin …` (break-glass) | Whoever has access to the database, outside the permissions. |
| Every other action | The active revision. |

A revision cannot grant itself the power to change the policy, and a revision that removes every permission does not lock out whoever must fix it: the holders of the static `policy.*` can still push, approve and activate a new revision, or go back to a previous one. The minimal configuration is therefore one static binding for the platform release managers.

## Simulation and impact

Before approving a revision, simulate the identities it concerns. Nothing is read from the identity provider or stored: subject, groups and claims are the ones you type.

```sh
manent policy access 15 --subject oidc:carol --group team-shop --claim employee_type=contractor --compare
manent policy access 15 --subject oidc:carol --claim employee_type=contractor \
  --action deployment.submit -a shop -e prod [-t acme] [-f deploy.yml]
```

```text
Identità oidc:carol, revisione 15:9be1c04d2f11

PERMESSO          AZIONI    APPLICAZIONI  AMBIENTI  TENANT          FINO A
owner team-shop   deployer  *             *         owner=team-shop -

Rispetto alla revisione attiva 14:3f2a9c1e5b7d:
+ owner team-shop deployer  *             *         owner=team-shop -

Esito: negato dalla policy (permesso owner team-shop)
  - contractors cannot change production
```

`current` is the revision in force; `--json` prints the response of `POST /api/v1/policies/{revision}/access`. The same simulation is in the **Access** section of the Policy page of the [web interface](interfaccia-web.md). It requires `policy.write`.

## Cache and availability

| Situation | Effect |
|---|---|
| New revision activated | Applies at once: the cache is per revision. |
| Grant removed by the identity provider (group, claim) | Applies within `rbac.opa.cache` (default 30s). |
| `until` reached | Applies at once, even with the grant in the cache. |
| No revision loaded, or older than `governance.opa.max_staleness` | Every non-static action is refused with `503`; `policy.*` keep working so that a revision can be loaded. |
| Error evaluating the grants | No grant for that identity: everything denied, error in the log. |
| Error evaluating the denials | The change is refused (`403`, `policy evaluation failed`). |

Going back to `rbac.engine: builtin` only needs a restart: the bindings of `manent.yml` are always there.

## Audit and metrics

Changes allowed carry in the [audit](../operazioni/accesso.md#audit) `authz_revision` and the `grant` that allowed them; denied ones the revision and the reasons. `manent whoami` and `GET /api/v1/me` show `authz_engine`, `authz_revision` and the grants with `name` and `until`.

| Metric | Description |
|---|---|
| `manent_authz_grants_evaluations_total{outcome}` | Grants computed (`ok`, `cached`, `error`). |
| `manent_authz_deny_evaluations_total{action,outcome}` | Denials evaluated (`allowed`, `denied`, `error`). |
| `manent_authz_eval_duration_seconds{part}` | Duration of the evaluations (`grants`, `deny`). |
| `manent_authz_invalid_grants_total` | Grants discarded as invalid or with `policy.*` actions. |
| `manent_authz_cache_entries` | Identities in the cache. |
