# UC28: infrastructure as a deploy with the external Terraform plugin (OpenTofu in the worker
# image). The module "creates" VMs with terraform_data resources in two states (one per region)
# of the pg backend on the lab Postgres, and exports their hosts; uc28-app is released on them
# through the environment configuration (imports from current + targets_from). The dry-run shows
# the planned changes, a second deploy of the same version changes nothing, and a version that
# destroys a VM is refused by max_destroy without retries, leaving the release in service.
log "UC28 · infrastruttura con il plugin Terraform esterno"

# tfmodule <version> <vms> <tag>: module with <vms> VMs (eu1-b, eu1-c, eu1-a in this order).
tfmodule() {
  d=$(mktemp -d)
  cat >"$d/main.tf" <<EOF
terraform {
  backend "pg" {}
}
variable "env" { type = string }
locals {
  hosts = slice(["eu1-b", "eu1-c", "eu1-a"], 0, $2)
}
resource "terraform_data" "vm" {
  for_each = toset(local.hosts)
  input    = "\${var.env}-\${each.key}-$3"
}
output "vm_hosts" {
  value = [for h in local.hosts : { host = h, user = "deploy" }]
}
output "environment" { value = var.env }
EOF
  COPYFILE_DISABLE=1 tar -czf "$WORK/artifacts/uc28-infra-$1.tgz" -C "$d" .
  rm -rf "$d"
}
tfmodule 1.0.0 2 a
tfmodule 1.0.1 2 b
tfmodule 1.1.0 1 b
artifact uc28-app 1.0.0 >/dev/null

infra() { art_descriptor "$1" "$2" uc28-infra prod "$3" terraform "file:///srv/artifacts/uc28-infra-$3.tgz"; }
deploy_result() { manent status "$1" | jq -r --arg h "$2" --arg k "$3" \
  '.result.steps[] | select(.name == "deploy") | .output.result | fromjson | .targets[] | select(.host == $h) | .details[$k] // empty'; }
states() { dc exec -T postgres psql -U manent -d manent -tAc "select string_agg(name, ' ' order by name) from uc28_tfstate.states" 2>/dev/null | tr -d '\r'; }

info "-- dry-run: piano su entrambi gli stati, nessuno stato scritto"
id=$(manent deploy --no-wait --dry-run -f "$(infra uc28-infra-dry uc28-infra-100 1.0.0)" | jq -r .id)
expect_eq "dry-run riuscito" "$(wait_state "$id" terminal 180)" SUCCESS
expect_eq "piano: 2 VM da creare in ogni region" "$(deploy_result "$id" uc28-eu add) $(deploy_result "$id" uc28-us add)" "2 2"
expect_match "il piano riporta gli indirizzi delle risorse" "$(deploy_result "$id" uc28-eu resources)" 'terraform_data.vm\["eu1-b"\]'
expect_eq "nessuno stato nel backend dopo il dry-run" "$(states)" ""

info "-- deploy: apply in ogni region, output esportati come target"
infra_id=$(submit "$(infra uc28-infra-a uc28-infra-100 1.0.0)")
expect_eq "deploy dell'infrastruttura riuscito" "$(wait_state "$infra_id" terminal 240)" SUCCESS
expect_eq "uno stato per region nel backend pg" "$(states)" "uc28-eu uc28-us"
expect_eq "workspace e modifiche per target" "$(deploy_result "$infra_id" uc28-eu workspace) $(deploy_result "$infra_id" uc28-us changes)" "uc28-eu true"
expect_eq "VM esportate (format targets)" "$(manent status "$infra_id" | jq -c '.exports.vms | fromjson | map(.host)')" '["eu1-b","eu1-c"]'
expect_eq "variabile con segnaposto" "$(manent status "$infra_id" | jq -r '.result.steps[] | select(.name == "deploy") | .output["out.environment"]')" prod
expect_eq "nessun segreto negli output dello step" "$(manent status "$infra_id" | grep -c 'manent:manent@' | tr -d ' ')" 0

info "-- applicazione sulle VM create, dalla configurazione di ambiente"
app_id=$(submit "$(descriptor uc28-app-a uc28-app-100 uc28-app prod 1.0.0)")
expect_eq "deploy dell'applicazione riuscito" "$(wait_state "$app_id" terminal 120)" SUCCESS
expect_eq "rilasciata solo sulle VM esportate" "$(currents uc28-app eu1-a eu1-b eu1-c)" "none 1.0.0 1.0.0"

info "-- stessa versione di nuovo: nessuna modifica"
id=$(submit "$(infra uc28-infra-b uc28-infra-101 1.0.0)")
expect_eq "secondo deploy riuscito" "$(wait_state "$id" terminal 240)" SUCCESS
expect_eq "nessuna modifica pianificata" "$(deploy_result "$id" uc28-eu changes) $(deploy_result "$id" uc28-us add)" "false 0"

info "-- nuova versione che modifica le risorse"
id=$(submit "$(infra uc28-infra-c uc28-infra-102 1.0.1)")
expect_eq "deploy della 1.0.1 riuscito" "$(wait_state "$id" terminal 240)" SUCCESS
expect_eq "2 risorse modificate in place per region, nessuna distrutta" "$(deploy_result "$id" uc28-eu change) $(deploy_result "$id" uc28-eu destroy)" "2 0"
expect_eq "inventario sulla 1.0.1" "$(manent inventory -a uc28-infra -e prod | jq -r '.[0].current.version')" 1.0.1

info "-- max_destroy: una versione che distrugge una VM viene rifiutata, senza retry"
id=$(submit "$(infra uc28-infra-d uc28-infra-103 1.1.0)")
expect_eq "deploy rifiutato" "$(wait_state "$id" terminal 240)" ERROR
expect_match "il motivo cita max_destroy e la risorsa" "$(manent status "$id" | jq -r .error)" 'max_destroy \(0\).*terraform_data.vm'
plans=$(dc logs --no-color worker-1 worker-2 2>/dev/null | grep 'terraform plan' | grep -c "deployment=$id")
expect_eq "un solo piano: errore permanente, nessun retry" "$plans" 1
expect_eq "l'inventario resta sulla 1.0.1" "$(manent inventory -a uc28-infra -e prod | jq -r '.[0].current.version')" 1.0.1
