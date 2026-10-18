# Tenants

A **tenant** is a customer or an instance: the same application is released in several copies, one per tenant, in every environment. Every tenant has its parameters, secrets and targets, its own deploys, inventory, rollbacks and approvals, and can receive the same release as the others with a single multi-tenant release, a group at a time.

The tenant is a third dimension next to application and environment, and it is optional: a deploy without a tenant behaves as before.

## Tenant registry

Every tenant is a document in the provider of the [environment configuration](configurazione-ambiente.md), next to the environments:

```yaml title="envconfig/tenants/acme.yml"
description: ACME S.p.A.
tags: {tier: gold, region: eu, contract: "2026"}
applications:
  shop: [staging, prod]            # environments the tenant is connected to
  billing: [prod]
params:
  shop:
    db_host: acme-db.internal      # values of the environment's tenant_params
secrets:
  shop:
    - {name: license, ref: "vault:secret/data/shop/acme#license"}
step_params:
  shop:
    migrate: {database: acme_shop}
disabled: false                    # a disabled tenant is never selected nor deployable
```

| Provider | Where | Configuration |
|---|---|---|
| `file` | `./envconfig/tenants/{tenant}.yml` | `envconfig.file.tenants` |
| `consul` | key `manent/tenants/{tenant}` | `envconfig.consul.tenants_key` |

- **Name:** lowercase letters, digits and `-`, at most 40 characters. It becomes part of IDs and directory names.
- **Tags:** key-value pairs, selected with the same selectors as the [waves](ondate.md) (`tier=gold, region in (eu, us)`); `name` selects by name and is reserved.
- **`applications`:** a tenant receives deploys only in the environments it is connected to.
- **Parameters and secrets:** per application, only secret references.
- **Reading:** the document is read again at the start of every deploy (step `config`, which records `tenant_source` and `tenant_revision`); the listing, used by selectors and permissions, is kept for `envconfig.tenants_cache` (default 30s).

```sh
manent tenants list [-a shop] [-e prod] [--selector "tier=gold"]
manent tenants show acme              # parameters and secret names, never the references
```

## Environment configuration

```yaml title="envconfig/shop/prod.yml"
plugin: kubernetes
tenancy: tenanted              # untenanted (default) | tenanted | both
tenant_params:
  db_host: {pattern: '[a-z0-9.-]+', required: true}
  feature_x: {pattern: 'true|false', default: "false"}
targets:
  - {host: k8s-eu-1, labels: {region: eu, tenant: "*"}}    # shared by every tenant
  - {host: k8s-acme, labels: {region: eu, tenant: acme}}   # dedicated
params:
  namespace: "shop-{tenant}"
secrets:
  - {name: db, ref: "vault:secret/data/shop/{tenant}#db"}
```

- **`tenancy`:**
    - `untenanted` refuses deploys with a tenant (the behaviour of every environment without the key);
    - `tenanted` requires one;
    - `both` accepts both, for example during a migration.
- **`tenant_params`:** the parameters every tenant provides, with an anchored `pattern`, `required` and `default`. A tenant that does not provide a required parameter, provides one out of pattern or provides one not declared refuses the deploy, at submission already. The environment decides what a tenant can vary: a tenant cannot set `post_command`.
- **Targets:**
    - a deploy of a tenant receives the targets with `tenant: <name>` plus those with `tenant: "*"`;
    - a deploy without tenant, those without the label plus the shared ones.
- **`{tenant}`** is replaced in parameters, secret references, custom step parameters and in the `secret_refs` of the `descriptor_policy`. An environment using it accepts only deploys with a tenant.

**Merge** happens in three layers and keeps the rule of the environment configuration: values in conflict are an error, never an override.

