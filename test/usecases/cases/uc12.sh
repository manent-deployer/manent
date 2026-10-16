# UC12: custom steps defined in manent.yml. uc12-migrate runs a command over SSH on one target
# (run_once) before the deploy, with a parameter from the environment configuration, a secret
# in a file and a declared output; uc12-notify calls an HTTP endpoint (fakes) with bearer,
# Idempotency-Key and a JSON body built by manent, then polls the job until it is done.
# Dry-run, log redaction, temporary error (exit 75, retried) and permanent error (no retries).
APP=uc12-db
log "UC12 · step personalizzati: comando via SSH e richiesta HTTP"

# migrate script on the targets: records arguments and environment, prints the secret (the
# deploy log must show ***), simulates temporary (exit 75) and permanent errors on request.
for h in eu1-a eu1-b; do
  dc exec -T -u deploy "$h" sh -c 'mkdir -p ~/bin && cat > ~/bin/migrate && chmod 755 ~/bin/migrate && rm -f ~/uc12-migrate.log' <<'SH'
#!/bin/sh
log=$HOME/uc12-migrate.log
echo "$* param=$MANENT_PARAM_DATABASE step=$MANENT_STEP dry=$MANENT_DRY_RUN version=$MANENT_VERSION" >>"$log"
[ -f "$MANENT_SECRETS_DIR/db-pass" ] && echo "secret=$(wc -c <"$MANENT_SECRETS_DIR/db-pass" | tr -d ' ')" >>"$log"
echo "connessione al database con password $(cat "$MANENT_SECRETS_DIR/db-pass" 2>/dev/null)"
if [ -e "$HOME/uc12.tempfail" ]; then rm -f "$HOME/uc12.tempfail"; echo "database occupato" >&2; exit 75; fi
if [ -e "$HOME/uc12.fail" ]; then echo "migrazione fallita" >&2; exit 1; fi
[ "$1" = up ] && echo "applied=3" >>"$MANENT_OUTPUT"
exit 0
SH
done
mlog() { dc exec -T "$1" cat /home/deploy/uc12-migrate.log 2>/dev/null; }
output() { manent status "$1" | jq -r --arg s "$2" --arg k "$3" '.result.steps[] | select(.name == $s) | .output[$k] // empty'; }
hooks() { curl -s 'http://127.0.0.1:19100/records?kind=hook'; }

artifact $APP 1.0.0 >/dev/null
artifact $APP 1.1.0 >/dev/null
artifact $APP 1.2.0 >/dev/null
desc() { descriptor "$1" "$2" $APP prod "$3" "steps: {uc12-migrate: true, uc12-notify: true}"; }

info "-- piano: tipo e hash della specifica, mai il comando"
plan=$(manent validate -f "$(desc uc12-v uc12-v 1.0.0)")
expect_eq "il piano contiene i due step con il loro tipo" \
  "$(printf '%s' "$plan" | jq -r '[.plan[] | select(.type) | "\(.name):\(.type)"] | join(" ")')" "uc12-migrate:command uc12-notify:http"
expect_match "ogni step personalizzato ha lo sha256 della specifica" "$(printf '%s' "$plan" | jq -r '.plan[] | select(.name == "uc12-migrate") | .spec_hash')" "^[0-9a-f]{64}$"
printf '%s' "$plan" | grep -q -e /home/deploy/bin/migrate -e fakes:9100 && fail "il piano espone comando o URL" || pass "il piano non espone comando né URL"

info "-- dry-run: solo il comando di dry_run, senza i segreti di run"
out=$(manent deploy --dry-run --timeout 2m -f "$(desc uc12-dry uc12-100-dry 1.0.0)")
expect_eq "dry-run riuscito" "$(printf '%s' "$out" | jq -r .dry_run)" true
expect_eq "eseguito \"plan\" su un solo target, senza il segreto" "$(mlog eu1-a | tr '\n' '|')$(mlog eu1-b)" \
  "plan --to 1.0.0 --db shop param=shop step=uc12-migrate dry=true version=1.0.0|"
expect_eq "nessuna chiamata HTTP in dry-run (lo step non ha dry_run)" "$(hooks | jq length)" 0

info "-- rilascio: migrazione, deploy, notifica con attesa del job"
id=$(submit "$(desc uc12-a uc12-100 1.0.0)")
expect_eq "deploy riuscito" "$(wait_state "$id" terminal 180)" SUCCESS
expect_eq "migrazione su eu1-a con parametro e segreto da 14 byte" "$(mlog eu1-a | tail -2 | tr '\n' '|')" \
  "up --to 1.0.0 --db shop param=shop step=uc12-migrate dry=false version=1.0.0|secret=14|"
expect_eq "run_once: nessuna esecuzione su eu1-b" "$(mlog eu1-b | wc -l | tr -d ' ')" 0
expect_eq "output dichiarato della migrazione" "$(output "$id" uc12-migrate applied)" 3
logs=$(manent logs "$id")
expect_match "il log del deploy mostra l'output del comando con il segreto redatto" "$logs" "connessione al database con password \*\*\*"
printf '%s' "$logs" | grep -q s3cret-uc12-db && fail "il segreto compare nel log del deploy" || pass "il segreto non compare nel log del deploy"
req=$(hooks | jq -c '[.[] | select(.method == "POST")][0]')
expect_eq "richiesta HTTP con bearer e Idempotency-Key <deploy>:<step>" \
  "$(printf '%s' "$req" | jq -r '.headers.Authorization + " " + .headers["Idempotency-Key"]')" "Bearer tok-uc12-hook $id:uc12-notify"
expect_eq "corpo JSON costruito da manent, con l'output della migrazione" \
  "$(printf '%s' "$req" | jq -c .body)" '{"application":"uc12-db","applied":"3","version":"1.0.0"}'
expect_eq "il job è stato interrogato fino a done" "$(hooks | jq '[.[] | select(.method == "GET")] | length')" 2
expect_match "output della notifica letto dalla risposta" "$(output "$id" uc12-notify ticket)" "^TCK-[0-9]+$"
expect_eq "rilascio completato sui due target" "$(currents $APP eu1-a eu1-b)" "1.0.0 1.0.0"

info "-- errore temporaneo (exit 75): ritentato"
dc exec -T -u deploy eu1-a touch /home/deploy/uc12.tempfail
id=$(submit "$(desc uc12-b uc12-110 1.1.0)")
expect_eq "deploy riuscito al secondo tentativo" "$(wait_state "$id" terminal 180)" SUCCESS
expect_eq "la migrazione della 1.1.0 è stata eseguita due volte" "$(mlog eu1-a | grep -c '^up --to 1.1.0')" 2

info "-- errore permanente: nessun tentativo in più e target non toccati"
dc exec -T -u deploy eu1-a touch /home/deploy/uc12.fail
id=$(submit "$(desc uc12-c uc12-120 1.2.0)")
expect_eq "deploy in errore" "$(wait_state "$id" terminal 180)" ERROR
expect_eq "una sola esecuzione della migrazione" "$(mlog eu1-a | grep -c '^up --to 1.2.0')" 1
expect_match "l'errore riporta il codice di uscita" "$(manent status "$id" | jq -r .error)" "uc12-migrate"
expect_eq "il deploy non è partito: target sulla 1.1.0" "$(currents $APP eu1-a eu1-b)" "1.1.0 1.1.0"
dc exec -T -u deploy eu1-a rm -f /home/deploy/uc12.fail
