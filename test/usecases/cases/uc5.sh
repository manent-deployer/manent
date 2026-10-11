# UC5: rilascio a ondate su due region (eu-1, us-1) con un worker per region e routing dei
# passi: canary in eu-1a, approvazione, poi un'AZ per region alla volta con le due region in
# parallelo. Ogni worker di region vede solo la rete dei suoi target.
APP=uc5-global
log "UC5 · rollout multi-region con worker di region"

for v in 1.0.0 1.1.0 1.2.0; do artifact $APP $v >/dev/null; done

info "-- topologia: orchestratore e un worker per region al posto dei worker generici"
dc stop worker-1 worker-2 >/dev/null 2>&1
dc --profile regions up -d orchestrator worker-eu1 worker-us1 >/dev/null 2>&1
nets() { docker inspect "$(dc --profile regions ps -q "$1")" --format '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}'; }
expect_eq "worker-eu1 vede solo la rete di eu-1" "$(nets worker-eu1)" "manent-uc_core manent-uc_eu1 "
expect_eq "worker-us1 vede solo la rete di us-1" "$(nets worker-us1)" "manent-uc_core manent-uc_us1 "
sleep 5

host_logs() { dc --profile regions logs "$1" 2>/dev/null | grep "deployment=$2 " | grep -o 'host=[a-z0-9-]*' | sort -u | tr '\n' ' '; }
stages() { manent status "$1" | jq -r '[.rollout.stages[] | "\(.name)=\(.state)"] | join(" ")'; }
started_at() { dc exec -T "$1" sh -c "awk '\$2==\"start\" && \$3==\"$2\" {print \$1}' /home/deploy/fault/$APP.log | tail -1"; }
ALL="eu1-a eu1-b eu1-c us1-a us1-b"

info "-- piano"
d=$(descriptor uc5-a uc5-100 $APP prod 1.0.0)
expect_eq "stadi: canary, poi le due region in parallelo un'AZ alla volta" \
  "$(manent validate -f "$d" | jq -r '[.rollout.stages[].name] | join(" ")')" "canary resto/eu-1b+us-1a resto/eu-1c+us-1b"

info "-- primo rilascio"
fault eu1-b $APP sleep 3
fault us1-a $APP sleep 3
id=$(submit "$d")
expect_eq "dopo il canary il rollout attende l'approvazione" "$(wait_rollout "$id" paused-approval 90)" paused-approval
expect_eq "solo il canary è aggiornato" "$(currents $APP $ALL)" "1.0.0 none none none none"
as cab manent approve "$id" >/dev/null
expect_eq "il rollout multi-region riesce" "$(wait_state "$id" terminal 150)" SUCCESS
expect_eq "tutti i target sulla 1.0.0" "$(currents $APP $ALL)" "1.0.0 1.0.0 1.0.0 1.0.0 1.0.0"
fault eu1-b $APP clear
fault us1-a $APP clear
expect_eq "worker-eu1 ha rilasciato solo su eu-1" "$(host_logs worker-eu1 "$id")" "host=eu1-a host=eu1-b host=eu1-c "
expect_eq "worker-us1 ha rilasciato solo su us-1" "$(host_logs worker-us1 "$id")" "host=us1-a host=us1-b "
expect_eq "l'orchestratore non ha toccato i target" "$(host_logs orchestrator "$id")" ""
e=$(started_at eu1-b 1.0.0); u=$(started_at us1-a 1.0.0)
d2=$(( e > u ? e - u : u - e ))
[ "$d2" -le 2 ] && pass "eu-1b e us-1a aggiornate in parallelo (scarto ${d2}s)" || fail "eu-1b e us-1a non in parallelo" "eu1-b=$e us1-a=$u"
expect_match "i log dei passi sono nel log del deploy" "$(manent logs "$id")" "us1-b"

info "-- guasto in us-1b: rollback-wave"
fault us1-b $APP fail
id=$(submit "$(descriptor uc5-b uc5-110-fault $APP prod 1.1.0)")
wait_rollout "$id" paused-approval 90 >/dev/null
as cab manent approve "$id" >/dev/null
expect_eq "il passo in us-1b fallisce: deploy ERROR" "$(wait_state "$id" terminal 150)" ERROR
expect_eq "stadio fallito: resto/eu-1c+us-1b" "$(manent status "$id" | jq -r .rollout.stage)" "resto/eu-1c+us-1b"
expect_eq "rollback-wave: solo lo stadio fallito torna alla 1.0.0" "$(currents $APP $ALL)" "1.1.0 1.1.0 1.0.0 1.1.0 1.0.0"
expect_eq "l'inventario segnala il drift" "$(manent inventory -a $APP -e prod | jq -r '.[0].drift')" true
fault us1-b $APP clear

info "-- worker di region spento: il passo aspetta il suo ritorno"
dc --profile regions stop worker-us1 >/dev/null 2>&1
id=$(submit "$(descriptor uc5-c uc5-120 $APP prod 1.2.0)")
wait_rollout "$id" paused-approval 90 >/dev/null
as cab manent approve "$id" >/dev/null
sleep 15
expect_eq "senza worker in us-1 il rollout resta in esecuzione" "$(state "$id")" PENDING
expect_eq "la parte eu-1 dello stadio è stata eseguita" "$(current eu1-b $APP)" 1.2.0
expect_eq "us1-a non è toccato" "$(current us1-a $APP)" 1.1.0
dc --profile regions start worker-us1 >/dev/null 2>&1
expect_eq "con il worker di nuovo attivo il rollout riesce" "$(wait_state "$id" terminal 150)" SUCCESS
expect_eq "tutti i target sulla 1.2.0" "$(currents $APP $ALL)" "1.2.0 1.2.0 1.2.0 1.2.0 1.2.0"

info "-- freeze della sola region us-1 (ambiente prod-us-freeze)"
id=$(submit "$(descriptor uc5-d uc5-freeze $APP prod-us-freeze 1.0.0)")
expect_eq "il rollout si ferma prima degli stadi con us-1" "$(wait_rollout "$id" paused-freeze 90)" paused-freeze
expect_match "lo stato indica il freeze" "$(manent status "$id" | jq -r .rollout.paused)" "uc5-us-congelata"
manent cancel "$id" >/dev/null
expect_eq "il rollout congelato si annulla" "$(wait_state "$id" terminal 30)" CANCELLED

info "-- ripristino dei worker generici"
dc --profile regions stop orchestrator worker-eu1 worker-us1 >/dev/null 2>&1
dc start worker-1 worker-2 >/dev/null 2>&1
