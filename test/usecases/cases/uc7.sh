# UC7: manifest su un cluster Kubernetes (k3s) con il plugin kubernetes. L'artefatto è il file
# dei manifest, verificato con lo sha256 e promosso così com'è tra gli ambienti: namespace e
# repliche arrivano dalla configurazione di ambiente (uc7-test con 1 replica, uc7-prod con 2).
# Le immagini sono fissate al digest (require_image_digest).
APP=uc7-api
log "UC7 · manifest su Kubernetes (plugin kubernetes)"

info "-- avvio di registry e cluster k3s"
dcp up -d registry k3s >/dev/null 2>&1
wait_registry || fail "registry non raggiungibile dall'host"
t=0
until kube -n default get serviceaccount default >/dev/null 2>&1; do t=$((t + 1)); [ "$t" -gt 120 ] && break; sleep 1; done
dcp exec -T k3s cat /var/lib/rancher/k3s/server/tls/server-ca.crt >"$WORK/k3s/ca.crt" 2>/dev/null
[ -s "$WORK/k3s/ca.crt" ] && pass "cluster pronto, CA del cluster disponibile ai worker" || fail "cluster k3s non pronto"
a100=$(image api 1.0.0)
a110=$(image api 1.1.0)
a120=$(image api 1.2.0)
a200=$(image api 2.0.0 broken)
k8s_manifest $APP 1.0.0 "$a100" >/dev/null
k8s_manifest $APP 1.1.0 "$a110" extra >/dev/null
k8s_manifest $APP 1.2.0 "$a120" >/dev/null
k8s_manifest $APP 2.0.0 "$a200" >/dev/null
(K8S_IMAGE=registry:5000/uc/api:1.0.0 k8s_manifest $APP 3.0.0-tag "" >/dev/null)

desc() { art_descriptor "$1" "$2" $APP "$3" "$4" kubernetes "file:///srv/artifacts/$APP-$4.yaml" "${5:-}"; }

info "-- dry-run lato server"
out=$(manent deploy --dry-run --timeout 2m -f "$(desc uc7-dry uc7-100-dry test 1.0.0)")
expect_eq "il dry-run riesce" "$(printf '%s' "$out" | jq -r .dry_run)" true
kube get namespace uc7-test >/dev/null 2>&1 && fail "il dry-run ha creato il namespace" || pass "il dry-run non crea nulla nel cluster"

info "-- test: primo rilascio"
id=$(submit "$(desc uc7-a uc7-100-test test 1.0.0)")
expect_eq "deploy dei manifest in test" "$(wait_state "$id" terminal 180)" SUCCESS
expect_eq "il Deployment usa l'immagine per digest" "$(k8s_image uc7-test $APP)" "$a100"
expect_eq "una replica pronta in test" "$(k8s_ready uc7-test $APP)" 1
expect_eq "lo storico indica la release in esercizio" "$(kube -n uc7-test get cm manent-$APP-test -o jsonpath='{.data.version}')" 1.0.0
expect_eq "gli oggetti hanno le etichette di manent" "$(kube -n uc7-test get deploy $APP -o jsonpath='{.metadata.labels.app\.kubernetes\.io/managed-by}')" manent

info "-- promozione in prod: stessi manifest, configurazione di ambiente diversa"
id=$(manent promote -a $APP --from test --to prod --no-wait | jq -r .id)
expect_eq "promozione test → prod riuscita" "$(wait_state "$id" terminal 180)" SUCCESS
expect_eq "due repliche pronte in prod (var.REPLICAS)" "$(k8s_ready uc7-prod $APP)" 2
expect_eq "stesso sha256 dei manifest di test" "$(manent status "$id" | jq -r .sha256)" "$(sha "$WORK/artifacts/$APP-1.0.0.yaml")"

info "-- aggiornamento alla 1.1.0 (aggiunge una ConfigMap)"
id=$(submit "$(desc uc7-b uc7-110 prod 1.1.0)")
expect_eq "rilascio della 1.1.0 riuscito" "$(wait_state "$id" terminal 180)" SUCCESS
expect_eq "il Deployment usa l'immagine 1.1.0" "$(k8s_image uc7-prod $APP)" "$a110"
expect_eq "la ConfigMap aggiunta esiste" "$(kube -n uc7-prod get cm $APP-extra -o jsonpath='{.data.version}')" 1.1.0

info "-- immagine difettosa: rollout bloccato e rollback"
id=$(submit "$(desc uc7-c uc7-200-broken prod 2.0.0)")
expect_eq "deploy della 2.0.0 in errore" "$(wait_state "$id" terminal 240)" ERROR
st=$(manent status "$id")
expect_eq "rollback completato" "$(printf '%s' "$st" | jq -r .rollback)" completed
expect_match "l'errore indica il rollout bloccato" "$(printf '%s' "$st" | jq -r .error)" "rollout"
expect_eq "il Deployment torna all'immagine 1.1.0" "$(k8s_image uc7-prod $APP)" "$a110"
expect_eq "due repliche pronte dopo il rollback" "$(k8s_ready uc7-prod $APP)" 2
expect_eq "lo storico indica di nuovo la 1.1.0" "$(kube -n uc7-prod get cm manent-$APP-prod -o jsonpath='{.data.version}')" 1.1.0

info "-- 1.2.0 senza la ConfigMap: prune"
id=$(submit "$(desc uc7-d uc7-120 prod 1.2.0)")
expect_eq "rilascio della 1.2.0 riuscito" "$(wait_state "$id" terminal 180)" SUCCESS
kube -n uc7-prod get cm $APP-extra >/dev/null 2>&1 && fail "la ConfigMap rimossa dai manifest esiste ancora" || pass "prune: la ConfigMap rimossa dai manifest è eliminata"
expect_eq "lo storico conserva al più 3 release (keep_releases)" \
  "$(kube -n uc7-prod get cm -l manent.io/role=release -o name | wc -l | tr -d ' ')" 3

info "-- immagine non fissata al digest"
id=$(submit "$(desc uc7-e uc7-300-tag prod 3.0.0-tag)")
expect_eq "deploy rifiutato" "$(wait_state "$id" terminal 120)" ERROR
expect_match "l'errore indica il digest mancante" "$(manent status "$id" | jq -r .error)" "non fissata al digest"
expect_eq "il cluster resta sulla 1.2.0" "$(k8s_image uc7-prod $APP)" "$a120"

info "-- rollback manuale"
id=$(manent rollback -a $APP -e prod --no-wait | jq -r .id)
expect_eq "rollback manuale riuscito" "$(wait_state "$id" terminal 180)" SUCCESS
expect_eq "torna la 1.1.0, con la sua ConfigMap" \
  "$(k8s_image uc7-prod $APP) $(kube -n uc7-prod get cm $APP-extra -o jsonpath='{.data.version}' 2>/dev/null)" "$a110 1.1.0"
expect_eq "l'inventario riporta la 1.1.0 come rollback" "$(manent inventory -a $APP -e prod | jq -r '.[0].current | "\(.version) \(.kind)"')" "1.1.0 rollback"

info "-- spegnimento del cluster"
dcp rm -sf k3s registry >/dev/null 2>&1
