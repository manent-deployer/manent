# Tenants

A **tenant** is an internal team that owns a set of applications. The tenant lists its applications in the registry with their **tags** (product, criticality, …) and is the scope of permissions, queues and restrictions. The identity of a release is **tenant/application/artifact/environment**: two teams can each have an application called `shop`, and the applications no tenant lists belong to the reserved tenant `untenanted`.

## Registry

The registry is a single YAML file, `tenants.yml`:

```yaml title="tenants.yml"
tenants:
  payments:
    description: Payments team
    tags: {division: commerce, cost_center: "4410"}
    applications:
      shop: {tags: {product: checkout, criticality: high}}
      billing: {tags: {product: invoicing}}
    members: {groups: [payments-eng]}       # implicit role rbac.tenant_roles.members
    admins: {subjects: ["oidc:alice"]}       # implicit role rbac.tenant_roles.admins
    queue: deployments.payments              # own deploy queue (optional)
    concurrency: 2                           # quota of the queue (optional)
    secret_refs: ["vault:secret/payments/*"] # allowed secret references (optional)
    artifact_sources: ["oci://registry.example.com/payments/"]   # allowed sources (optional)
    callback_hosts: [hooks.payments.example.com]                 # allowed callback hosts (optional)
  retail:
    applications:
      shop: {tags: {product: storefront}}   # another shop: the release retail/shop
    disabled: false                          # a disabled tenant cannot deploy
```

```yaml title="manent.yml"
tenants:
  file: /etc/manent/tenants.yml
  cache: 30s           # how long the registry is kept before reading it again
  untenanted: allow    # allow (default) | reject: the applications no tenant lists
```

