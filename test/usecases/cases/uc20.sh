# UC20: Windows (windows plugin). An IIS application over WinRM and a Windows service over
# OpenSSH, on a host emulated with a real PowerShell: the plugin's scripts run as they would on
# Windows Server, on IIS and service cmdlets that keep their configuration in JSON
# (test/usecases/windows).
APP=uc20-shop
SVC=uc20-worker
log "UC20 · Windows: IIS via WinRM e servizio Windows via OpenSSH"

# uc20_zip <app> <version> [broken]: zip artifact with version, healthz (the IIS application)
# and bin/worker.exe (the service executable; a broken one fails at startup).
uc20_zip() {
  d=$(mktemp -d)
  mkdir -p "$d/bin"
  printf '%s\n' "$2" >"$d/version"
  ok=0
  if [ "${3:-}" = broken ]; then ok=1; else printf 'ok %s\n' "$2" >"$d/healthz"; fi
  printf '#!/bin/sh\n# worker %s\nexit %s\n' "$2" "$ok" >"$d/bin/worker.exe"
  rm -f "$WORK/artifacts/$1-$2.zip"
  (cd "$d" && zip -qr "$WORK/artifacts/$1-$2.zip" .) 2>/dev/null ||
    (cd "$d" && python3 -m zipfile -c "$WORK/artifacts/$1-$2.zip" version bin $( [ -f healthz ] && echo healthz ))
  rm -rf "$d"
}
for v in 1.0.0 1.1.0 1.3.0 1.4.0; do uc20_zip $APP $v; done
uc20_zip $APP 1.2.0 broken
uc20_zip $SVC 2.0.0
uc20_zip $SVC 2.1.0 broken

