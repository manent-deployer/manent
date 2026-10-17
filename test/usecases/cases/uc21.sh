# UC21: the rest of Windows (windows plugin). An IIS application on two hosts behind an ARR web
# farm, an MSI package on the nodes of an NLB cluster and a scheduled task, on the emulated
# Windows hosts of UC20 (test/usecases/windows): the plugin's PowerShell runs as on Windows
# Server, on cmdlets, msiexec and Microsoft.Web.Administration that keep their state in JSON.
WEB=uc21-web
AGENT=uc21-agent
JOB=uc21-job
log "UC21 · Windows: web farm ARR, pacchetti MSI dietro NLB, attività pianificate"

# uc21_zip <app> <version>: zip with version, healthz and bin/worker.exe.
uc21_zip() {
  d=$(mktemp -d)
  mkdir -p "$d/bin"
  printf '%s\n' "$2" >"$d/version"
  printf 'ok %s\n' "$2" >"$d/healthz"
  printf '#!/bin/sh\n# worker %s\nexit 0\n' "$2" >"$d/bin/worker.exe"
  rm -f "$WORK/artifacts/$1-$2.zip"
  (cd "$d" && zip -qr "$WORK/artifacts/$1-$2.zip" .) 2>/dev/null ||
    (cd "$d" && python3 -m zipfile -c "$WORK/artifacts/$1-$2.zip" version healthz bin)
  rm -rf "$d"
}
# uc21_msi <version> <product code> [fail]: the emulated MSI package (JSON read by msiexec.exe).
uc21_msi() {
  printf '{"name": "UC21 Agent", "version": "%s", "code": "%s", "fail": %s}\n' "$1" "$2" "${3:-false}" >"$WORK/artifacts/$AGENT-$1.msi"
}
for v in 1.0.0 1.1.0; do uc21_zip $WEB $v; done
for v in 3.0.0 3.1.0; do uc21_zip $JOB $v; done
uc21_msi 4.0.0 '{4E0A1B2C-0000-4000-8000-000000000400}'
uc21_msi 4.1.0 '{4E0A1B2C-0000-4000-8000-000000000410}'
uc21_msi 4.2.0 '{4E0A1B2C-0000-4000-8000-000000000420}' true

wstate() { dc exec -T "$1" cat "/srv/win/state/$2.json"; }
served() { dc exec -T "$1" curl -s "http://127.0.0.1:8080/$WEB/version"; }
history() { wstate win1 "$1" | jq -r '.history | join(" ")'; }
products() { wstate "$1" products | jq -r '[.[] | "\(.name) \(.version)"] | join(",")'; }
task() { wstate win1 tasks | jq -r ".[\"\\\\UC21\\\\Export\"].$1"; }
rel() { echo "/srv/win/apps/$1/releases/$2"; }
desc() { art_descriptor "uc21-$1" "$2" "$3" prod "$4" windows "file:///srv/artifacts/$3-$4.${6:-zip}" "${5:-}"; }

info "-- avvio dei due host Windows emulati"
dc --profile windows up -d win1 win2 >/dev/null 2>&1
for h in win1 win2; do
  t=0
  until [ "$(dc exec -T $h curl -sk -o /dev/null -w '%{http_code}' https://127.0.0.1:5986/wsman 2>/dev/null)" = 401 ] || [ "$t" -gt 60 ]; do sleep 2; t=$((t + 2)); done
done
expect_eq "WinRM in ascolto su win1 e win2" \
  "$(dc exec -T win1 curl -sk -o /dev/null -w '%{http_code}' https://127.0.0.1:5986/wsman) $(dc exec -T win2 curl -sk -o /dev/null -w '%{http_code}' https://127.0.0.1:5986/wsman)" "401 401"

info "-- IIS dietro la web farm ARR"
id=$(submit "$(desc w1 uc21-w100 $WEB 1.0.0)")
expect_eq "deploy della 1.0.0 riuscito" "$(wait_state "$id" terminal 180)" SUCCESS
expect_eq "win1 e win2 servono la 1.0.0" "$(served win1) $(served win2)" "1.0.0 1.0.0"
expect_eq "un server alla volta in Drain e di nuovo Available" "$(history arr)" "win1:1 win1:0 win2:1 win2:0"
expect_match "per ogni lotto svuotamento, deploy e rientro" "$(step_names "$id")" \
  "drain@eu/eu-1/eu-1a#0 .*deploy@eu/eu-1/eu-1a#0 restore@eu/eu-1/eu-1a#0 .*drain@eu/eu-1/eu-1b#0"
id=$(submit "$(desc w2 uc21-w110 $WEB 1.1.0)")
expect_eq "deploy della 1.1.0 riuscito" "$(wait_state "$id" terminal 180)" SUCCESS
expect_eq "win1 e win2 servono la 1.1.0" "$(served win1) $(served win2)" "1.1.0 1.1.0"
expect_eq "la farm è di nuovo tutta Available" "$(wstate win1 arr | jq -r '[.farms["uc21-farm"][].state] | join(" ")')" "0 0"

