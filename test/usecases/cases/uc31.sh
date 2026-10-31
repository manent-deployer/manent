# UC31: several environment configuration providers (envconfig.providers). The uc31-* applications
# are read first from the legacy provider (on_missing: next while they move to the default one);
# the uc31team tenant names its own provider in the registry (tenant_only: the match rules never
# choose it, and a configuration missing there does not move to the next one).
APP=uc31-web
TEAM=uc31-team-web
log "UC31 · più provider della configurazione di ambiente"

for v in 1.0.0 1.1.0; do artifact $APP $v >/dev/null; artifact $TEAM $v >/dev/null; done

# config_output <id> <key>: output key of the config step, "none" if absent.
config_output() {
  manent status "$1" | jq -r --arg k "$2" '[.result.steps[] | select(.name == "config") | .output[$k]] | first // "none"'
}

info "-- regola di instradamento: il provider legacy ha ancora test"
expect_eq "validate: target dal provider legacy" "$(manent validate -f "$(descriptor uc31-a uc31-test-100 $APP test 1.0.0)" | jq -r '[.deploy.targets[].host] | join(",")')" eu1-a
id=$(submit "$(descriptor uc31-a uc31-test-100 $APP test 1.0.0)")
expect_eq "deploy riuscito" "$(wait_state "$id" terminal 60)" SUCCESS
expect_eq "letto dal provider legacy" "$(config_output "$id" provider)" uc31-legacy
expect_eq "nessun provider saltato" "$(config_output "$id" skipped)" none
expect_eq "eu1-a sulla 1.0.0" "$(current eu1-a $APP)" 1.0.0
expect_eq "eu1-c (stesso ambiente nel provider default) non toccato" "$(current eu1-c $APP)" none

info "-- on_missing: next: prod non è più nel provider legacy"
id=$(submit "$(descriptor uc31-b uc31-prod-100 $APP prod 1.0.0)")
expect_eq "deploy riuscito" "$(wait_state "$id" terminal 60)" SUCCESS
expect_eq "letto dal provider default" "$(config_output "$id" provider)" default
expect_eq "provider legacy saltato" "$(config_output "$id" skipped)" uc31-legacy
expect_match "sorgente: il file del provider default" "$(config_output "$id" source)" "untenanted/$APP/$APP/prod.yml"
expect_eq "eu1-b sulla 1.0.0" "$(current eu1-b $APP)" 1.0.0

info "-- provider del tenant (registro)"
id=$(submit "$(descriptor uc31-c uc31-team-100 $TEAM prod 1.0.0)")
expect_eq "deploy riuscito" "$(wait_state "$id" terminal 60)" SUCCESS
expect_eq "tenant del registro" "$(manent status "$id" | jq -r .tenant)" uc31team
expect_eq "letto dal provider del tenant" "$(config_output "$id" provider)" uc31-team
expect_eq "eu1-c sulla 1.0.0" "$(current eu1-c $TEAM)" 1.0.0
expect_match "ambiente assente nel provider del tenant: nessun ripiego, nessun target" \
  "$(manent_err deploy --no-wait -f "$(descriptor uc31-d uc31-team-test-100 $TEAM test 1.0.0)")" "target"
