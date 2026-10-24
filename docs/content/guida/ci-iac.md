# CI and infrastructure as code

manent is used from a pipeline without writing API calls: the client has options for CI, the repository contains a GitHub Action and a GitLab template, and a Terraform provider keeps the release in service equal to the configuration.

| Tool | Where | What it does |
|---|---|---|
| `manent deploy --ci` | client | Outcome as `key=value` lines for GitHub or GitLab, job summary, version and source from the pipeline. |
| `manent env validate` | client | Checks a candidate environment configuration before applying it. |
| GitHub Action | `integrations/github` | Installs the client and deploys, validates a descriptor or a configuration. |
| GitLab template | `integrations/gitlab/manent.yml` | Deploy job with a dotenv report. |
| Terraform provider | `integrations/terraform` | `manent_deployment`, `manent_inventory`, `manent_environment_check`, `manent_tenants`. |

## The client in CI

The descriptor stays in the repository; the pipeline adds what only it knows:

```sh
manent deploy -f deploy/prod.yml --version "$TAG" --source "https://nexus.example.com/shop-$TAG.tgz" --id "ci-$RUN_ID" --ci
```

| Option | Effect |
|---|---|
| `--version`, `--source`, `--sha256` | Replace the artifact fields of the descriptor. |
| `--id` | Deploy ID: with the same ID a re-run job does not deploy again (idempotency). |
| `--outputs <file>` | Appends `deployment_id`, `state`, `application`, `environment`, `version`, `dry_run`, `tenant` to the file as `key=value` lines, also when the deploy fails, and the [exports](catene.md#export-import) of a successful deploy as `export_<name>=<value>` (single-line values only); `config_revision` with the [git provider](../integrazioni/git.md). |
| `--summary <file>` | Appends a Markdown summary: version, deploy, outcome, rollout, rollback and error. |
| `--ci` | On GitHub Actions (`GITHUB_ACTIONS=true`) outcome in `$GITHUB_OUTPUT` and summary in `$GITHUB_STEP_SUMMARY`; on GitLab CI (`GITLAB_CI=true`) outcome in `manent.env` (or `$MANENT_DOTENV`). Outside a recognized CI it is an error. |

Server and token come from `MANENT_SERVER` and `MANENT_TOKEN` (a CI secret, with a [static token](../operazioni/accesso.md) of the group allowed to deploy). The command exits non-zero if the deploy does not succeed, so the job fails; with a required [approval](governance.md#approvazioni) it waits up to `--timeout`.

### Candidate environment configuration { #env-validate }

```sh
manent env validate -a shop -e prod -f envconfig/shop/shop/prod.yml [-d deploy/prod.yml]
```

The server merges the configuration with an example descriptor (default: the request of the deploy in service) and applies the enqueue-time checks: unknown fields, [descriptor policy](../operazioni/configurazione.md), merge, rollback, [wave](ondate.md) and [traffic](traffico.md) plan, plugins and health checks. It writes nothing: it is the check to run in the merge request of the configuration repository, before Consul or the file receive it. API: `POST /api/v1/environments/validate`, permission `deployment.read`.

## GitHub Action

```yaml
jobs:
  deploy:
    runs-on: ubuntu-latest
    environment: production
    steps:
      - uses: actions/checkout@v4
      - id: manent
        uses: manent-deployer/manent/integrations/github@v0.1.0
        with:
          server: https://manent.example.com
          token: ${{ secrets.MANENT_TOKEN }}
          descriptor: deploy/prod.yml
          version: ${{ github.ref_name }}
          id: gh-${{ github.run_id }}-${{ github.run_attempt }}
      - run: echo "deploy ${{ steps.manent.outputs.deployment-id }}: ${{ steps.manent.outputs.state }}"
```

| Input | Default | Meaning |
|---|---|---|
| `server`, `token` | | Server and token (required). |
| `command` | `deploy` | `deploy`, `validate` (descriptor) or `env-validate` (candidate configuration). |
| `descriptor` | | YAML descriptor; for `env-validate` it is the example descriptor, optional. |
| `version`, `source`, `sha256`, `id` | | As the client options. |
| `dry-run` | `false` | [Dry-run](dry-run.md) on a worker. |
| `wait`, `timeout` | `true`, `30m` | Waiting for the end. |
| `after` | | IDs of deploys that must succeed first, space-separated. |
| `application`, `environment`, `env-config` | | For `env-validate`. |
| `ca-file` | | Private CA of the server. |

Outputs: `deployment-id`, `state`, `version`; the job summary appears on the workflow page. If the client is not on the `PATH`, the action builds it from its own checkout (same version as the action) with `actions/setup-go`.

## GitLab template

```yaml
include:
  - remote: https://raw.githubusercontent.com/manent-deployer/manent/v0.1.0/integrations/gitlab/manent.yml
    inputs:
      descriptor: deploy/prod.yml
      version: $CI_COMMIT_TAG
      id: gl-$CI_PIPELINE_ID
      manent_ref: v0.1.0
```

The inputs are `job`, `stage`, `descriptor`, `version`, `source`, `sha256`, `id`, `dry_run`, `timeout`, `image` (default `golang:1.27-alpine`: the client is installed with `go install` if missing) and `manent_ref`. `MANENT_SERVER` and `MANENT_TOKEN` are CI/CD variables of the project (the token masked and protected). The dotenv report `manent.env` makes `deployment_id`, `state` and `version` variables of the following jobs, also when the deploy fails.

## Terraform provider

The provider is in `integrations/terraform` (a separate Go module: it adds no dependencies to manent) and is built with `make terraform-provider`. Until it is published in the registry it is used with `dev_overrides` in the Terraform CLI configuration file:

```hcl
provider_installation {
  dev_overrides { "manent-deployer/manent" = "/opt/terraform/providers" }   # directory with terraform-provider-manent
  direct {}
}
```

```hcl
terraform {
  required_providers {
    manent = { source = "manent-deployer/manent" }
  }
}

provider "manent" {}   # server and token from MANENT_SERVER and MANENT_TOKEN (or server, token, token_file, ca_file)

resource "manent_deployment" "shop" {
  application          = "shop"
  environment          = "prod"
  descriptor           = file("deploy/prod.yml")
  version              = var.shop_version
  source               = "https://nexus.example.com/shop-${var.shop_version}.tgz"
  timeout              = "30m"
  redeploy_out_of_sync = true
}
```

### `manent_deployment`

The resource is the **release in service** of an application in an environment.

- **Create and update** run a deploy with the descriptor and the attributes (`version` is required, `source` and `sha256` optional) and wait for its end within `timeout`, approvals included. Every change is a new deploy: the descriptor `id` is ignored. A failed deploy fails the apply; with automatic rollback the environment goes back to the previous version and the next plan tries again.
- **Read** reads the [inventory](governance.md#inventario): if someone released or rolled back another version outside Terraform, the plan shows the difference (`"1.0.0" -> "1.1.0"`) and the apply releases the configured one again.
- With `redeploy_out_of_sync = true`, an environment with targets changed outside manent according to the [reconciliation](governance.md#stato-reale) (`out_of_sync`) shows in the plan as `"1.1.0 (target fuori sincronia)" -> "1.1.0"`, and the apply releases it again.
- **Destroy** does not touch the targets: manent does not remove releases, the resource only leaves the state (with a warning).
- **Import**: `terraform import manent_deployment.shop shop/prod`.
- Computed attributes: `id` (deploy in service), `state`, `out_of_sync`.

### `manent_environment_check`

Checks a candidate environment configuration as `manent env validate` does: if it is not valid the **plan fails**, before another resource writes it into the configuration system.

```hcl
data "manent_environment_check" "shop_prod" {
  application = "shop"
  environment = "prod"
  config      = local.shop_prod_config        # YAML or JSON
}

resource "consul_keys" "shop_prod" {
  key {
    path  = "manent/shop/shop/prod"
    value = data.manent_environment_check.shop_prod.valid ? local.shop_prod_config : null
  }
}
```

Computed attributes: `valid`, `plugin`, `targets` (hosts of the effective deploy), `stages` (rollout stages) and `warnings`.

### `manent_inventory`

```hcl
data "manent_inventory" "prod" { environment = "prod" }
# data.manent_inventory.prod.entries[*]: application, artifact, environment, tenant, version, deployment_id, sha256, out_of_sync
```

### `manent_tenants`

`manent_tenants` (`application`, `selector`) returns `names` and `tenants` (name, tags, owned applications): the teams of the [tenant registry](tenant.md).

!!! note "What stays out"
    Permissions are in the manent configuration (`manent.yml`), read at startup, and so are the YAML [governance](governance.md) rules; the Rego policies and the runtime freezes are managed with `manent policy` and `manent freeze`, or come from a bundle or a directory versioned elsewhere ([Policies in Rego](policy.md)). The environment configuration stays in its system (Consul, files in the repository): the provider checks it, `consul_keys` or the repository write it.
