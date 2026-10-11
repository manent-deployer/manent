# UC2: configurazione di ambiente (target e parametri non stanno nel descrittore), accesso con
# token e permessi, promozione test → prod con approvazione, rifiuto e freeze con deroga.
APP=uc2-api
log "UC2 · configurazione di ambiente, permessi, promozione con approvazione e freeze"

artifact $APP 1.0.0 >/dev/null
artifact $APP 1.1.0 >/dev/null

info "-- accesso"
code=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:18080/api/v1/deployments)
expect_eq "API senza token: 401" "$code" 401
d=$(descriptor uc2-a uc2-100-test $APP test 1.0.0)
err=$(as viewer manent_err deploy --no-wait -f "$d")
expect_match "un viewer non può avviare deploy" "$err" "permesso negato.*deployment.submit"

info "-- configurazione di ambiente"
d=$(descriptor uc2-b uc2-100-conflict $APP test 1.0.0 "deploy:
  params: {dest_dir: /tmp/altrove}")
err=$(manent_err deploy --no-wait -f "$d")
expect_match "un parametro in conflitto con l'ambiente è rifiutato" "$err" "dest_dir"
d=$(descriptor uc2-c uc2-100-prod $APP prod 1.0.0)
err=$(manent_err deploy --no-wait -f "$d")
expect_match "prod richiede lo stesso artefatto già rilasciato in test" "$err" "non risulta rilasciato con successo in test"
d=$(descriptor uc2-a uc2-100-test $APP test 1.0.0)
id=$(submit "$d")
expect_eq "deploy in test con target e parametri dall'ambiente" "$(wait_state "$id" terminal 60)" SUCCESS
expect_eq "us1-a (test) sulla 1.0.0" "$(current us1-a $APP)" 1.0.0
expect_match "lo step config riporta la sorgente della configurazione" \
  "$(manent status "$id" | jq -r '.result.steps[] | select(.name=="config") | .output.source')" "uc2-api/test"

info "-- promozione con approvazione"
out=$(manent promote -a $APP --from test --to prod --no-wait)
id=$(printf '%s' "$out" | jq -r .id)
expect_eq "la promozione attende l'approvazione" "$(state "$id")" AWAITING_APPROVAL
err=$(manent_err approve "$id")
expect_match "ci non ha il permesso di approvare" "$err" "deployment.approve"
as cab manent approve -m "ok dal CAB" "$id" >/dev/null
expect_eq "approvata dal CAB, la promozione riesce" "$(wait_state "$id" terminal 90)" SUCCESS
expect_eq "prod (tre AZ) sulla 1.0.0" "$(currents $APP eu1-a eu1-b eu1-c)" "1.0.0 1.0.0 1.0.0"
st=$(manent status "$id")
expect_eq "il deploy è di tipo promotion" "$(printf '%s' "$st" | jq -r .kind)" promotion
expect_eq "lo sha256 è quello rilasciato in test" "$(printf '%s' "$st" | jq -r .sha256)" "$(sha "$WORK/artifacts/$APP-1.0.0.tar.gz")"

d=$(descriptor uc2-d uc2-110-test $APP test 1.1.0)
id=$(submit "$d")
expect_eq "1.1.0 in test" "$(wait_state "$id" terminal 60)" SUCCESS
id=$(as admin manent promote -a $APP --from test --to prod --no-wait | jq -r .id)
expect_eq "seconda promozione (richiesta dall'admin) in attesa" "$(state "$id")" AWAITING_APPROVAL
err=$(as admin manent_err approve "$id")
expect_match "nessuno approva il proprio deploy, neanche l'admin" "$err" "non può approvarlo"
expect_eq "l'auto-approvazione non sblocca il deploy" "$(state "$id")" AWAITING_APPROVAL
as cab manent reject -m "non in questa finestra" "$id" >/dev/null
expect_eq "promozione rifiutata" "$(wait_state "$id" terminal 30)" REJECTED
expect_eq "prod resta sulla 1.0.0" "$(currents $APP eu1-a eu1-b eu1-c)" "1.0.0 1.0.0 1.0.0"
expect_eq "l'inventario di prod riporta la 1.0.0" "$(manent inventory -a $APP -e prod | jq -r '.[0].current.version')" 1.0.0

info "-- freeze"
expect_match "il freeze è elencato" "$(manent freezes -a $APP)" "uc2-congelato"
d=$(descriptor uc2-e uc2-100-frozen $APP frozen 1.0.0)
err=$(manent_err deploy --no-wait -f "$d")
expect_match "deploy in un ambiente congelato rifiutato" "$err" "bloccato dal freeze \"uc2-congelato\""
d=$(descriptor uc2-f uc2-100-frozen $APP frozen 1.0.0 'freezeOverride: "INC-42: correzione urgente"')
err=$(manent_err deploy --no-wait -f "$d")
expect_match "la deroga richiede un permesso che ci non ha" "$err" "deployment.freeze-override"
id=$(as admin submit "$d")
expect_eq "con la deroga dell'admin il deploy riesce" "$(wait_state "$id" terminal 60)" SUCCESS
expect_eq "us1-b (frozen) sulla 1.0.0" "$(current us1-b $APP)" 1.0.0