- **Names:** tenants use lowercase letters, digits and `-` (at most 40); `untenanted` is reserved. Applications follow the rule of `artifact.name`. In application tags `application`, `tenant` and the `tenant.` prefix are reserved, in tenant tags `name`.
- **Same name in several tenants:** allowed. Each pair tenant/application is a release of its own, with its history, inventory, configuration and permissions.
- **Tags in the registry only:** the descriptor cannot add tags, and its `tenant` field only chooses among the tenants that list the application.
- **Invalid registry:** if `tenants.yml` becomes unreadable or invalid while the servers run, the last valid registry stays in force. manent logs a warning (also to the SIEM) and `/readyz` answers 503 until the file is fixed.
- **Revision:** the registry source and its revision (a prefix of the file's sha256) are in `manent tenants show`.

```sh
manent tenants list [-a <application>] [--selector "division=commerce"]
manent tenants show payments        # tags, applications with their tags, queue
```

## The tenant of a deploy

The tenant comes from the registry. The `tenant` field of the descriptor (or `--tenant`) is needed only when several tenants list the application:

| Case | Tenant of the deploy |
|---|---|
| One tenant lists the application, no `tenant` | That tenant |
| Several tenants list it, no `tenant` | Refused: the error lists the tenants |
| `tenant` given and that tenant lists the application | That tenant |
| `tenant` given and that tenant does not list it | Refused |
| No tenant lists the application | `untenanted`, or refused with `tenants.untenanted: reject` |
| The tenant is disabled | Refused, never `untenanted` |

```yaml title="deploy.yml"
application: shop
tenant: retail          # required only because shop is in two tenants
environment: prod
```

The resolved tenant is recorded in the request and in the descriptor of the deploy (`tenant` attribute, `Status.tenant`, field `tenant` of callbacks and chat messages, CI outputs). Promotions and rollbacks stay in its identity. The same choice applies to `rollback`, `promote`, `env validate` and `policy access` with `--tenant` (`-t`), and to `imports.<name>.current.tenant`.

```sh
manent deploy -f deploy.yml --tenant retail
manent rollback -a shop -e prod --tenant payments
manent list -t payments              # deploys of the applications of payments
manent inventory -t payments
```

Without a registry every application is `untenanted`: nothing changes for those who do not use tenants.

## Is a tenant required to deploy?

| Configuration | Deploy of an application no tenant lists |
|---|---|
| No registry (`tenants.file` empty, the default) | Allowed, as `untenanted`. |
| Registry, `tenants.untenanted: allow` (default) | Allowed, as `untenanted`: applications of tenants and applications without coexist. |
| Registry, `tenants.untenanted: reject` | Refused at submission: `application <name> is not registered in any tenant`. |

An `untenanted` application has no queue, restrictions or implicit roles: only explicit bindings with patterns without prefix (`applications: [shop]`) give access to it.

## Environment configuration

The default templates contain `{tenant}`, so two applications with the same name read different documents:

| Provider | Default template |
|---|---|
| `file` | `./envconfig/{tenant}/{application}/{artifact}/{environment}.yml` |
| `consul` | `manent/{tenant}/{application}/{artifact}/{environment}` |
| `git` | `{tenant}/{application}/{artifact}/{environment}.yml` |

The applications without tenant use the segment `untenanted`. `{tenant}` is also available in `descriptor_policy.secret_refs`. A template without `{tenant}` stays valid if your application names are unique.

## Qualified application patterns

In bindings, permission policy grants, governance rules, freezes and notification routes, application patterns are **qualified**:

| Pattern | Applies to |
|---|---|
| `shop`, `uc-*`, `*` | Only the `untenanted` applications |
| `payments/shop`, `payments/*` | The applications of `payments` |
| `*/shop` | Every `shop`, of any tenant |
| `*/*` | Every application |

A wide binding written for the applications without tenant (`applications: ["*"]`) therefore never reaches a tenant's application. Without a registry everything is `untenanted`, so `"*"` keeps covering every application.

```yaml title="manent.yml"
rbac:
  bindings:
    - {groups: [platform], role: admin, applications: ["*/*"], environments: ["*"]}
    - {groups: [retail-ci], role: deployer, applications: ["retail/*"], environments: ["*"]}
governance:
  rules:
    - {name: critical, applications: ["*/*"], environments: [prod], selector: "criticality=high", approvals: {required: 2}}
  freezes:
    - {name: retail-close, applications: ["retail/*"], environments: [prod], from: 2026-12-20T00:00:00Z, to: 2027-01-07T00:00:00Z}
```

If a deployer of `payments` names `tenant: retail` by mistake, the deploy is refused with 403 unless they have an explicit permission on `retail`'s application: the implicit roles cover only their own tenant, and patterns without prefix only the `untenanted` applications.

## Selectors on tags

Application selectors read the application tags, `application`, `tenant` and the tenant tags with the `tenant.` prefix: `criticality=high`, `tenant=payments`, `tenant=untenanted`, `tenant.division in (commerce)`. Tenant selectors (`tenants`) read the tenant tags and `name`.

| Where | Field |
|---|---|
| RBAC bindings and permission grants | `tenants` (owner) and `selector` (applications) |
| Governance rules and freezes | `tenants` and `selector`; in Rego `input.tenant` and `input.application_tags` |
| Listings | `manent inventory --selector …`, `manent list --selector …`, query `selector` of the API |
| Runtime freezes | `manent freeze add … --selector criticality=high` |

```yaml title="manent.yml"
rbac:
  selector_keys: [criticality, product, tenant]   # keys the selectors of permissions can read
  bindings:
    - {groups: [sre], role: approver, applications: ["*/*"], environments: [prod], selector: "criticality=high"}
```

The selectors of permissions can read only the keys of `rbac.selector_keys` (empty: no selector in permissions): a cosmetic tag must not give access.

## Members, admins and visibility

```yaml title="manent.yml"
rbac:
  tenant_roles: {members: deployer, admins: admin}   # default; an empty role gives nothing
  cross_tenant_inventory: true                       # default
```

- **Members and admins** of a tenant receive the role of `tenant_roles` on the tenant's applications (`<tenant>/*`), in every environment. The implicit roles never give `policy.*`, decided only by `rbac.bindings`, and never cover the `untenanted` applications.
- **Collaborators** of other teams get explicit bindings (`applications: ["payments/*"]` or `tenants: "name=payments"`).
- **Across tenants** whoever belongs to a tenant can read the inventory of the others (`cross_tenant_inventory`); everything else needs a permission.
- **Platform roles** (`platform.read`, `policy.*`, break-glass) stay across tenants.

## Isolation between tenants

`untenanted` counts as a tenant of its own.

- **Deploy IDs:** an ID already used by another tenant is refused (`deployment id … is already in use`), without saying whose it is.
- **Dependencies:** `after` towards a deploy of another tenant is refused, unless the deploy is the producer of an import; across tenants imports read only the exports with `public: true`.
- **Releases:** a composed release contains the applications of one tenant.
- **Restrictions:** `secret_refs`, `artifact_sources` and `callback_hosts` of the tenant are checked at submission and again by the `config` step on the effective descriptor (the restrictions are recorded in the request).

```yaml title="exports of a deploy"
exports:
  vpc_id: {from: deploy.vpc_id, pattern: 'vpc-[0-9a-f]+', public: true}   # readable by other tenants
  admin_host: {from: deploy.admin, pattern: '[a-z0-9.-]+'}                # only the owner's deploys
```

## Moving an application to another tenant

When an `untenanted` application joins a tenant, or passes from one tenant to another, its identity changes. To keep history, inventory and rollbacks, move its history after updating the registry:

```sh
manent-server -c manent.yml admin move-application -a legacy --from untenanted --to retail [--dry-run]
```

It changes the `tenant` attribute of the ended deploys (and of their approvals and waits); workflows not ended are listed in `pending`. Move the environment configuration too (for example from `untenanted/legacy/…` to `retail/legacy/…`). `admin rename-artifact` takes `--tenant` for the applications of a tenant.

## Queues and workers per tenant

A tenant with `queue` runs its deploys on that queue; the others, `untenanted` included, use the default queue (`dbos.queue`). Every worker registers the queues of the registry at startup and, by default, serves every tenant. A pool dedicated to some teams, with their credentials in its own `manent.yml`, listens only to their queues:

```yaml title="manent.yml of the payments workers"
dbos:
  tenants: [payments]      # only deployments.payments (the default queue for tenants without one)
plugins:
  ecs: {role_arn: "arn:aws:iam::111111111111:role/payments-deployer"}
```

- `concurrency` of the tenant is the global limit of its queue (quota); `environment_concurrency` still applies per release (tenant, application, artifact, environment).
- With region routing the passes of a tenant's deploy run on `<queue>.<region>`.
- A queue added to the registry is registered by the workers at their next start: until then its deploys stay queued.
- Approvals, dependency waits and releases stay on the shared approvals queue: they run no plugin code.
