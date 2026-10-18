# UC24: tenants. Four customers of uc24-shop in prod: a deploy per tenant with its parameters,
# targets dedicated (eu1-b for acme) and shared (eu1-a, one installation per tenant); a
# multi-tenant release with a pilot group and a tenant denied by its contract (Rego policy);
# the support of a customer limited to its tenant; a freeze by tag; the rollback of the
# release; the adoption of an environment per customer.
APP=uc24-shop
log "UC24 · tenant: deploy per cliente, rilascio multi-tenant con pilota, policy per contratto, permessi e freeze per tenant"

for v in 1.0.0 1.1.0; do artifact $APP $v >/dev/null; done

info "-- registro dei tenant"
expect_eq "quattro tenant collegati a uc24-shop/prod" "$(manent tenants list -a $APP -e prod | jq -r '[.[].name] | join(",")')" "acme,globex,initech,umbrella"
expect_eq "i segreti non sono esposti, i parametri sì" "$(manent tenants show globex | jq -r '.params."uc24-shop".replicas')" 2

info "-- un deploy per tenant"
d=$(descriptor uc24-a uc24-100-acme $APP prod 1.0.0 "tenant: acme")
id=$(submit "$d")
expect_eq "deploy di acme riuscito" "$(wait_state "$id" terminal 60)" SUCCESS
expect_eq "acme sul target condiviso (installazione propria)" "$(current eu1-a $APP-acme)" 1.0.0
expect_eq "acme sul target dedicato" "$(current eu1-b $APP-acme)" 1.0.0
d=$(descriptor uc24-b uc24-100-globex $APP prod 1.0.0 "tenant: globex")
id=$(submit "$d")
expect_eq "deploy di globex riuscito" "$(wait_state "$id" terminal 60)" SUCCESS
expect_eq "globex solo sul target condiviso" "$(current eu1-a $APP-globex) $(current eu1-b $APP-globex)" "1.0.0 none"
expect_match "lo step config riporta il tenant e il suo documento" \
  "$(manent status "$id" | jq -r '.result.steps[] | select(.name=="config") | .output.tenant_source')" "tenants/globex.yml"
d=$(descriptor uc24-c uc24-100-none $APP prod 1.0.0)
expect_match "senza tenant prod è rifiutato" "$(manent_err deploy --no-wait -f "$d")" "requires a tenant"
d=$(descriptor uc24-d uc24-100-x $APP prod 1.0.0 "tenant: nessuno")
expect_match "un tenant inesistente è rifiutato" "$(manent_err deploy --no-wait -f "$d")" "tenant not found"

info "-- policy per contratto"
rm -rf "$WORK/uc/uc24-policy" && cp -R cases/uc24-policy "$WORK/uc/uc24-policy"
rev=$(as pol manent policy push -d /uc/uc24-policy -m "uc24: contratti 2025 fermi alla 1.0" | jq -r .revision)
as admin manent policy approve "$rev" >/dev/null 2>&1
sleep 3
expect_match "la revisione per contratto è in vigore" "$(as admin manent policy list | jq -r .current)" "^$rev:"

