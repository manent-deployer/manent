# Tenants

A **tenant** is an internal team that owns a set of applications. Every application belongs to exactly one tenant, which lists it in the registry with its **tags** (product, criticality, …). The tenant is the scope of permissions, queues and restrictions; the tags group and select the applications across teams. There is no hierarchy: one owner per application, free tags on tenants and applications.

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
  platform:
    applications:
      infra: {}
    disabled: false                          # a disabled tenant cannot deploy
```

```yaml title="manent.yml"
tenants:
  file: /etc/manent/tenants.yml
  cache: 30s           # how long the registry is kept before reading it again
```

- **Ownership:** an application listed by two tenants makes the whole registry invalid.
- **Registered applications only:** with a registry configured, the deploy of an application that no tenant lists is refused. Without a registry every application is accepted, without owner.
- **Tags in the registry only:** the descriptor cannot set tenant or tags, otherwise a deploy could put itself in another team's scope.
- **Names:** tenants use lowercase letters, digits and `-` (at most 40); applications follow the rule of `artifact.name`. `application`, `tenant` and the `tenant.` prefix are reserved in application tags, `name` in tenant tags.
- **Revision:** the `config` step records the tenant of the deploy; the registry source and its revision (a prefix of the file's sha256) are in `manent tenants show`.

```sh
manent tenants list [-a <application>] [--selector "division=commerce"]
manent tenants show payments        # tags, applications with their tags, queue
```

## Is a tenant required to deploy?

It depends on the configuration of the server:

| Configuration | Deploy of an application |
|---|---|
| No registry (`tenants.file` empty, the default) | Allowed for every application, which has no owner. Only `rbac.bindings` and the governance rules apply. |
| Registry configured | Allowed only if a tenant lists the application. Otherwise it is refused at submission, before being queued: `application <name> is not registered in any tenant`. |
| Registry configured, owner tenant with `disabled: true` | Refused: `tenant <name>, owner of <application>, is disabled`. |

To onboard a new application with a registry:

1. Add it under its tenant in `tenants.yml`, with its tags.
2. Wait up to `tenants.cache` (default `30s`) for API and workers to read the registry again.
3. Deploy: the owner is recorded automatically, the descriptor never sets it.

An application listed by two tenants makes the whole registry invalid. Keep the registry under version control and review it like the rest of the configuration.

## The tenant of a deploy

The tenant is not chosen: it comes from the application. At submission the engine reads the owner from the registry and records it in the request (`tenant` attribute, `Status.tenant`, field `tenant` of callbacks and chat messages, `tenant` in the result of the `config` step). The deploy identity stays application/artifact/environment.

```sh
manent list -t payments              # deploys of the applications of payments
manent inventory -t payments
manent dora -t payments --by-tenant
```

## Selectors on tags

Application selectors read the application tags, `application`, `tenant` and the tenant tags with the `tenant.` prefix: `criticality=high`, `tenant=payments`, `tenant.division in (commerce)`. Tenant selectors (`tenants`) read the tenant tags and `name`.

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
    - {groups: [sre], role: approver, applications: ["*"], environments: [prod], selector: "criticality=high"}
governance:
  rules:
    - {name: critical, applications: ["*"], environments: [prod], selector: "criticality=high", approvals: {required: 2}}
```

The selectors of permissions can read only the keys of `rbac.selector_keys` (empty: no selector in permissions): a cosmetic tag must not give access.

## Members, admins and visibility

```yaml title="manent.yml"
rbac:
  tenant_roles: {members: deployer, admins: admin}   # default; an empty role gives nothing
  cross_tenant_inventory: true                       # default
```

- **Members and admins** of a tenant receive the role of `tenant_roles` on the tenant's applications, in every environment. The implicit roles never give `policy.*`, decided only by `rbac.bindings`.
- **Collaborators** of other teams get explicit bindings (`tenants: "name=payments"`).
- **Across tenants** whoever belongs to a tenant can read the inventory of the others (`cross_tenant_inventory`); everything else needs a permission.
- **Platform roles** (`platform.read`, `policy.*`, break-glass) stay across tenants.

## Isolation between tenants

- **Deploy IDs:** an ID already used by another tenant is refused (`deployment id … is already in use`), without saying whose it is.
- **Dependencies:** `after` towards a deploy of another tenant is refused, unless the deploy is the producer of an import; across tenants imports read only the exports with `public: true`.
- **Releases:** a composed release contains the applications of one tenant.
- **Restrictions:** `secret_refs`, `artifact_sources` and `callback_hosts` of the tenant are checked at submission and again by the `config` step on the effective descriptor (the restrictions are recorded in the request).

```yaml title="exports of a deploy"
exports:
  vpc_id: {from: deploy.vpc_id, pattern: 'vpc-[0-9a-f]+', public: true}   # readable by other tenants
  admin_host: {from: deploy.admin, pattern: '[a-z0-9.-]+'}                # only the owner's deploys
```

## Queues and workers per tenant

A tenant with `queue` runs its deploys on that queue; the others use the default queue (`dbos.queue`). Every worker registers the queues of the registry at startup and, by default, serves every tenant. A pool dedicated to some teams, with their credentials in its own `manent.yml`, listens only to their queues:

```yaml title="manent.yml of the payments workers"
dbos:
  tenants: [payments]      # only deployments.payments (the default queue for tenants without one)
plugins:
  ecs: {role_arn: "arn:aws:iam::111111111111:role/payments-deployer"}
```

- `concurrency` of the tenant is the global limit of its queue (quota); `environment_concurrency` still applies per application artifact and environment.
- With region routing the passes of a tenant's deploy run on `<queue>.<region>`.
- A queue added to the registry is registered by the workers at their next start: until then its deploys stay queued.
- Approvals, dependency waits and releases stay on the shared approvals queue: they run no plugin code.