iis() { dc exec -T win1 cat /srv/win/state/iis.json; }
iis_path() { iis | jq -r '.sites["Default Web Site"]["/uc20-shop"].path'; }
pool() { iis | jq -r ".pools[\"uc20-shop\"].$1"; }
svc() { dc exec -T win1 cat /srv/win/state/services.json | jq -r ".UC20Worker.$1"; }
web() { dc exec -T win1 curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:8080/$APP/$1"; }
served() { dc exec -T win1 curl -s "http://127.0.0.1:8080/$APP/version"; }
rel() { echo "/srv/win/apps/$1/releases/$2"; }
deploy_status() { manent status "$1" | jq -r '[.result.steps[]? | select(.name == "deploy") | .output.result] | first // ""'; }

info "-- avvio dell'host Windows emulato"
dc --profile windows up -d win1 >/dev/null 2>&1
t=0
until [ "$(dc exec -T win1 curl -sk -o /dev/null -w '%{http_code}' https://127.0.0.1:5986/wsman 2>/dev/null)" = 401 ] || [ "$t" -gt 60 ]; do sleep 2; t=$((t + 2)); done
expect_eq "endpoint WinRM in ascolto con autenticazione" "$(dc exec -T win1 curl -sk -o /dev/null -w '%{http_code}' https://127.0.0.1:5986/wsman)" 401
expect_match "PowerShell sull'host" "$(dc exec -T win1 pwsh -NoProfile -c '$PSVersionTable.PSVersion.ToString()')" '^7\.'

checks="rollback: {enabled: true}
steps: {post-test: true}
postTest:
  http:
    - {url: \"http://win1:8080/$APP/healthz\"}"
desc() { art_descriptor "uc20-$1" "$2" "$3" prod "$4" windows "file:///srv/artifacts/$3-$4.zip" "${5:-}"; }

info "-- IIS via WinRM"
out=$(manent deploy --dry-run -f "$(desc dry uc20-100-dry $APP 1.0.0)")
expect_eq "dry-run riuscito" "$(printf '%s' "$out" | jq -r .dry_run)" true
expect_eq "il dry-run non cambia l'applicazione" "$(iis_path)" /srv/win/wwwroot

id=$(submit "$(desc a uc20-100 $APP 1.0.0 "$checks")")
expect_eq "deploy della 1.0.0 riuscito" "$(wait_state "$id" terminal 120)" SUCCESS
expect_match "il deploy è passato da WinRM" "$(deploy_status "$id")" '"transport":"winrm"'
expect_eq "physicalPath sulla release 1.0.0" "$(iis_path)" "$(rel $APP 1.0.0)"
expect_eq "l'applicazione serve la 1.0.0" "$(served)" 1.0.0
expect_eq "healthz risponde 200" "$(web healthz)" 200
expect_eq "application pool fermato e riavviato durante il cambio" "$(pool state) $(pool starts)" "Started 1"

id=$(submit "$(desc b uc20-110 $APP 1.1.0 "$checks")")
expect_eq "deploy della 1.1.0 riuscito" "$(wait_state "$id" terminal 120)" SUCCESS
expect_eq "l'applicazione serve la 1.1.0" "$(served)" 1.1.0

id=$(submit "$(desc c uc20-120 $APP 1.2.0 "$checks")")
expect_eq "release senza healthz: post-test rosso, deploy ERROR" "$(wait_state "$id" terminal 120)" ERROR
expect_eq "rollback automatico completato" "$(manent status "$id" | jq -r .rollback)" completed
expect_eq "physicalPath tornato alla 1.1.0" "$(iis_path)" "$(rel $APP 1.1.0)"
expect_eq "healthz di nuovo 200" "$(web healthz)" 200

info "-- modifica in IIS fuori da manent (riconciliazione)"
dc exec -T -u deploy win1 pwsh -NoProfile -c "Import-Module WebAdministration; Set-WebConfigurationProperty -PSPath 'MACHINE/WEBROOT/APPHOST' -Filter \"system.applicationHost/sites/site[@name='Default Web Site']/application[@path='/uc20-shop']/virtualDirectory[@path='/']\" -Name physicalPath -Value $(rel $APP 1.0.0)" >/dev/null
t=0
until [ "$(manent inventory -a $APP -e prod | jq -r '.[0].out_of_sync // false')" = true ] || [ "$t" -ge 90 ]; do sleep 3; t=$((t + 3)); done
expect_eq "il target letto via WinRM risulta drifted sulla 1.0.0" \
  "$(manent inventory -a $APP -e prod | jq -r '.[0].targets.targets[0] | "\(.state) \(.version)"')" "drifted 1.0.0"

info "-- release già presenti e pulizia"
manent rollback -a $APP -e prod --to 1.0.0 --timeout 3m >/dev/null
rb=$(manent list -a $APP -e prod -n 1 | jq -r '.[0].id')
expect_eq "rollback manuale alla 1.0.0" "$(iis_path)" "$(rel $APP 1.0.0)"
expect_match "la release già estratta non viene copiata di nuovo" "$(manent logs "$rb")" "copia saltata"
for v in 1.3.0 1.4.0; do
  id=$(submit "$(desc "r$v" "uc20-$v" $APP $v "$checks")")
  expect_eq "deploy della $v riuscito" "$(wait_state "$id" terminal 120)" SUCCESS
done
expect_eq "keep_releases 3: restano le tre release più recenti" "$(dc exec -T win1 ls "/srv/win/apps/$APP/releases" | tr '\n' ' ')" "1.2.0 1.3.0 1.4.0 "

info "-- servizio Windows via OpenSSH"
expect_eq "servizio sull'eseguibile precedente" "$(svc state)" Running
id=$(submit "$(desc s1 uc20-w200 $SVC 2.0.0 "rollback: {enabled: true}")")
expect_eq "deploy del servizio 2.0.0 riuscito" "$(wait_state "$id" terminal 120)" SUCCESS
expect_match "il deploy è passato da SSH" "$(deploy_status "$id")" '"transport":"ssh"'
expect_eq "binPath sull'eseguibile della release, con gli argomenti" "$(svc path)" "\"$(rel $SVC 2.0.0)/bin/worker.exe\" --port 9090"
expect_eq "servizio di nuovo in esecuzione" "$(svc state)" Running

id=$(submit "$(desc s2 uc20-w210 $SVC 2.1.0 "rollback: {enabled: true}")")
expect_eq "eseguibile che non parte: deploy ERROR" "$(wait_state "$id" terminal 120)" ERROR
expect_match "l'errore viene dall'avvio del servizio" "$(manent status "$id" | jq -r .error)" "cannot be started"
expect_eq "rollback completato" "$(manent status "$id" | jq -r .rollback)" completed
expect_eq "binPath tornato alla 2.0.0" "$(svc path)" "\"$(rel $SVC 2.0.0)/bin/worker.exe\" --port 9090"
expect_eq "servizio in esecuzione sulla 2.0.0" "$(svc state)" Running
expect_eq "l'inventario resta sulla 2.0.0" "$(manent inventory -a $SVC -e prod | jq -r '.[0].current.version')" 2.0.0

info "-- spegnimento dell'host Windows emulato"
dc --profile windows stop win1 >/dev/null 2>&1