info "-- rilascio multi-tenant con pilota"
d=$(descriptor uc24-e uc24-110 $APP prod 1.1.0 "tenants:
  selector: \"tier in (gold, silver)\"
  groups:
    - {name: pilota, select: \"tier=gold\", bake: 2s}
    - {name: resto}
  maxParallel: 2
  onFailure: stop")
out=$(manent deploy --no-wait -f "$d")
run=$(printf '%s' "$out" | jq -r .id)
expect_eq "tre tenant nel rilascio" "$(printf '%s' "$out" | jq -r '.tenants | join(",")')" "acme,globex,umbrella"
expect_match "initech escluso dalla policy per il contratto" "$(printf '%s' "$out" | jq -r '.skipped[0].tenant + ": " + .skipped[0].reason')" "^initech: .*2025 contract"
expect_eq "il rilascio riesce" "$(wait_state "$run" terminal 120)" SUCCESS
st=$(manent status "$run")
expect_eq "stato per tenant" "$(printf '%s' "$st" | jq -r '[.tenants_run.tenants[] | .tenant + "=" + .state] | join(",")')" "acme=SUCCESS,globex=SUCCESS,initech=DENIED,umbrella=SUCCESS"
expect_eq "acme e globex sulla 1.1.0, initech mai rilasciato" "$(current eu1-a $APP-acme) $(current eu1-a $APP-globex) $(current eu1-a $APP-initech)" "1.1.0 1.1.0 none"
expect_eq "il deploy del tenant appartiene al rilascio" "$(manent status "$run.globex" | jq -r '.run + " " + .tenant')" "$run globex"
expect_eq "inventario per tenant" "$(manent inventory -a $APP -e prod | jq -r '[.[] | .tenant + "=" + .current.version] | join(",")')" "acme=1.1.0,globex=1.1.0,umbrella=1.1.0"

info "-- permessi per tenant"
d=$(descriptor uc24-g uc24-110-acme-sup $APP prod 1.1.0 "tenant: acme")
id=$(as supacme submit "$d")
expect_eq "il supporto di acme rilascia acme" "$(wait_state "$id" terminal 60)" SUCCESS
d=$(descriptor uc24-h uc24-110-globex-sup $APP prod 1.1.0 "tenant: globex")
expect_match "il supporto di acme non tocca globex" "$(as supacme manent_err deploy --no-wait -f "$d")" "permission denied.*tenant globex"

info "-- freeze per tag"
as admin manent freeze add --name uc24-gold --reason "chiusura trimestrale dei clienti gold" -a $APP -e prod --tenants "tier=gold" --for 1h >/dev/null
d=$(descriptor uc24-i uc24-110-acme-frz $APP prod 1.1.0 "tenant: acme")
expect_match "il freeze ferma acme (gold)" "$(manent_err deploy --no-wait -f "$d")" 'freeze "uc24-gold"'
d=$(descriptor uc24-j uc24-110-globex-frz $APP prod 1.1.0 "tenant: globex")
id=$(submit "$d")
expect_eq "globex (silver) non è toccato dal freeze" "$(wait_state "$id" terminal 60)" SUCCESS
as admin manent freeze delete uc24-gold >/dev/null 2>&1

info "-- rollback del rilascio"
out=$(manent rollback -a $APP -e prod --run "$run" --no-wait)
rb=$(printf '%s' "$out" | jq -r .id)
expect_eq "il rollback del rilascio riesce" "$(wait_state "$rb" terminal 120)" SUCCESS
expect_eq "rollback in ordine inverso dei tenant riusciti" "$(printf '%s' "$out" | jq -r '.tenants | join(",")')" "globex,acme"
expect_match "umbrella, al primo rilascio, resta fuori dal rollback" "$(printf '%s' "$out" | jq -r '.skipped[0].tenant')" "^umbrella$"
expect_eq "acme e globex di nuovo sulla 1.0.0" "$(current eu1-a $APP-acme) $(current eu1-b $APP-acme) $(current eu1-a $APP-globex)" "1.0.0 1.0.0 1.0.0"

info "-- adozione di un ambiente per cliente"
d=$(descriptor uc24-k uc24-100-legacy $APP legacy-umbrella 1.0.0)
id=$(submit "$d")
expect_eq "deploy nell'ambiente per cliente di oggi" "$(wait_state "$id" terminal 60)" SUCCESS
out=$(dc exec -T api manent-server admin tenants-adopt -a $APP --environment legacy-umbrella --as prod --tenant umbrella 2>/dev/null)
aid=$(printf '%s' "$out" | jq -r .id)
expect_eq "adozione registrata senza toccare i target" "$(wait_state "$aid" terminal 30)" SUCCESS
expect_eq "l'inventario di prod/umbrella riporta l'adozione" \
  "$(manent inventory -a $APP -e prod -t umbrella | jq -r '.[0].current.kind + " " + .[0].current.version')" "adoption 1.0.0"

info "-- ritorno alla policy predefinita"
rm -rf "$WORK/uc/uc24-default" && mkdir -p "$WORK/uc/uc24-default" && printf '{}\n' >"$WORK/uc/uc24-default/data.json"
back=$(as pol manent policy push -d /uc/uc24-default -m "ritorno alle regole di manent.yml" | jq -r .revision)
as admin manent policy approve "$back" >/dev/null 2>&1
sleep 3
expect_match "policy predefinita di nuovo in vigore" "$(as admin manent policy list | jq -r .current)" "^$back:"
