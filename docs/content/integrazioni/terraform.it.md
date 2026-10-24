# Terraform

Il provider Terraform tratta **la release in esercizio** di un'applicazione come una risorsa. `terraform plan` mostra quando la versione in un ambiente è diversa da quella configurata, o quando i target sono stati cambiati fuori da manent; `terraform apply` la rilascia tramite manent, con tutte le sue regole. Un data source verifica una configurazione di ambiente candidata e **fa fallire il plan** prima che un'altra risorsa la scriva in Consul.

!!! abstract "Cosa serve"
    Terraform 1.x · Go, per compilare il provider finché non è pubblicato nel registry · un token di manent per l'identità che esegue Terraform

## 1. Compila e installa il provider

Il provider è un modulo Go separato in `integrations/terraform`:

```sh
make terraform-provider                       # bin/terraform-provider-manent
sudo install -d /opt/terraform/providers
sudo install bin/terraform-provider-manent /opt/terraform/providers/
```

Finché non è nel registry, indica a Terraform dove trovarlo con `dev_overrides` nella configurazione della CLI:

```hcl title="~/.terraformrc"
provider_installation {
  dev_overrides { "manent-deployer/manent" = "/opt/terraform/providers" }
  direct {}
}
```

Terraform stampa poi un avviso che il provider è sostituito da una build locale: è normale.

## 2. Configuralo

```hcl title="main.tf"
terraform {
  required_providers {
    manent = { source = "manent-deployer/manent" }
  }
}

provider "manent" {}   # MANENT_SERVER e MANENT_TOKEN, oppure server, token, token_file, ca_file
```

L'identità del token ha bisogno di `deployment.read` per il plan, e di `deployment.submit` sugli ambienti in cui rilascia.

## 3. La release in esercizio come risorsa

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

| Situazione | Plan |
|---|---|
| Nulla è cambiato | Nessuna modifica. |
| `shop_version` è cambiata | `version: "1.3.2" -> "1.4.0"`: l'apply esegue un deploy e lo attende, approvazioni comprese, fino a `timeout`. |
| Qualcuno ha rilasciato o riportato indietro un'altra versione fuori da Terraform | La differenza letta dall'[inventario](../guida/governance.md#inventario); l'apply rilascia di nuovo quella configurata. |
| Target cambiati fuori da manent (con la [riconciliazione](../guida/governance.md#stato-reale) e `redeploy_out_of_sync`) | `"1.4.0 (targets out of sync)" -> "1.4.0"`: l'apply la rilascia di nuovo. |

- Un deploy fallito fa fallire l'apply. Con il rollback automatico l'ambiente è tornato alla versione precedente, e il plan successivo ci riprova.
- `terraform destroy` non tocca i target: manent non disinstalla le release, la risorsa esce solo dallo state.
- Un ambiente esistente si importa con `terraform import manent_deployment.shop_prod shop/prod` (`payments/shop/prod` per l'applicazione di un [tenant](../guida/tenant.md)). L'attributo `tenant` sceglie tra i tenant che elencano l'applicazione, obbligatorio solo se più di uno la elenca.
- Il data source `manent_tenants` elenca i [tenant](../guida/tenant.md) con le loro applicazioni, per esempio per un `for_each` sulle applicazioni di un team.

## 4. Verifica le modifiche alla configurazione nel plan

Quando Terraform scrive anche la configurazione di ambiente in Consul, lascia che manent la verifichi prima:

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

Una configurazione non valida (un conflitto di fusione, un parametro non ammesso, un'ondata che non seleziona alcun target, un parametro sconosciuto del plugin) **fa fallire il plan** con l'errore di manent, prima di toccare Consul. Il data source restituisce anche `plugin`, `targets`, `stages` e `warnings`, utili negli output o nei controlli di policy.

## 5. Leggi l'inventario

```hcl
data "manent_inventory" "prod" { environment = "prod" }

output "prod_versions" {
  value = { for e in data.manent_inventory.prod.entries : e.application => e.version }
}
```

## Cosa resta fuori da Terraform

Permessi e regole di governance YAML stanno in `manent.yml`; le policy Rego e i freeze a runtime si gestiscono con `manent policy` e `manent freeze`. La configurazione di ambiente resta nel suo sistema: il provider la verifica, `consul_keys` (o il tuo repository) la scrive.

Riferimento: [CI e infrastruttura come codice](../guida/ci-iac.md#manent_deployment).
