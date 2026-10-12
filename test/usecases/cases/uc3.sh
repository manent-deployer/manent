# UC3: più worker sullo stesso Postgres. Deploy serializzati nello stesso ambiente e paralleli
# tra ambienti, worker che cade durante un deploy (recovery automatica da parte dell'altro) e
# arresto ordinato (drain) che attende il deploy in corso.
APP=uc3-batch
log "UC3 · più worker: concorrenza, caduta di un worker e drain"

for v in 1.0.0 1.1.0 1.2.0 1.3.0; do artifact $APP $v >/dev/null; done

# executor_of <id>: il worker che ha eseguito l'ultimo step del deploy (dai log).
executor_of() {
  dc logs -t worker-1 worker-2 2>/dev/null | grep "deployment=$1 " | awk '{print $3, $1}' | sort | tail -1 | awk '{print $2}' | sed 's/-1$//'
}
# started <target> <versione>: esecuzioni del post_command per la versione.
started() { dc exec -T "$1" sh -c "cat /home/deploy/fault/$APP.log 2>/dev/null | grep -c 'start $2' || true"; }
# wait_started <target> <versione>: attende che il post_command della versione sia in corso.
wait_started() {
  t=0
  while [ "$(started "$1" "$2")" = 0 ] && [ "$t" -lt 60 ]; do sleep 1; t=$((t + 1)); done
}

info "-- concorrenza: un deploy alla volta per ambiente, ambienti diversi in parallelo"
fault eu1-b $APP sleep 6
fault eu1-c $APP sleep 6
a=$(submit "$(descriptor uc3-a uc3-100-test $APP test 1.0.0)")
b=$(submit "$(descriptor uc3-b uc3-110-test $APP test 1.1.0)")
c=$(submit "$(descriptor uc3-c uc3-100-qa $APP qa 1.0.0)")
for id in $a $b $c; do expect_eq "deploy $id riuscito" "$(wait_state "$id" terminal 90)" SUCCESS; done
fault eu1-b $APP clear
fault eu1-c $APP clear
logb=$(dc exec -T eu1-b cat /home/deploy/fault/$APP.log)
logc=$(dc exec -T eu1-c cat /home/deploy/fault/$APP.log)
end1=$(printf '%s\n' "$logb" | awk '$2=="end" && $3=="1.0.0" {print $1}')
start2=$(printf '%s\n' "$logb" | awk '$2=="start" && $3=="1.1.0" {print $1}')
startqa=$(printf '%s\n' "$logc" | awk '$2=="start" && $3=="1.0.0" {print $1}')
start1=$(printf '%s\n' "$logb" | awk '$2=="start" && $3=="1.0.0" {print $1}')
[ "$start2" -ge "$end1" ] && pass "in test il secondo deploy parte dopo la fine del primo" \
  || fail "deploy sovrapposti nello stesso ambiente" "fine 1.0.0=$end1, inizio 1.1.0=$start2"
[ "$startqa" -lt "$end1" ] && [ "$startqa" -ge "$((start1 - 6))" ] && pass "qa è eseguito in parallelo a test" \
  || fail "qa non è stato eseguito in parallelo" "qa=$startqa, test=$start1..$end1"
expect_eq "eu1-b (test) sulla 1.1.0, eu1-c (qa) sulla 1.0.0" "$(currents $APP eu1-b eu1-c)" "1.1.0 1.0.0"

info "-- un worker cade durante il deploy"
fault eu1-b $APP sleep 30
id=$(submit "$(descriptor uc3-d uc3-120-crash $APP test 1.2.0)")
wait_started eu1-b 1.2.0
w=$(executor_of "$id")
expect_match "il deploy è in esecuzione su un worker" "$w" "^worker-[12]$"
expect_eq "activity: il deploy è tra quelli in esecuzione, su $w" \
  "$(manent activity --json -a $APP | jq -r --arg id "$id" '.running[] | select(.id == $id) | .executor')" "$w"
expect_match "workers: $w è attivo e sta eseguendo il deploy" \
  "$(manent workers --json | jq -r --arg w "$w" '.[] | select(.id == $w) | .state + " " + ([.tasks[].deployment_id] | join(","))')" "^alive .*$id"
expect_match "workers richiede platform.read (viewer: 403)" "$(as viewer manent_err workers)" "platform.read"
dc kill "$w" >/dev/null 2>&1
fault eu1-b $APP clear
info "$w terminato con SIGKILL; recovery automatica attesa dopo ~10s"
expect_eq "il deploy è ripreso dall'altro worker e riesce" "$(wait_state "$id" terminal 120)" SUCCESS
other=$( [ "$w" = worker-1 ] && echo worker-2 || echo worker-1 )
expect_eq "l'ultimo step è stato eseguito da $other" "$(executor_of "$id")" "$other"
expect_eq "eu1-b sulla 1.2.0" "$(current eu1-b $APP)" 1.2.0
expect_eq "solo lo step interrotto è stato rieseguito (2 esecuzioni)" "$(started eu1-b 1.2.0)" 2
expect_match "il worker caduto è stato ripreso da $other" "$(dc logs "$other" 2>/dev/null | grep -i recover | tail -3)" "$w"
expect_eq "workers: $w risulta perso e ripreso da $other" \
  "$(manent workers --json | jq -r --arg w "$w" '.[] | select(.id == $w) | .state + " " + .recovered_by')" "stale executor:$other"
dc start "$w" >/dev/null 2>&1
sleep 8
expect_eq "workers: $w di nuovo attivo dopo il riavvio" "$(manent workers --json | jq -r --arg w "$w" '.[] | select(.id == $w) | .state')" alive
expect_eq "al riavvio il worker caduto non riesegue il deploy" "$(started eu1-b 1.2.0)" 2

info "-- arresto ordinato (drain)"
fault eu1-b $APP sleep 10
id=$(submit "$(descriptor uc3-e uc3-130-drain $APP test 1.3.0)")
wait_started eu1-b 1.3.0
w=$(executor_of "$id")
t0=$(date +%s)
dc stop "$w" >/dev/null 2>&1
el=$(( $(date +%s) - t0 ))
fault eu1-b $APP clear
[ "$el" -ge 5 ] && pass "lo stop di $w attende il deploy in corso (${el}s)" || fail "lo stop di $w non ha atteso il deploy" "${el}s"
expect_eq "il deploy si conclude prima dell'arresto" "$(state "$id")" SUCCESS
expect_eq "eseguito una sola volta" "$(started eu1-b 1.3.0)" 1
dc start "$w" >/dev/null 2>&1
