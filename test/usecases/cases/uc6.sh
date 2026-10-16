# UC6: container images on VMs with Docker (container plugin). The artifact is a registry
# image (oci+http://registry:5000/uc/web:<tag>): manent pins its digest, which becomes the
# deploy's sha256. Test environment on vm-a; prod on vm-a and vm-b, in waves (canary on vm-a),
# with health gate on /healthz and rollback-all.
APP=uc6-web
log "UC6 · container su VM con Docker (plugin container)"

info "-- avvio di registry e VM"
dcp up -d registry vm-a vm-b >/dev/null 2>&1
wait_registry || fail "registry non raggiungibile dall'host"
for vm in vm-a vm-b; do
  t=0; until dcp exec -T -u deploy "$vm" docker info >/dev/null 2>&1; do t=$((t + 1)); [ "$t" -gt 60 ] && break; sleep 1; done
done
d100=$(image web 1.0.0)
d110=$(image web 1.1.0)
d200=$(image web 2.0.0 broken)
[ -n "$d100" ] && [ -n "$d110" ] && [ -n "$d200" ] && pass "immagini di prova nel registry" || fail "immagini di prova non caricate nel registry"

src() { echo "oci+http://registry:5000/uc/web:$1"; }
desc() { art_descriptor "$1" "$2" $APP "$3" "$4" container "$(src "$4")" "${5:-}"; }
vms() { echo "$(vm_version vm-a $APP) $(vm_version vm-b $APP)"; }

info "-- test: un container su vm-a"
id=$(submit "$(desc uc6-a uc6-100-test test 1.0.0)")
expect_eq "deploy dell'immagine 1.0.0 in test" "$(wait_state "$id" terminal 120)" SUCCESS
expect_eq "lo sha256 del deploy è il digest dell'immagine" "$(manent status "$id" | jq -r .sha256)" "$d100"
expect_eq "il container di test risponde con la 1.0.0" "$(vm_get vm-a 8081 /version)" 1.0.0
expect_eq "il container è avviato per digest" "$(vm_image vm-a uc6-web-test)" "$d100"
expect_eq "idempotenza: lo stesso descrittore restituisce lo stesso deploy" "$(submit "$(desc uc6-a uc6-100-test test 1.0.0)")" "$id"

info "-- promozione in prod, a ondate"
id=$(manent promote -a $APP --from test --to prod --no-wait | jq -r .id)
expect_eq "promozione test → prod riuscita" "$(wait_state "$id" terminal 180)" SUCCESS
expect_eq "stadi canary e resto superati" "$(manent status "$id" | jq -r '[.rollout.stages[] | "\(.name)=\(.state)"] | join(" ")')" "canary=succeeded resto=succeeded"
expect_eq "entrambe le VM sulla 1.0.0" "$(vms)" "1.0.0 1.0.0"
expect_eq "vm-b risponde con la 1.0.0" "$(vm_get vm-b 8080 /version)" 1.0.0

info "-- dry-run della 1.1.0"
out=$(manent deploy --dry-run --timeout 2m -f "$(desc uc6-dry uc6-110-dry prod 1.1.0)")
expect_eq "il dry-run riesce" "$(printf '%s' "$out" | jq -r .dry_run)" true
expect_eq "il dry-run non tocca i container" "$(vms)" "1.0.0 1.0.0"

info "-- aggiornamento alla 1.1.0"
id=$(submit "$(desc uc6-b uc6-110 prod 1.1.0)")
expect_eq "rilascio della 1.1.0 riuscito" "$(wait_state "$id" terminal 180)" SUCCESS
expect_eq "entrambe le VM sulla 1.1.0" "$(vms)" "1.1.0 1.1.0"
expect_eq "vm-a avvia l'immagine 1.1.0 per digest" "$(vm_image vm-a $APP)" "$d110"

info "-- immagine difettosa: il canary non diventa healthy, rollback-all"
id=$(submit "$(desc uc6-c uc6-200-broken prod 2.0.0)")
expect_eq "deploy della 2.0.0 in errore" "$(wait_state "$id" terminal 180)" ERROR
st=$(manent status "$id")
expect_eq "rollback completato" "$(printf '%s' "$st" | jq -r .rollback)" completed
expect_match "l'errore indica il container unhealthy" "$(printf '%s' "$st" | jq -r .error)" "unhealthy"
expect_eq "vm-a di nuovo sulla 1.1.0, vm-b mai toccata" "$(vms)" "1.1.0 1.1.0"
expect_eq "vm-a avvia di nuovo l'immagine 1.1.0" "$(vm_image vm-a $APP)" "$d110"
expect_eq "l'applicazione risponde" "$(vm_get vm-a 8080 /healthz)" "ok 1.1.0"

info "-- rollback manuale alla 1.0.0"
id=$(manent rollback -a $APP -e prod --to 1.0.0 --no-wait | jq -r .id)
expect_eq "rollback manuale riuscito" "$(wait_state "$id" terminal 180)" SUCCESS
expect_eq "entrambe le VM sulla 1.0.0" "$(vms)" "1.0.0 1.0.0"
expect_eq "è un deploy di tipo rollback con lo stesso digest" "$(manent status "$id" | jq -r '"\(.kind) \(.sha256)"')" "rollback $d100"

info "-- VM non raggiungibile durante il rilascio"
dcp stop vm-b >/dev/null 2>&1
id=$(submit "$(desc uc6-d uc6-110-down prod 1.1.0)")
expect_eq "con vm-b spenta il deploy fallisce" "$(wait_state "$id" terminal 180)" ERROR
expect_eq "rollback-all riporta il canary alla 1.0.0" "$(vm_version vm-a $APP)" 1.0.0
dcp start vm-b >/dev/null 2>&1

info "-- tag spostato nel registry: la promozione rifiuta l'immagine diversa"
moved=$(image web 1.0.0 "" 1.0.0-alterata)
[ "$moved" != "$d100" ] && pass "il tag 1.0.0 punta ora a un'altra immagine" || fail "il tag 1.0.0 non è cambiato"
id=$(manent promote -a $APP --from test --to prod --no-wait | jq -r .id)
expect_eq "la promozione della 1.0.0 fallisce" "$(wait_state "$id" terminal 120)" ERROR
expect_match "il controllo qa confronta i digest" "$(manent status "$id" | jq -r .error)" "sha256 atteso $d100"
expect_eq "il canary non è stato toccato" "$(vm_version vm-a $APP)" 1.0.0

info "-- spegnimento delle VM"
dcp rm -sf vm-a vm-b >/dev/null 2>&1
