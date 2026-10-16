# UC4: wave rollout in one region (eu-1): canary in AZ eu-1a, approval, then the other AZs
# one at a time, with bake and health gate (/healthz probe) between passes.
# prod environment: onFailure rollback-all; prod-stop environment: onFailure stop.
APP=uc4-web
log "UC4 · rollout a ondate per AZ in una region"

artifact $APP 1.0.0 >/dev/null
artifact $APP 1.1.0 broken >/dev/null
artifact $APP 1.2.0 >/dev/null
artifact $APP 1.3.0 >/dev/null

stages() { manent status "$1" | jq -r '[.rollout.stages[] | "\(.name)=\(.state)"] | join(" ")'; }
started() { dc exec -T "$1" sh -c "cat /home/deploy/fault/$APP.log 2>/dev/null | grep -c 'start $2' || true"; }

info "-- piano"
d=$(descriptor uc4-a uc4-100 $APP prod 1.0.0)
expect_eq "validate mostra gli stadi: canary, poi una AZ alla volta" \
  "$(manent validate -f "$d" | jq -r '[.rollout.stages[].name] | join(" ")')" "canary resto/eu-1b resto/eu-1c"

info "-- primo rilascio con approvazione dopo il canary"
id=$(submit "$d")
expect_eq "dopo il canary il rollout attende l'approvazione" "$(wait_rollout "$id" paused-approval 90)" paused-approval
expect_eq "lo stato del deploy è AWAITING_APPROVAL" "$(state "$id")" AWAITING_APPROVAL
expect_eq "solo il canary (eu1-a) è aggiornato" "$(currents $APP eu1-a eu1-b eu1-c)" "1.0.0 none none"
as cab manent approve "$id" >/dev/null
expect_eq "approvato, il rollout prosegue e riesce" "$(wait_state "$id" terminal 120)" SUCCESS
expect_eq "tutte le AZ sulla 1.0.0" "$(currents $APP eu1-a eu1-b eu1-c)" "1.0.0 1.0.0 1.0.0"
expect_eq "tutti gli stadi superati" "$(stages "$id")" "canary=succeeded resto/eu-1b=succeeded resto/eu-1c=succeeded"
expect_eq "rollout_progress 3/3" "$(manent status "$id" | jq -r .rollout_progress)" 3/3
expect_match "uno step per passo e lotto (deploy@resto/eu-1/eu-1c#0)" "$(step_names "$id")" "deploy@resto/eu-1/eu-1c#0"

info "-- release difettosa: health gate rosso sul canary"
id=$(submit "$(descriptor uc4-b uc4-110-broken $APP prod 1.1.0)")
expect_eq "deploy ERROR" "$(wait_state "$id" terminal 90)" ERROR
st=$(manent status "$id")
expect_eq "rollout fallito sul canary" "$(printf '%s' "$st" | jq -r '.rollout.status + " " + .rollout.stage')" "failed canary"
expect_match "il gate riporta la sonda rossa" "$(printf '%s' "$st" | jq -r '.rollout.stages[0].gate')" "eu1-a"
expect_eq "il canary è tornato alla 1.0.0, le altre AZ non sono state toccate" "$(currents $APP eu1-a eu1-b eu1-c)" "1.0.0 1.0.0 1.0.0"
expect_eq "nessuna esecuzione della 1.1.0 su eu1-b ed eu1-c" "$(started eu1-b 1.1.0) $(started eu1-c 1.1.0)" "0 0"
expect_eq "canary riportato indietro, stadi resto mai avviati" "$(stages "$id")" "canary=rolled-back resto/eu-1b=pending resto/eu-1c=pending"

info "-- guasto nell'ultima AZ: rollback-all"
fault eu1-c $APP fail
id=$(submit "$(descriptor uc4-c uc4-120-fault $APP prod 1.2.0)")
expect_eq "canary superato, attesa approvazione" "$(wait_rollout "$id" paused-approval 90)" paused-approval
as cab manent approve "$id" >/dev/null
expect_eq "il passo resto/eu-1c fallisce: deploy ERROR" "$(wait_state "$id" terminal 120)" ERROR
expect_eq "eu-1a ed eu-1b erano stati aggiornati" "$(started eu1-a 1.2.0) $(started eu1-b 1.2.0)" "1 1"
expect_eq "rollback-all: tutte le AZ di nuovo sulla 1.0.0" "$(currents $APP eu1-a eu1-b eu1-c)" "1.0.0 1.0.0 1.0.0"
expect_eq "esito del rollback" "$(manent status "$id" | jq -r .rollback)" completed
fault eu1-c $APP clear

info "-- approvazione rifiutata"
id=$(submit "$(descriptor uc4-d uc4-130-reject $APP prod 1.3.0)")
expect_eq "canary sulla 1.3.0, attesa approvazione" "$(wait_rollout "$id" paused-approval 90)" paused-approval
expect_eq "eu1-a sulla 1.3.0" "$(current eu1-a $APP)" 1.3.0
as cab manent reject -m "metriche del canary non convincenti" "$id" >/dev/null
expect_eq "rifiuto: rollout fermato" "$(wait_state "$id" terminal 60)" ERROR
expect_eq "il canary è tornato alla 1.0.0" "$(currents $APP eu1-a eu1-b eu1-c)" "1.0.0 1.0.0 1.0.0"

info "-- stessa versione di nuovo, dopo la correzione"
id=$(submit "$(descriptor uc4-e uc4-120-retry $APP prod 1.2.0)")
wait_rollout "$id" paused-approval 90 >/dev/null
as cab manent approve "$id" >/dev/null
expect_eq "il nuovo rilascio della 1.2.0 riesce" "$(wait_state "$id" terminal 120)" SUCCESS
expect_eq "tutte le AZ sulla 1.2.0" "$(currents $APP eu1-a eu1-b eu1-c)" "1.2.0 1.2.0 1.2.0"

info "-- onFailure stop (ambiente prod-stop, release in /home/deploy/apps/uc4-web-stop)"
id=$(submit "$(descriptor uc4-f uc4-stop-100 $APP prod-stop 1.0.0)")
wait_rollout "$id" paused-approval 90 >/dev/null
as cab manent approve "$id" >/dev/null
expect_eq "primo rilascio in prod-stop" "$(wait_state "$id" terminal 120)" SUCCESS
fault eu1-c $APP fail
id=$(submit "$(descriptor uc4-g uc4-stop-120 $APP prod-stop 1.2.0)")
wait_rollout "$id" paused-approval 90 >/dev/null
as cab manent approve "$id" >/dev/null
expect_eq "guasto in eu-1c: deploy ERROR" "$(wait_state "$id" terminal 120)" ERROR
expect_eq "stop: nessun rollback" "$(manent status "$id" | jq -r '.rollback // "nessuno"')" "nessuno"
expect_eq "i passi superati restano sulla 1.2.0" "$(currents uc4-web-stop eu1-a eu1-b)" "1.2.0 1.2.0"
expect_eq "l'inventario segnala il drift" "$(manent inventory -a $APP -e prod-stop | jq -r '.[0].drift')" true
fault eu1-c $APP clear
