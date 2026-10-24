# UC19: releases as code with Terraform. The manent provider (integrations/terraform, loaded with
# dev_overrides) keeps the release in service equal to the configuration: apply deploys, a
# version released outside Terraform or targets changed by hand show in the plan, the candidate
# environment configuration is checked in the plan.
APP=uc19-shop
log "UC19 · Terraform: release in esercizio e configurazione di ambiente come codice"

artifact $APP 1.0.0 >/dev/null
artifact $APP 1.1.0 >/dev/null
artifact $APP 1.2.0 broken >/dev/null
TF="$WORK/tf"
rm -rf "$TF"/* "$TF"/.terraform* 2>/dev/null
cat >"$TF/.terraformrc" <<'EOT'
provider_installation {
  dev_overrides {
    "joeg-ita/manent" = "/provider"
  }
  direct {}
}
EOT
cat >"$TF/deploy.yml" <<EOT
apiVersion: manent/v1
application: $APP
environment: prod
artifact: {type: application, name: $APP, version: "0.0.0", source: "file:///srv/artifacts/$APP-0.0.0.tar.gz"}
rollback: {enabled: true}
steps: {post-test: true}
postTest:
  http:
    - {url: "http://eu1-b:8080/$APP/current/healthz"}
EOT
cat >"$TF/main.tf" <<EOT
terraform {
  required_providers {
    manent = { source = "joeg-ita/manent" }
  }
}

# Server and token from MANENT_SERVER and MANENT_TOKEN.
provider "manent" {}

variable "app_version" { type = string }
variable "env_config" {
  type    = string
  default = ""
}

data "manent_environment_check" "prod" {
  count       = var.env_config == "" ? 0 : 1
  application = "$APP"
  environment = "prod"
  config      = file(var.env_config)
  descriptor  = file("deploy.yml")
}

resource "manent_deployment" "shop" {
  application          = "$APP"
  environment          = "prod"
  descriptor           = file("deploy.yml")
  version              = var.app_version
  source               = "file:///srv/artifacts/$APP-\${var.app_version}.tar.gz"
  timeout              = "3m"
  redeploy_out_of_sync = true
}

data "manent_inventory" "shop" {
  application = "$APP"
  depends_on  = [manent_deployment.shop]
}

output "deployment_id" { value = manent_deployment.shop.id }
output "state" { value = manent_deployment.shop.state }
output "inventory_version" { value = data.manent_inventory.shop.entries[0].version }
output "checked_targets" { value = try(data.manent_environment_check.prod[0].targets, []) }
EOT

# tf <args>: runs terraform in the lab; prints the output, the exit code goes to $WORK/tf/code.
tf() {
  dc --profile terraform run --rm -T terraform "$@" -no-color >"$TF/last.log" 2>&1
  echo $? >"$TF/code"
}
tfout() { dc --profile terraform run --rm -T terraform output -raw "$1" 2>/dev/null; }
code() { cat "$TF/code"; }

info "-- apply: la prima release"
tf apply -auto-approve -input=false -var app_version=1.0.0
expect_eq "terraform apply riesce" "$(code)" 0
expect_eq "eu1-b sulla 1.0.0" "$(current eu1-b $APP)" 1.0.0
expect_eq "stato del deploy nella risorsa" "$(tfout state)" SUCCESS
expect_eq "l'inventario letto dal data source" "$(tfout inventory_version)" 1.0.0
id1=$(tfout deployment_id)
expect_eq "l'ID della risorsa è il deploy in esercizio" "$(manent inventory -a $APP -e prod | jq -r '.[0].current.id')" "$id1"

tf plan -detailed-exitcode -input=false -var app_version=1.0.0
expect_eq "plan senza modifiche con la stessa versione" "$(code)" 0

info "-- nuova versione"
tf apply -auto-approve -input=false -var app_version=1.1.0
expect_eq "apply della 1.1.0 riesce" "$(code)" 0
expect_eq "eu1-b sulla 1.1.0" "$(current eu1-b $APP)" 1.1.0
expect_eq "nuovo deploy, nuovo ID" "$([ "$(tfout deployment_id)" != "$id1" ] && echo nuovo || echo uguale)" nuovo

info "-- versione cambiata fuori da Terraform"
manent rollback -a $APP -e prod --to 1.0.0 --timeout 2m >/dev/null
expect_eq "rollback manuale con la CLI" "$(current eu1-b $APP)" 1.0.0
tf plan -detailed-exitcode -input=false -var app_version=1.1.0
expect_eq "il plan vede la differenza" "$(code)" 2
expect_match "il plan mostra la versione in esercizio e quella voluta" "$(cat "$TF/last.log")" '"1.0.0" -> "1.1.0"'
tf apply -auto-approve -input=false -var app_version=1.1.0
expect_eq "apply riporta la 1.1.0" "$(current eu1-b $APP)" 1.1.0

info "-- target cambiato a mano (riconciliazione)"
dc exec -T -u deploy eu1-b sh -c "cd /home/deploy/apps/$APP && ln -sfn /home/deploy/apps/$APP/releases/1.0.0 .manual && mv -T .manual current"
t=0
until [ "$(manent inventory -a $APP -e prod | jq -r '.[0].out_of_sync // false')" = true ] || [ "$t" -ge 90 ]; do sleep 3; t=$((t + 3)); done
tf plan -detailed-exitcode -input=false -var app_version=1.1.0
expect_eq "con redeploy_out_of_sync il plan prevede un nuovo deploy" "$(code)" 2
expect_match "il plan dice perché" "$(cat "$TF/last.log")" "targets out of sync"
tf apply -auto-approve -input=false -var app_version=1.1.0
expect_eq "apply riallinea il target" "$(current eu1-b $APP)" 1.1.0

info "-- configurazione di ambiente verificata nel plan"
cp config/envconfig/untenanted/$APP/$APP/prod.yml "$TF/env-ok.yml"
sed 's/^plugin: remote-copy/plugn: remote-copy/' config/envconfig/untenanted/$APP/$APP/prod.yml >"$TF/env-bad.yml"
tf apply -auto-approve -input=false -var app_version=1.1.0 -var env_config=env-ok.yml
expect_eq "configurazione candidata valida: apply senza deploy" "$(code)" 0
expect_eq "il data source riporta i target della configurazione" "$(dc --profile terraform run --rm -T terraform output -json checked_targets 2>/dev/null | tr -d ' \n')" '["eu1-b"]'
tf plan -input=false -var app_version=1.1.0 -var env_config=env-bad.yml
expect_eq "configurazione candidata non valida: il plan fallisce" "$(code)" 1
expect_match "l'errore indica l'ambiente" "$(cat "$TF/last.log")" "invalid configuration of uc19-shop/prod"
expect_match "l'errore indica il campo" "$(cat "$TF/last.log")" "plugn"

info "-- release difettosa"
before=$(manent inventory -a $APP -e prod | jq -r '.[0].current.id')
tf apply -auto-approve -input=false -var app_version=1.2.0
expect_eq "apply della release difettosa fallisce" "$(code)" 1
expect_match "l'errore riporta il deploy non riuscito" "$(cat "$TF/last.log")" "deploy .* failed"
expect_eq "rollback automatico: eu1-b sulla 1.1.0" "$(current eu1-b $APP)" 1.1.0
expect_eq "l'inventario resta sul deploy precedente" "$(manent inventory -a $APP -e prod | jq -r '.[0].current.id')" "$before"
tf plan -detailed-exitcode -input=false -var app_version=1.2.0
expect_eq "il plan successivo ritenta la 1.2.0" "$(code)" 2

info "-- destroy"
tf destroy -auto-approve -input=false -var app_version=1.1.0
expect_eq "destroy riesce" "$(code)" 0
expect_match "avviso: la release resta in esercizio" "$(cat "$TF/last.log")" "release left live"
expect_eq "eu1-b resta sulla 1.1.0" "$(current eu1-b $APP)" 1.1.0