info "-- pacchetto MSI sui nodi del cluster NLB"
id=$(submit "$(desc m1 uc21-m400 $AGENT 4.0.0 "" msi)")
expect_eq "installazione della 4.0.0 riuscita" "$(wait_state "$id" terminal 180)" SUCCESS
expect_eq "prodotto installato su win1 e win2" "$(products win1) $(products win2)" "UC21 Agent 4.0.0 UC21 Agent 4.0.0"
expect_eq "proprietà pubbliche passate a msiexec" \
  "$(wstate win1 products | jq -r '[.[].properties | "PORT=\(.PORT) ENV=\(.ENV)"] | first')" "PORT=7070 ENV=prod"
expect_eq "un nodo alla volta svuotato, fermato e riavviato" "$(history nlb)" \
  "win1:Draining win1:Stopped win1:Converged win2:Draining win2:Stopped win2:Converged"
expect_eq "pacchetto conservato nella release" "$(dc exec -T win1 ls "$(rel $AGENT 4.0.0)" | tr '\n' ' ')" "package.msi "
id=$(submit "$(desc m2 uc21-m410 $AGENT 4.1.0 "" msi)")
expect_eq "aggiornamento alla 4.1.0 riuscito" "$(wait_state "$id" terminal 180)" SUCCESS
expect_eq "MajorUpgrade: resta solo la 4.1.0" "$(products win1) $(products win2)" "UC21 Agent 4.1.0 UC21 Agent 4.1.0"
expect_eq "l'inventario legge la 4.1.0 dal prodotto installato" "$(manent inventory -a $AGENT -e prod | jq -r '.[0].current.version')" 4.1.0
id=$(submit "$(desc m3 uc21-m420 $AGENT 4.2.0 "rollback: {enabled: true}" msi)")
expect_eq "pacchetto che fallisce (1603): deploy ERROR" "$(wait_state "$id" terminal 180)" ERROR
# 1603 on Windows; on the emulated host exit codes have 8 bits (67).
expect_match "l'errore riporta l'uscita di msiexec e il log" "$(manent status "$id" | jq -r .error)" "msiexec install: exit (1603|67) \\(log in /srv/win/apps/$AGENT/releases/4.2.0\\)"
expect_eq "win1 resta sulla 4.1.0, win2 non toccato" "$(products win1) $(products win2)" "UC21 Agent 4.1.0 UC21 Agent 4.1.0"
dc exec -T -u deploy win1 sh -c 'echo "{}" > /srv/win/state/products.json'
manent rollback -a $AGENT -e prod --to 4.0.0 --timeout 3m >/dev/null
expect_eq "rollback manuale alla 4.0.0: installata dove manca, downgrade dopo la disinstallazione" "$(products win1) $(products win2)" "UC21 Agent 4.0.0 UC21 Agent 4.0.0"

info "-- attività pianificata"
id=$(submit "$(desc t1 uc21-t300 $JOB 3.0.0)")
expect_eq "deploy della 3.0.0 riuscito" "$(wait_state "$id" terminal 120)" SUCCESS
expect_eq "azione sull'eseguibile della release, con argomenti e cartella" \
  "$(task execute) $(task arguments) $(task dir)" "$(rel $JOB 3.0.0)/bin/worker.exe --all $(rel $JOB 3.0.0)"
expect_eq "run_now: attività avviata una volta, esito 0" "$(task runs) $(task last_result)" "1 0"
dc exec -T -u deploy win1 pwsh -NoProfile -c '$p = "/srv/win/state/tasks.json"; $s = Get-Content -Raw $p | ConvertFrom-Json -AsHashtable; $s["\UC21\Export"].running_until = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds() + 600; $s | ConvertTo-Json -Depth 8 | Set-Content $p'
id=$(submit "$(desc t2 uc21-t310 $JOB 3.1.0)")
expect_eq "attività in esecuzione: il deploy non la interrompe e fallisce" "$(wait_state "$id" terminal 120)" ERROR
expect_match "l'errore indica l'attività in esecuzione" "$(manent status "$id" | jq -r .error)" "still running"
expect_eq "l'attività resta sulla 3.0.0" "$(task execute)" "$(rel $JOB 3.0.0)/bin/worker.exe"
id=$(submit "$(desc t3 uc21-t311 $JOB 3.1.0 'deploy: {params: {stop_task: "true"}}')")
expect_eq "con stop_task il deploy ferma l'istanza e passa alla 3.1.0" "$(wait_state "$id" terminal 120) $(task execute)" "SUCCESS $(rel $JOB 3.1.0)/bin/worker.exe"

info "-- spegnimento degli host Windows emulati"
dc --profile windows stop win1 win2 >/dev/null 2>&1
