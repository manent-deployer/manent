# UC1: deploy pianificato (schedule.at) su un target in una AZ e su tre AZ di una region,
# senza rollout: tutti i target nello stesso step. Fallimenti, rollback automatico e manuale.
APP=uc1-shop
log "UC1 · deploy pianificato su una AZ e su più AZ di una region"

artifact $APP 1.0.0 >/dev/null
artifact $APP 1.1.0 >/dev/null
artifact $APP 1.2.0 broken >/dev/null
artifact $APP 1.2.1 >/dev/null
artifact $APP 1.3.0 >/dev/null

# Post-test sull'endpoint dell'applicazione e rollback automatico richiesto dal descrittore.
checks_test='steps: {post-test: true}
postTest:
  http:
    - {url: "http://eu1-a:8080/uc1-shop/current/healthz"}'
rb_on='rollback: {enabled: true}'

info "-- un target (eu1-a), ambiente test"

at=$(at_in 20)
d=$(descriptor uc1-a uc1-100-test $APP test 1.0.0 "schedule: {at: $at}
$rb_on
$checks_test")
id=$(submit "$d")
expect_eq "schedule.at: il deploy è accodato in DELAYED" "$(state "$id")" DELAYED
expect_eq "idempotenza: lo stesso descrittore restituisce lo stesso deploy" "$(submit "$d")" "$id"
expect_eq "idempotenza: un solo deploy accodato" "$(manent list -a $APP -e test | jq length)" 1
expect_eq "schedule.at: scheduled_at è l'orario richiesto" "$(manent status "$id" | jq -r .scheduled_at)" "$at"
sleep 5
expect_eq "prima dell'orario il target non è toccato" "$(current eu1-a $APP)" none
expect_eq "all'orario il deploy riesce" "$(wait_state "$id" terminal 90)" SUCCESS
expect_eq "una sola esecuzione sul target" "$(runs eu1-a $APP)" 1
expect_eq "eu1-a punta alla release 1.0.0" "$(current eu1-a $APP)" 1.0.0
healthy eu1-a $APP && pass "healthz della 1.0.0 risponde 200" || fail "healthz della 1.0.0 non risponde"
expect_match "step eseguiti, post-test compreso" "$(step_names "$id")" "secrets artifacts qa .*deploy post-test activation"
expect_eq "idempotenza dopo l'orario: il reinvio restituisce il deploy concluso" "$(submit "$d")" "$id"
expect_eq "dopo il reinvio nessuna nuova esecuzione" "$(runs eu1-a $APP)" 1


d=$(descriptor uc1-cancel uc1-110-cancel $APP test 1.1.0 "schedule: {at: $(at_in 60)}")
id=$(submit "$d")
expect_eq "deploy pianificato annullabile: DELAYED" "$(state "$id")" DELAYED
manent cancel "$id" >/dev/null
expect_eq "dopo cancel lo stato è CANCELLED" "$(state "$id")" CANCELLED
expect_eq "il target resta sulla 1.0.0" "$(current eu1-a $APP)" 1.0.0

d=$(descriptor uc1-b uc1-110-test $APP test 1.1.0 "schedule: {at: $(at_in 8)}
$rb_on
$checks_test")
id=$(submit "$d")
expect_eq "aggiornamento pianificato alla 1.1.0" "$(wait_state "$id" terminal 60)" SUCCESS
expect_eq "eu1-a punta alla 1.1.0" "$(current eu1-a $APP)" 1.1.0
expect_eq "le release precedenti restano (layout releases)" \
  "$(dc exec -T eu1-a ls /home/deploy/apps/$APP/releases | tr '\n' ' ')" "1.0.0 1.1.0 "

info "-- fallimenti con rollback automatico"

d=$(descriptor uc1-c uc1-120-broken $APP test 1.2.0 "$rb_on
$checks_test")
id=$(submit "$d")
expect_eq "release senza healthz: post-test fallisce e il deploy è ERROR" "$(wait_state "$id" terminal 90)" ERROR
st=$(manent status "$id")
expect_eq "rollback automatico completato" "$(printf '%s' "$st" | jq -r .rollback)" completed
expect_match "l'errore indica il post-test" "$(printf '%s' "$st" | jq -r .error)" "post-test"
expect_eq "eu1-a è tornato alla 1.1.0" "$(current eu1-a $APP)" 1.1.0
healthy eu1-a $APP && pass "healthz di nuovo verde dopo il rollback" || fail "healthz rosso dopo il rollback"

fault eu1-a $APP fail
d=$(descriptor uc1-d uc1-121-fault $APP test 1.2.1 "$rb_on")
id=$(submit "$d")
expect_eq "post_command in errore sul target: deploy ERROR" "$(wait_state "$id" terminal 90)" ERROR
expect_eq "rollback automatico completato" "$(manent status "$id" | jq -r .rollback)" completed
expect_eq "eu1-a è tornato alla 1.1.0" "$(current eu1-a $APP)" 1.1.0
fault eu1-a $APP clear

runs_before=$(runs eu1-a $APP)
d=$(descriptor uc1-e uc1-130-sha $APP test 1.3.0 "  sha256: \"$(printf '0%.0s' $(seq 64))\"
$rb_on")
id=$(submit "$d")
expect_eq "sha256 errato: deploy ERROR" "$(wait_state "$id" terminal 60)" ERROR
expect_match "l'errore indica lo sha256" "$(manent status "$id" | jq -r .error)" "sha256"
expect_eq "nessuna esecuzione sul target" "$(runs eu1-a $APP)" "$runs_before"
expect_eq "eu1-a resta sulla 1.1.0" "$(current eu1-a $APP)" 1.1.0

info "-- fallimento senza rollback e ripristino"

d=$(descriptor uc1-f uc1-120-norb $APP test 1.2.0 "rollback: {enabled: false}
$checks_test")
id=$(submit "$d")
expect_eq "release difettosa senza rollback: ERROR" "$(wait_state "$id" terminal 90)" ERROR
expect_eq "senza rollback il target resta sulla release difettosa" "$(current eu1-a $APP)" 1.2.0
healthy eu1-a $APP && fail "healthz dovrebbe essere rosso sulla 1.2.0" || pass "healthz rosso sulla 1.2.0"
inv=$(manent inventory -a $APP -e test)
expect_eq "l'inventario riporta l'ultima release riuscita (1.1.0)" "$(printf '%s' "$inv" | jq -r '.[0].current.version')" 1.1.0
manent rollback -a $APP -e test --to 1.1.0 --timeout 2m >/dev/null
expect_eq "rollback manuale all'ultima release riuscita (1.1.0)" "$(current eu1-a $APP)" 1.1.0
healthy eu1-a $APP && pass "healthz di nuovo verde dopo il ripristino" || fail "healthz rosso dopo il ripristino"
err=$(manent_err rollback -a $APP -e test --to 1.1.0)
expect_match "dopo il ripristino la 1.1.0 è di nuovo in esercizio" "$err" "già in esercizio"

manent rollback -a $APP -e test --timeout 2m >/dev/null
expect_eq "rollback manuale alla release precedente (1.0.0)" "$(current eu1-a $APP)" 1.0.0
rb=$(manent list -a $APP -e test -n 1 | jq -r '.[0] | "\(.kind) \(.state)"')
expect_eq "il rollback manuale è un deploy di tipo rollback" "$rb" "rollback SUCCESS"

info "-- dry-run"
runs_before=$(runs eu1-a $APP)
d=$(descriptor uc1-h uc1-110-dry $APP test 1.1.0)
out=$(manent deploy --dry-run -f "$d")
expect_eq "il dry-run riesce" "$(printf '%s' "$out" | jq -r .dry_run)" true
expect_eq "il dry-run non modifica il target" "$(current eu1-a $APP)" 1.0.0
expect_eq "il dry-run non esegue comandi sul target" "$(runs eu1-a $APP)" "$runs_before"

info "-- tre AZ della region eu-1 (eu1-a, eu1-b, eu1-c), ambiente staging"

d=$(descriptor uc1-s1 uc1-100-staging $APP staging 1.0.0 "schedule: {at: $(at_in 10)}
$rb_on")
id=$(submit "$d")
expect_eq "deploy pianificato su tre AZ: DELAYED" "$(state "$id")" DELAYED
expect_eq "il deploy su tre AZ riesce" "$(wait_state "$id" terminal 60)" SUCCESS
expect_eq "tutte le AZ sulla 1.0.0" "$(currents $APP eu1-a eu1-b eu1-c)" "1.0.0 1.0.0 1.0.0"

before=$(dc exec -T eu1-a sh -c "grep -c 'start 1.1.0' /home/deploy/fault/$APP.log || true")
fault eu1-c $APP fail
d=$(descriptor uc1-s2 uc1-110-staging-fault $APP staging 1.1.0 "schedule: {at: $(at_in 5)}
$rb_on")
id=$(submit "$d")
expect_eq "guasto sulla terza AZ: deploy ERROR" "$(wait_state "$id" terminal 90)" ERROR
expect_eq "rollback completato" "$(manent status "$id" | jq -r .rollback)" completed
expect_eq "tutte le AZ di nuovo sulla 1.0.0" "$(currents $APP eu1-a eu1-b eu1-c)" "1.0.0 1.0.0 1.0.0"
after=$(dc exec -T eu1-a sh -c "grep -c 'start 1.1.0' /home/deploy/fault/$APP.log || true")
[ "$after" -gt "$before" ] && pass "eu1-a era stato aggiornato prima del guasto (e riportato indietro)" || fail "eu1-a non risulta aggiornato prima del guasto"
fault eu1-c $APP clear

dc stop eu1-c >/dev/null 2>&1
d=$(descriptor uc1-s3 uc1-110-staging-down $APP staging 1.1.0 "$rb_on")
id=$(submit "$d")
expect_eq "target eu1-c spento: deploy ERROR" "$(wait_state "$id" terminal 120)" ERROR
expect_match "l'errore indica il target irraggiungibile" "$(manent status "$id" | jq -r .error)" "eu1-c"
expect_eq "eu1-a ed eu1-b non sono stati toccati" "$(currents $APP eu1-a eu1-b)" "1.0.0 1.0.0"
dc start eu1-c >/dev/null 2>&1
sleep 3
d=$(descriptor uc1-s4 uc1-110-staging $APP staging 1.1.0 "$rb_on")
id=$(submit "$d")
expect_eq "con il target di nuovo attivo il deploy riesce" "$(wait_state "$id" terminal 90)" SUCCESS
expect_eq "tutte le AZ sulla 1.1.0" "$(currents $APP eu1-a eu1-b eu1-c)" "1.1.0 1.1.0 1.1.0"
