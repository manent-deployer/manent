# Applications with several artifacts

An application is often made of several artifacts released separately: a frontend, a backend, the database migrations. With the **artifact identity** every artifact is a release of its own: environment configuration, queue, inventory, rollback, promotion and retention are per application, artifact and environment. The artifact is `artifact.name`, already required in every descriptor: there is no new field.

```yaml
# backend
application: shop
environment: prod
artifact: {type: war, name: shop-api, version: 1.5.0, source: https://repo.example.com/shop-api-1.5.0.war}
---
# frontend
application: shop
environment: prod
artifact: {type: tgz, name: shop-web, version: 3.2.0, source: https://repo.example.com/shop-web-3.2.0.tgz}
```

## Turning it on

```yaml title="manent.yml"
deploy:
  identity: artifact        # application (default) | artifact
```

| | `application` (default) | `artifact` |
|---|---|---|
| Release in service (inventory) | one per application/environment | one per application/artifact/environment |
| Queue partition (`environment_concurrency`) | `<app>/<env>` | `<app>+<artifact>/<env>`: different artifacts deploy in parallel |
| Rollback, promotion, `requires` | of the application | of the artifact |
| Retention `keep_releases` | per application/environment | per artifact |
| Default schedule name | `<app>-<env>` | `<app>-<artifact>-<env>` |
| `artifact.name` | any (a warning if not valid) | letters, digits, `.`, `_`, `:`, `-` (an image path goes in `source`) |

The order between artifacts (migrations, then backend, then frontend) is expressed with [`after` or a composed release](catene.md).

!!! warning "Change it with the workers drained"
    The deploys already enqueued keep the old queue partition: for a moment a deploy enqueued before and one enqueued after the change could run together in the same environment. Change `deploy.identity` with the workers drained (`Worker.Drain`, `/readyz` 503) and set the same value on API and workers.

The history needs no migration: every workflow already records the artifact name, and the inventory is rebuilt per artifact from it.

## Environment configuration per artifact

The templates of the providers accept `{artifact}`: one document per artifact, with nothing shared (common targets and secrets are repeated in every document).

```yaml title="manent.yml"
envconfig:
  provider: file
  required: true                                   # a missing document is an error: it catches a misspelled artifact
  file:
    path: "envconfig/{application}/{artifact}/{environment}.yml"
    legacy_path: "envconfig/{application}/{environment}.yml"   # only during the transition
  # consul:
  #   key: "manent/{application}/{artifact}/{environment}"
  #   legacy_key: "manent/{application}/{environment}"
```

```text
envconfig/
  shop/
    shop-api/prod.yml
    shop-web/prod.yml
    shop-db/prod.yml
```

- **Transition:** with `legacy_path`/`legacy_key` an artifact without its own document reads the application's one; the `config` step records `legacy: true` in its result. Remove them when every artifact has its document.
- **Names:** with `{artifact}` in the template an `artifact.name` that cannot be part of a path or key (e.g. `team/api`) is a permanent error.
- **Templates without `{artifact}`** keep working as before: one document per application/environment, whatever the identity.
- **`descriptor_policy.secret_refs`** accepts `{artifact}`: `vault:secret/{application}/{artifact}/{environment}/*`.
- **`manent env validate --artifact <name>`** checks a candidate document of an artifact.

## Rollback, promotion and inventory

```sh
manent inventory -a shop                              # one entry per artifact
manent rollback -a shop -e prod --artifact shop-api   # -A for short
manent promote -a shop --from staging --to prod -A shop-web
manent list -a shop -A shop-db
manent dora -a shop -A shop-api
```

With a single artifact in the environment `--artifact` can be omitted; with more than one the request is refused and the error lists them. In a composed release with `onFailure: rollback` every deploy is rolled back on its own artifact. Imports of the release in service (`imports.<name>.current`) take `artifact` too, required when the application has more than one artifact in that environment.

## Permissions and governance per artifact

Bindings, rules and freezes accept an optional list of patterns on `artifact.name`:

```yaml title="manent.yml"
rbac:
  bindings:
    - groups: [web-team]
      role: deployer
      applications: [shop]
      environments: ["*"]
      artifacts: ["shop-web*"]       # only the deploys of these artifacts
governance:
  rules:
    - name: migrations
      applications: ["*"]
      environments: [prod]
      artifacts: ["*-db"]
      approvals: {required: 2}
  freezes:
    - name: frontend-freeze
      applications: [shop]
      environments: [prod]
      artifacts: [shop-web]
      from: 2026-12-20T00:00:00Z
      to: 2027-01-07T00:00:00Z
```

- **Bindings with `artifacts`** apply only to the deploys of those artifacts, never to the actions on the whole environment (freezes, schedules listing, tenants): those need a binding without `artifacts`. The same field is accepted in the grants of a [permission policy](permessi.md) (`data.manent.rbac.bindings`, `grants`).
- **Rules with `artifacts`** apply only to those artifacts; in Rego the library reads `input.artifact.name`.
- **Freezes with `artifacts`** block only those artifacts; where the artifact is unknown (e.g. custom steps checking the environment) a partial freeze still applies. `manent freeze add -A shop-web` creates one at runtime.

## Tenants

Parameters, secrets and custom step parameters of a [tenant](tenant.md) can be given for one artifact with the key `<application>/<artifact>`; they take priority over the application's:

```yaml title="envconfig/tenants/acme.yml"
params:
  shop: {db_host: acme-db.internal}
  shop/shop-db: {db_host: acme-db-admin.internal}
```

## Renaming an artifact

A new `artifact.name` is a new release, without history. To keep it, rename the artifact in the history:

```sh
manent-server -c manent.yml admin rename-artifact -a shop --from api --to shop-api [--dry-run]
```

It changes the `artifact` attribute of the ended deploys (and of their approvals and waits): inventory, rollbacks and promotions follow the new name, and a rollback releases the old version under the new name. Workflows not ended are listed in `pending` and left unchanged: run the command again when they end. The API processes rebuild their inventory within 10 minutes. Schedules keep the descriptor they were created with.
