# UC9: deploy chains. A deploy waiting for another (after), a failing dependency, a successful
# composite release (db → api → web) and a failed one with onFailure: rollback.
# Every application has its own target: uc9-db on eu1-a, uc9-api on eu1-b, uc9-web on eu1-c.
log "UC9 · catene di deploy: dipendenze e release composte"

for v in 1.0.0 1.1.0 1.2.0 1.3.0; do
  for a in uc9-db uc9-api uc9-web; do artifact $a $v >/dev/null; done
done

# event_at <target> <app> <start|end> <version>: time (epoch) of the post_command event.
event_at() { dc exec -T "$1" sh -c "cat /home/deploy/fault/$2.log 2>/dev/null" | awk -v e="$3" -v v="$4" '$2==e && $3==v {print $1}' | tail -1; }
# after_ok <description> <target1> <app1> <target2> <app2> <version>: app2 started after app1 ended.
after_ok() {
  end=$(event_at "$2" "$3" end "$6"); start=$(event_at "$4" "$5" start "$6")
  [ -n "$end" ] && [ -n "$start" ] && [ "$start" -ge "$end" ] && pass "$1" || fail "$1" "fine $3=$end, inizio $5=$start"
}

info "-- after: api attende la migrazione del database"
fault eu1-a uc9-db sleep 8
db=$(submit "$(descriptor uc9-db-100 uc9-db-100 uc9-db prod 1.0.0)")
api=$(submit "$(descriptor uc9-api-100 uc9-api-100 uc9-api prod 1.0.0 "after: [$db]")")
expect_eq "api è in attesa delle dipendenze" "$(state "$api")" WAITING_DEPENDENCY
expect_eq "lo stato riporta la dipendenza" "$(manent status "$api" | jq -r '.after.dependencies[0].id')" "$db"
expect_eq "activity: api tra i deploy in attesa delle dipendenze" \
  "$(manent activity --json -a uc9-api | jq -r '[.waiting_dependency[].id] | join(" ")')" "$api"
expect_eq "il target di api non è toccato" "$(current eu1-b uc9-api)" none
fault eu1-a uc9-db clear
expect_eq "il database riesce" "$(wait_state "$db" terminal 90)" SUCCESS
expect_eq "poi api riesce" "$(wait_state "$api" terminal 90)" SUCCESS
after_ok "api è partita dopo la fine della migrazione" eu1-a uc9-db eu1-b uc9-api 1.0.0
expect_eq "dipendenze soddisfatte nello stato di api" "$(manent status "$api" | jq -r .after.status)" satisfied

info "-- una dipendenza fallisce: il deploy non viene eseguito"
fault eu1-a uc9-db sleep 6
fault eu1-a uc9-db fail
db=$(submit "$(descriptor uc9-db-110 uc9-db-110 uc9-db prod 1.1.0)")
api=$(submit "$(descriptor uc9-api-110 uc9-api-110 uc9-api prod 1.1.0 "after: [$db]")")
expect_eq "il database fallisce" "$(wait_state "$db" terminal 90)" ERROR
expect_eq "api si conclude in DEPENDENCY_FAILED" "$(wait_state "$api" DEPENDENCY_FAILED 60)" DEPENDENCY_FAILED
expect_match "il motivo cita la dipendenza" "$(manent status "$api" | jq -r .error)" "$db è concluso in ERROR"
expect_eq "api resta sulla 1.0.0" "$(current eu1-b uc9-api)" 1.0.0
fault eu1-a uc9-db clear
expect_match "una dipendenza già fallita è rifiutata all'invio" \
  "$(manent_err deploy --no-wait -f "$(descriptor uc9-api-111 uc9-api-111 uc9-api prod 1.1.0)" --after "$db")" "concluso in ERROR"

info "-- release composta: db, poi api, poi web"
for n in db api web; do descriptor "uc9-r1-$n" "uc9-r1-$n" "uc9-$n" prod 1.2.0 >/dev/null; done
cat >"$WORK/uc/uc9-r1.yml" <<'EOF'
apiVersion: manent/v1
kind: Release
id: uc9-r1
deploys:
  - {name: web, file: uc9-r1-web.yml, after: [api]}
  - {name: api, file: uc9-r1-api.yml, after: [db]}
  - {name: db, file: uc9-r1-db.yml}
EOF
out=$(manent release submit -f /uc/uc9-r1.yml --interval 1s)
expect_eq "la release riesce" "$(printf '%s' "$out" | jq -r .state)" SUCCESS
expect_eq "deploy in ordine topologico" "$(printf '%s' "$out" | jq -r '[.deploys[].name] | join(" ")')" "db api web"
expect_eq "tutti i target sulla 1.2.0" "$(current eu1-a uc9-db) $(current eu1-b uc9-api) $(current eu1-c uc9-web)" "1.2.0 1.2.0 1.2.0"
after_ok "api è partita dopo db" eu1-a uc9-db eu1-b uc9-api 1.2.0
after_ok "web è partita dopo api" eu1-b uc9-api eu1-c uc9-web 1.2.0
expect_eq "i deploy riportano la release" "$(manent status uc9-r1-web | jq -r .release)" uc9-r1
expect_eq "reinvio idempotente" "$(manent release submit -f /uc/uc9-r1.yml --no-wait | jq -r .id)" uc9-r1
expect_match "release list" "$(manent release list | jq -r '[.[].id] | join(" ")')" "uc9-r1"

info "-- release fallita con onFailure: rollback"
fault eu1-c uc9-web fail
for n in db api web; do descriptor "uc9-r2-$n" "uc9-r2-$n" "uc9-$n" prod 1.3.0 >/dev/null; done
cat >"$WORK/uc/uc9-r2.yml" <<'EOF'
apiVersion: manent/v1
kind: Release
id: uc9-r2
onFailure: rollback
deploys:
  - {name: db, file: uc9-r2-db.yml}
  - {name: api, file: uc9-r2-api.yml, after: [db]}
  - {name: web, file: uc9-r2-web.yml, after: [api]}
EOF
out=$(manent release submit -f /uc/uc9-r2.yml --interval 1s || true)
fault eu1-c uc9-web clear
expect_eq "la release fallisce" "$(printf '%s' "$out" | jq -r .state)" ERROR
expect_eq "web fallisce, api e db riescono e vengono riportati indietro" \
  "$(printf '%s' "$out" | jq -r '[.deploys[] | .name + "=" + .state + "/" + (.rollback_state // "-")] | join(" ")')" \
  "db=SUCCESS/SUCCESS api=SUCCESS/SUCCESS web=ERROR/-"
expect_eq "api e db di nuovo sulla 1.2.0" "$(current eu1-a uc9-db) $(current eu1-b uc9-api)" "1.2.0 1.2.0"
expect_eq "il rollback di api è un rollback manuale della release" \
  "$(manent status uc9-r2.rollback.api | jq -r '.kind + " " + .version')" "rollback 1.2.0"
