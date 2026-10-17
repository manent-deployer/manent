# UC14: war on WildFly with the jboss plugin (HTTP management API with digest authentication).
# Every version is a deployment uc14-shop-<version>.war with runtime-name uc14-shop.war: the
# switch is a composite operation after suspending the server; previous versions stay disabled
# (keep_releases). A war WildFly refuses leaves the version in service; a red post-test brings
# the previous deployment back (automatic rollback); manual rollback; JVM restart.
APP=uc14-shop
log "UC14 · JBoss/WildFly: deployment per versione, sospensione, rollback e restart"

# mgmt <json>: management operation, from the host.
mgmt() { curl -s --digest -u manent:uc-jboss-1 -H 'Content-Type: application/json' -d "$1" http://127.0.0.1:19990/management; }
# deployments: "<name>=<enabled>" of the uc14-shop deployments, sorted.
deployments() { mgmt '{"operation":"read-children-resources","child-type":"deployment"}' | jq -r '.result | to_entries | map(select(.key | startswith("uc14-shop-"))) | map("\(.key)=\(.value.enabled)") | sort | join(" ")'; }
served() { curl -s http://127.0.0.1:18082/uc14-shop/version | tr -d '\r\n'; }
detail() { manent status "$1" | jq -r --arg k "$2" '.result.steps[] | select(.name == "deploy") | .output.result | fromjson | .targets[0].details[$k] // empty'; }

info "-- avvio di WildFly"
dc --profile jboss up -d wildfly >/dev/null 2>&1
t=0; until [ "$(mgmt '{"operation":"read-attribute","name":"server-state"}' | jq -r .result 2>/dev/null)" = running ]; do t=$((t + 1)); [ "$t" -gt 120 ] && break; sleep 1; done
expect_eq "WildFly in esecuzione, management raggiungibile con digest" "$(mgmt '{"operation":"read-attribute","name":"server-state"}' | jq -r .result)" running

for v in 1.0.0 2.0.0; do war $APP $v >/dev/null; done
war $APP 3.0.0 invalid >/dev/null
war $APP 4.0.0 broken >/dev/null
war $APP 5.0.0 >/dev/null
desc() { art_descriptor "$1" "$2" $APP prod "$3" war "file:///srv/artifacts/$APP-$3.war" "${4:-}"; }
posttest='steps: {post-test: true}
postTest:
  http:
    - {url: "http://wildfly:8080/uc14-shop/healthz"}'

info "-- dry-run"
out=$(manent deploy --dry-run --timeout 2m -f "$(desc uc14-dry uc14-100-dry 1.0.0)")
expect_eq "dry-run riuscito: modalità standalone" "$(printf '%s' "$out" | jq -r '.steps[]? | select(.name == "deploy") | .output.result' | jq -r '.targets[0].details.mode' 2>/dev/null)" standalone
expect_eq "il dry-run non carica nulla" "$(deployments)" ""

info "-- primo rilascio"
id=$(submit "$(desc uc14-a uc14-100 1.0.0 "$posttest")")
expect_eq "deploy riuscito" "$(wait_state "$id" terminal 180)" SUCCESS
expect_eq "WildFly serve la 1.0.0 su /uc14-shop" "$(served)" 1.0.0
expect_eq "un deployment per versione, attivo" "$(deployments)" "uc14-shop-1.0.0.war=true"
expect_eq "runtime-name comune alle versioni" "$(mgmt '{"operation":"read-attribute","address":[{"deployment":"uc14-shop-1.0.0.war"}],"name":"runtime-name"}' | jq -r .result)" uc14-shop.war

info "-- secondo rilascio con sospensione del server"
id=$(submit "$(desc uc14-b uc14-200 2.0.0 "$posttest")")
expect_eq "deploy riuscito" "$(wait_state "$id" terminal 180)" SUCCESS
expect_eq "WildFly serve la 2.0.0" "$(served)" 2.0.0
expect_eq "la 1.0.0 resta caricata ma disattivata" "$(deployments)" "uc14-shop-1.0.0.war=false uc14-shop-2.0.0.war=true"
expect_eq "server sospeso durante il cambio e versione precedente nei dettagli" "$(detail "$id" suspended) $(detail "$id" previous_version)" "true 1.0.0"
expect_eq "server di nuovo in servizio" "$(mgmt '{"operation":"read-attribute","name":"suspend-state"}' | jq -r .result)" RUNNING

info "-- war rifiutato da WildFly: resta la versione in esercizio"
id=$(submit "$(desc uc14-c uc14-300-invalid 3.0.0 "$posttest")")
expect_eq "deploy in errore" "$(wait_state "$id" terminal 180)" ERROR
expect_match "l'errore riporta il rifiuto di WildFly" "$(manent status "$id" | jq -r .error)" "version switch"
expect_eq "WildFly serve ancora la 2.0.0" "$(served)" 2.0.0
expect_eq "server di nuovo in servizio dopo il cambio fallito" "$(mgmt '{"operation":"read-attribute","name":"suspend-state"}' | jq -r .result)" RUNNING

info "-- post-test rosso: rollback automatico alla 2.0.0"
id=$(submit "$(desc uc14-d uc14-400-broken 4.0.0 "$posttest
rollback: {enabled: true}")")
expect_eq "deploy in errore" "$(wait_state "$id" terminal 180)" ERROR
expect_eq "rollback completato" "$(manent status "$id" | jq -r .rollback)" completed
expect_eq "WildFly serve di nuovo la 2.0.0" "$(served)" 2.0.0
expect_match "la 2.0.0 è il deployment attivo" "$(deployments)" "uc14-shop-2.0.0.war=true"

info "-- rollback manuale"
id=$(manent rollback -a $APP -e prod --no-wait | jq -r .id)
expect_eq "rollback manuale riuscito" "$(wait_state "$id" terminal 180)" SUCCESS
expect_eq "WildFly serve la 1.0.0, riattivata senza caricarla di nuovo" "$(served)" 1.0.0

info "-- restart della JVM dopo il cambio"
expect_match "un parametro in conflitto con la configurazione di ambiente è rifiutato" \
  "$(manent_err deploy --no-wait -f "$(desc uc14-x uc14-conflict 5.0.0 'deploy: {params: {suspend: "false"}}')")" "suspend"
id=$(submit "$(desc uc14-e uc14-500 5.0.0 "deploy: {params: {restart: \"true\"}}")")
expect_eq "deploy con restart riuscito" "$(wait_state "$id" terminal 300)" SUCCESS
expect_eq "il server è stato riavviato" "$(detail "$id" restarted)" true
expect_eq "dopo il riavvio WildFly serve la 5.0.0" "$(served)" 5.0.0
expect_eq "keep_releases=3: restano al più tre versioni" "$(deployments | wc -w | tr -d ' ')" 3

info "-- spegnimento di WildFly"
dc --profile jboss rm -sf wildfly >/dev/null 2>&1
