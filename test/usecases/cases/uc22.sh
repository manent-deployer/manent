# UC22: Azure. App Service (appservice plugin) with a staging slot and the swap, without slot,
# and in a percentage rollout with the gate on Log Analytics (azure-monitor); Container Apps
# (containerapps plugin) with a revision per deploy. Azure is emulated at protocol level by
# the fakes service (internal/azurefake): Entra ID token, Resource Manager, Kudu, the web apps
# and the container apps themselves, Log Analytics.
WEB=uc22-web
SITE=uc22-site
RAMP=uc22-ramp
API=uc22-api
AZ=http://127.0.0.1:19400
log "UC22 · Azure: App Service con slot e swap, rilascio per percentuale, Container Apps"

# uc22_zip <app> <version> [broken]: package with version and healthz (missing if broken).
uc22_zip() {
  d=$(mktemp -d)
  printf '%s\n' "$2" >"$d/version"
  [ "${3:-}" = broken ] || printf 'ok %s\n' "$2" >"$d/healthz"
  rm -f "$WORK/artifacts/$1-$2.zip"
  (cd "$d" && zip -qr "$WORK/artifacts/$1-$2.zip" .) 2>/dev/null ||
    (cd "$d" && python3 -m zipfile -c "$WORK/artifacts/$1-$2.zip" $(ls))
  rm -rf "$d"
}
for v in 1.0.0 1.1.0; do uc22_zip $WEB $v; done
uc22_zip $WEB 1.2.0 broken
uc22_zip $SITE 1.0.0
for v in 1.0.0 2.0.0 2.1.0; do uc22_zip $RAMP $v; done

az_state() { curl -fs "$AZ/fake/state"; }
site() { az_state | jq -r ".sites[\"$1\"].$2"; }
served() { curl -s "$AZ/site/$1.azurewebsites.net/version"; }
app_served() { curl -s "$AZ/app/$API.nicebeach-1a2b3c.westeurope.azurecontainerapps.io/version"; }
desc() { art_descriptor "uc22-$1" "$2" "$3" prod "$4" appservice "file:///srv/artifacts/$3-$4.zip" "${5:-}"; }

info "-- App Service con slot di staging"
id=$(submit "$(desc w1 uc22-w100 $WEB 1.0.0)")
expect_eq "deploy della 1.0.0 riuscito" "$(wait_state "$id" terminal 120)" SUCCESS
expect_eq "dopo lo swap la produzione serve la 1.0.0" "$(served $WEB)" 1.0.0
id=$(submit "$(desc w2 uc22-w110 $WEB 1.1.0 "rollback: {enabled: true}")")
expect_eq "deploy della 1.1.0 riuscito" "$(wait_state "$id" terminal 120)" SUCCESS
expect_eq "produzione sulla 1.1.0, lo slot conserva la 1.0.0" "$(served $WEB) $(served $WEB-staging)" "1.1.0 1.0.0"
expect_eq "la versione viaggia con lo swap nelle app setting" \
  "$(site $WEB 'settings.MANENT_VERSION') $(site $WEB/staging 'settings.MANENT_VERSION')" "1.1.0 1.0.0"
expect_eq "l'inventario legge la 1.1.0 dalla produzione" "$(manent inventory -a $WEB -e prod | jq -r '.[0].current.version')" 1.1.0
id=$(submit "$(desc w3 uc22-w120 $WEB 1.2.0 "rollback: {enabled: true}")")
expect_eq "release senza healthz: lo slot non diventa sano, deploy ERROR" "$(wait_state "$id" terminal 180)" ERROR
expect_match "l'errore indica il controllo di salute dello slot" "$(manent status "$id" | jq -r .error)" "/healthz of slot staging not healthy"
expect_eq "la release difettosa non è mai arrivata in produzione" "$(served $WEB)" 1.1.0

info "-- swap fatto a mano fuori da manent (riconciliazione)"
curl -fs -X POST -H 'Authorization: Bearer fake-token-uc22' -H 'Content-Type: application/json' -d '{"targetSlot": "production"}' \
  "$AZ/subscriptions/33333333-0000-4000-8000-000000000022/resourceGroups/uc22/providers/Microsoft.Web/sites/$WEB/slots/staging/slotsswap?api-version=2023-12-01" >/dev/null
t=0
until [ "$(manent inventory -a $WEB -e prod | jq -r '.[0].out_of_sync // false')" = true ] || [ "$t" -ge 90 ]; do sleep 3; t=$((t + 3)); done
expect_eq "la web app risulta drifted sulla versione arrivata con lo swap" \
  "$(manent inventory -a $WEB -e prod | jq -r '.[0].targets.targets[0] | "\(.state) \(.version)"')" "drifted 1.2.0"
