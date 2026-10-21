# Terraform

The Terraform provider treats **the release in service** of an application as a resource. `terraform plan` shows when the version in an environment differs from the configured one, or when targets were changed outside manent; `terraform apply` releases it through manent, with all its rules. A data source checks a candidate environment configuration and **fails the plan** before another resource writes it to Consul.

!!! abstract "What you need"
    Terraform 1.x · Go, to build the provider until it is published in the registry · a manent token for the identity that runs Terraform

## 1. Build and install the provider

The provider is a separate Go module in `integrations/terraform`:

```sh
make terraform-provider                       # bin/terraform-provider-manent
sudo install -d /opt/terraform/providers
sudo install bin/terraform-provider-manent /opt/terraform/providers/
```

Until it is in the registry, point Terraform at it with `dev_overrides` in the CLI configuration:

```hcl title="~/.terraformrc"
provider_installation {
  dev_overrides { "joeg-ita/manent" = "/opt/terraform/providers" }
  direct {}
}
```

Terraform then prints a warning that the provider is overridden by a local build: that is expected.

## 2. Configure it

```hcl title="main.tf"
terraform {
  required_providers {
    manent = { source = "joeg-ita/manent" }
  }
}

provider "manent" {}   # MANENT_SERVER and MANENT_TOKEN, or server, token, token_file, ca_file
```

The identity of the token needs `deployment.read` to plan, and `deployment.submit` on the environments it releases to.

## 3. The release in service as a resource

```hcl title="shop.tf"
variable "shop_version" { type = string }

resource "manent_deployment" "shop_prod" {
  application          = "shop"
  environment          = "prod"
  descriptor           = file("${path.module}/deploy/prod.yml")
  version              = var.shop_version
  source               = "https://nexus.example.com/repository/releases/shop/shop-${var.shop_version}.tgz"
  timeout              = "30m"
  redeploy_out_of_sync = true
}
```

```sh
terraform plan -var shop_version=1.4.0
```

| Situation | Plan |
|---|---|
| Nothing changed | No changes. |
| `shop_version` changed | `version: "1.3.2" -> "1.4.0"`: apply runs a deploy and waits for it, approvals included, up to `timeout`. |
| Someone released or rolled back another version outside Terraform | The difference read from the [inventory](../guida/governance.md#inventario); apply releases the configured one again. |
| Targets changed outside manent (with [reconciliation](../guida/governance.md#stato-reale) and `redeploy_out_of_sync`) | `"1.4.0 (target fuori sincronia)" -> "1.4.0"`: apply releases it again. |

- A failed deploy fails the apply. With automatic rollback the environment is back on the previous version, and the next plan tries again.
- `terraform destroy` does not touch the targets: manent does not uninstall releases, the resource just leaves the state.
- An existing environment is imported with `terraform import manent_deployment.shop_prod shop/prod`.
- For [tenants](../guida/tenant.md#terraform), `manent_deployment` takes `tenant`, and the `manent_tenants` data source drives a `for_each` with one resource per tenant.

## 4. Gate configuration changes in the plan

When Terraform also writes the environment configuration to Consul, let manent check it first:

```hcl title="envconfig.tf"
locals {
  shop_prod_config = file("${path.module}/envconfig/shop/shop/prod.yml")
}

data "manent_environment_check" "shop_prod" {
  application = "shop"
  environment = "prod"
  config      = local.shop_prod_config
}

resource "consul_keys" "shop_prod" {
  key {
    path  = "manent/shop/shop/prod"
    value = data.manent_environment_check.shop_prod.valid ? local.shop_prod_config : null
  }
}
```

An invalid configuration (a merge conflict, a forbidden parameter, a wave that selects no target, an unknown plugin parameter) **fails the plan** with manent's error, before Consul is touched. The data source also returns `plugin`, `targets`, `stages` and `warnings`, useful in outputs or in policy checks.

## 5. Read the inventory

```hcl
data "manent_inventory" "prod" { environment = "prod" }

output "prod_versions" {
  value = { for e in data.manent_inventory.prod.entries : e.application => e.version }
}
```

## What stays out of Terraform

Permissions and YAML governance rules live in `manent.yml`; Rego policies and runtime freezes are managed with `manent policy` and `manent freeze`. The environment configuration stays in its system: the provider checks it, `consul_keys` (or your repository) writes it.

Reference: [CI and infrastructure as code](../guida/ci-iac.md#manent_deployment).