1. The **environment** is authoritative.
2. The **tenant** adds only the declared parameters, its secrets and custom step parameters.
3. The **descriptor** adds what the [descriptor policy](../operazioni/configurazione.md#descriptor_policy) allows, never a tenant parameter.

## A deploy for a tenant

```yaml
application: shop
environment: prod
tenant: acme
artifact: {type: kubernetes, name: shop, version: 1.4.0, source: oci://registry/shop:1.4.0}
```

```sh
manent deploy -f deploy.yml --tenant acme
```

The queue partition is `<application>/<environment>/<tenant>`: two deploys of acme never overlap, while acme and globex proceed in parallel (`dbos.environment_concurrency` applies per tenant). Status, inventory, DORA metrics, callback events and chat messages carry the tenant; `--tenant` (or `?tenant=`) filters `list`, `inventory`, `activity` and `dora` (`--by-tenant` for the metrics per tenant).

**Promotion and rollback** are per tenant:

```sh
manent promote -a shop --from staging --to prod --tenant acme
manent rollback -a shop -e prod --tenant acme [--to 1.3.2]
```

The mandatory promotion (`requires`) is checked per tenant: acme in prod requires acme in staging, not any tenant.

## Releasing to several tenants { #rilasciare-a-piu-tenant }

```yaml
application: shop
environment: prod
id: shop-1.4.0-prod
tenants:
  selector: "tier in (gold, silver), region=eu"   # or names: [acme, globex]
  groups:                                         # optional: tenant canary
    - {name: pilot, select: "tier=gold", bake: 30m}
    - {name: rest}
  maxParallel: 5
  onFailure: stop                                 # stop | continue
  onDenied: skip                                  # skip | reject
  approvalsBetween: [pilot]
artifact: {...}
```

```sh
manent deploy -f deploy.yml --tenants "tier=gold" [--max-parallel 5]
manent status shop-1.4.0-prod                     # state of every tenant
manent cancel shop-1.4.0-prod                     # tenants not yet ended
```

- **Selection:** `names` or `selector`; disabled tenants and those not connected to the application and environment are left out. The list is fixed at submission: a recovery never selects again.
- **One deploy per tenant**, with ID `<id>.<tenant>`, its approvals and its automatic rollback. The release is the `manent.tenants` workflow, on the approval queue: it takes no slot.
- **Groups:**
    - a group starts when the previous one has ended and its `bake` has passed;
    - a group without `select` takes the remaining tenants;
    - with `approvalsBetween` the release waits for an approval after the group (`manent approve <id>`), with the strictest rule among the tenants.
- **`maxParallel`** is the number of tenant deploys running at the same time (default 5).
- **`onFailure`:**
    - `stop` starts no other tenant after a failure (those running finish);
    - `continue` goes on with the others.
- **`onDenied`:**
    - with `skip` a tenant denied by the [governance policy](policy.md) is left out with the reason and the others proceed;
    - with `reject` everything is refused.
- **States** of a tenant: the deploy state, or `PENDING_START`, `SKIPPED`, `DENIED`. The release ends `succeeded`, `failed` or `stopped`.

**Recurring schedules** with `tenants` select the tenants again at every run: a tenant added after the schedule's creation enters the next runs. `schedule.at` delays the whole release.

**Group promotion and rollback:**

```sh
manent promote -a shop --from staging --to prod --tenants "tier=gold"   # each from its own release; the others are left out
manent rollback -a shop -e prod --run shop-1.4.0-prod                   # the successful tenants, in reverse order
```

The rollback of a release is a new multi-tenant release `<run>.rollback` with `onFailure: continue`: tenants with no previous release (first release) are left out.

## Governance

The tenant enters every governance decision:

- **YAML rules and freezes** take `tenants`, a selector:

  ```yaml
  governance:
    rules:
      - {name: gold-prod, applications: ["*"], environments: [prod], tenants: "tier=gold", approvals: {required: 2}}
    freezes:
      - {name: acme-close, applications: ["*"], environments: [prod], tenants: "name=acme", from: ..., to: ...}
  ```

  A rule or window without `tenants` applies to every deploy, one with `tenants` only to the matching tenants. `manent freeze add --tenants "tier=gold"` creates one at runtime.
- **Rego policies** receive `input.tenant` (`name`, `tags`) and `input.tenants_run` (input schema 2); `lib.tenant_matches(selector, input.tenant)` applies a selector. See [Policies in Rego](policy.md).

```rego
deny contains {"code": "contract", "message": "the 2025 contracts stay on 1.0.x"} if {
	input.tenant.tags.contract == "2025"
	not startswith(input.artifact.version, "1.0.")
}
```

## Permissions { #permessi }

RBAC bindings take `tenants`, a selector:

```yaml
rbac:
  bindings:
    - {groups: [support-acme], role: deployer, applications: ["*"], environments: [prod], tenants: "name=acme"}
    - {groups: [account-gold], role: approver, applications: ["*"], environments: [prod], tenants: "tier=gold"}
```

- A binding without `tenants` applies to every deploy, with or without tenant: existing configurations do not change.
- A binding with `tenants` applies only to the deploys of the matching tenants, with the tags read from the registry.
- A multi-tenant release requires the permission on **every** selected tenant; approving or cancelling one, on every tenant of the release.
- Lists (`list`, `inventory`, `activity`, `dora`, `tenants`) show only what can be read.

## From an environment per customer

Before tenants, a customer was often an environment (`prod-acme`). The adoption records the release in service there as a deploy of the tenant, with kind `adoption` and without touching the targets, so promotions and rollbacks of the tenant start from the right release:

```sh
manent-server -c manent.yml admin tenants-adopt -a shop --environment prod-acme --as prod --tenant acme
```

## Terraform

`manent_deployment` takes `tenant`, and the data source `manent_tenants` lists the tenants for a `for_each`:

```hcl
data "manent_tenants" "gold" {
  application = "shop"
  environment = "prod"
  selector    = "tier=gold"
}

resource "manent_deployment" "shop" {
  for_each    = toset(data.manent_tenants.gold.names)
  application = "shop"
  environment = "prod"
  tenant      = each.key
  descriptor  = file("deploy/prod.yml")
  version     = var.version
}
```

## Not covered

- An API to write the registry: the tenants live in the provider, managed with Git, Terraform or Consul.
- A single approval for a whole release (one per tenant deploy, plus those between groups).
- The `tenant` label on the Prometheus metrics, whose cardinality would grow with the tenants.