# For manent the 1.1.0 is still in service (a rollback to it is refused): a new deploy of it
# realigns production, with a new swap.
id=$(submit "$(desc w4 uc22-w111 $WEB 1.1.0)")
expect_eq "nuovo deploy della 1.1.0: di nuovo in produzione con uno swap" "$(wait_state "$id" terminal 120) $(served $WEB)" "SUCCESS 1.1.0"
t=0
until [ "$(manent inventory -a $WEB -e prod | jq -r '.[0].targets.targets[0].state // ""')" = in-sync ] || [ "$t" -ge 60 ]; do sleep 3; t=$((t + 3)); done
expect_eq "la web app torna in-sync" "$(manent inventory -a $WEB -e prod | jq -r '.[0].targets.targets[0].state')" in-sync

info "-- App Service senza slot"
id=$(submit "$(desc s1 uc22-s100 $SITE 1.0.0)")
expect_eq "deploy in produzione riuscito" "$(wait_state "$id" terminal 120)" SUCCESS
expect_eq "la produzione serve la 1.0.0" "$(served $SITE)" 1.0.0
expect_eq "versione e deploy nei tag della web app, nessuna app setting" \
  "$(site $SITE 'tags["manent-version"]') $(site $SITE 'tags["manent-deployment"]') $(site $SITE 'settings | length')" "1.0.0 uc22-s100 0"

info "-- rilascio per percentuale con il gate su Log Analytics"
series() { curl -fs -X POST -d "{\"match\": \"uc22-ramp\", \"values\": [$1]}" "$AZ/fake/series" >/dev/null; }
series "0.01, 0.01"
id=$(submit "$(desc r1 uc22-r100 $RAMP 1.0.0)")
expect_eq "primo rilascio (nessuna stabile da confrontare) riuscito" "$(wait_state "$id" terminal 120)" SUCCESS
id=$(submit "$(desc r2 uc22-r200 $RAMP 2.0.0)")
seen=""; t=0
while [ "$t" -lt 120 ]; do
  r=$(site $RAMP 'ramp[0].reroutePercentage // empty'); [ -n "$r" ] && seen=$r
  [ "$(state "$id")" = SUCCESS ] && break
  sleep 1; t=$((t + 1))
done
expect_eq "il 30% del traffico di produzione verso lo slot (rampUpRules)" "$seen" 30
expect_eq "rilascio della 2.0.0 riuscito" "$(wait_state "$id" terminal 120)" SUCCESS
expect_eq "dopo lo swap: produzione sulla 2.0.0, nessuna regola di routing" "$(served $RAMP) $(site $RAMP 'ramp | length')" "2.0.0 0"
expect_match "il gate ha interrogato Log Analytics con la query KQL" "$(az_state | jq -r '.queries | join("\n")')" "AppRoleName == 'uc22-ramp'.*bin\\(TimeGenerated, [0-9]+s\\) \\| [0-9TZ:-]+/[0-9TZ:-]+"
series "0.01, 0.4"
id=$(submit "$(desc r3 uc22-r210 $RAMP 2.1.0)")
expect_eq "errori oltre la soglia: deploy ERROR" "$(wait_state "$id" terminal 180)" ERROR
expect_match "il gate cita la metrica da azure-monitor" "$(manent status "$id" | jq -r .error)" "metric errori.*value 0.4 out of threshold"
expect_eq "la produzione resta sulla 2.0.0, nessun traffico allo slot" "$(served $RAMP) $(site $RAMP 'ramp | length')" "2.0.0 0"

info "-- Container Apps"
dcp up -d registry >/dev/null 2>&1
wait_registry || fail "registry non raggiungibile dall'host"
i100=$(image $API 1.0.0)
i200=$(image $API 2.0.0)
i300=$(image uc22-broken 3.0.0 broken)
[ -n "$i100" ] && [ -n "$i200" ] && [ -n "$i300" ] && pass "immagini di prova nel registry" || fail "immagini di prova non caricate nel registry"
adesc() { art_descriptor "uc22-$1" "$2" $API prod "$3" containerapps "oci+http://registry:5000/uc/$4:$3" "${5:-}"; }
id=$(submit "$(adesc a1 uc22-a100 1.0.0 $API)")
expect_eq "nuova revisione 1.0.0 con tutto il traffico" "$(wait_state "$id" terminal 120) $(app_served)" "SUCCESS 1.0.0"
expect_eq "immagine fissata al digest nella revisione" \
  "$(az_state | jq -r '.apps["uc22-api"] | .revisions[.latestReady].image')" "registry:5000/uc/$API@sha256:$i100"
id=$(submit "$(adesc a2 uc22-a200 2.0.0 $API "rollback: {enabled: true}")")
expect_eq "revisione 2.0.0 in servizio" "$(wait_state "$id" terminal 120) $(app_served)" "SUCCESS 2.0.0"
id=$(submit "$(adesc a3 uc22-a300 3.0.0 uc22-broken "rollback: {enabled: true}")")
expect_eq "revisione che non diventa sana: deploy ERROR" "$(wait_state "$id" terminal 120)" ERROR
expect_eq "rollback completato: il traffico torna sulla revisione 2.0.0" "$(manent status "$id" | jq -r .rollback) $(app_served)" "completed 2.0.0"
expect_eq "l'inventario resta sulla 2.0.0" "$(manent inventory -a $API -e prod | jq -r '.[0].current.version')" 2.0.0
