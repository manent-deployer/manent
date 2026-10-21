# UC25: permissions as a Rego policy (rbac.engine: opa). A revision gives the team uc25-api and
# the applications of the tenants tagged with its name, an on-call shift that expires with
# until, and denies production to contractors (OIDC claim employee_type). The access simulation shows gains,
# losses and denials before the approval. A revision that removes every permission is fixed by
# whoever has the static policy.* of manent.yml. At the end the default policy comes back.
APP=uc25-api
log "UC25 · permessi come policy Rego: contractor, team proprietari dei tenant, reperibilità con until, revisione che toglie tutto"

for v in 1.0.0 1.1.0 1.2.0; do artifact $APP $v >/dev/null; done

# jwt <user>: access token of an OIDC user of the lab (test/usecases/oidc).
jwt() { curl -fsS "http://127.0.0.1:19000/lab/token?user=$1"; }
# with_jwt <token> <manent args>: the client with an OIDC identity; with_jwt_err returns stderr.
with_jwt() { tok=$1; shift; dc run --rm -T -e MANENT_TOKEN="$tok" client "$@" 2>>"$WORK/client.log"; }
with_jwt_err() { tok=$1; shift; dc run --rm -T -e MANENT_TOKEN="$tok" client "$@" 2>&1 >/dev/null | grep -v -e '^ *Container' -e '^time='; }
DAVE=$(jwt dave)
CAROL=$(jwt carol)

info "-- policy predefinita: i binding di manent.yml"
me=$(as admin manent whoami)
expect_eq "motore dei permessi opa" "$(printf '%s' "$me" | jq -r .authz_engine)" opa
expect_match "i permessi policy.* dell'admin vengono dalla configurazione statica" \
  "$(printf '%s' "$me" | jq -r '[.grants[].name] | join(",")')" "\\(static\\)"
d_test=$(descriptor uc25-a uc25-100-test $APP test 1.0.0)
err=$(with_jwt_err "$DAVE" deploy --no-wait -f "$d_test")
expect_match "senza revisione il team non ha permessi su uc25" "$err" "oidc:dave cannot perform deployment.submit"

info "-- nuova revisione: simulazione degli accessi"
rm -rf "$WORK/uc/uc25-policy" && cp -R cases/uc25-policy "$WORK/uc/uc25-policy" && mkdir -p "$WORK/uc/uc25-policy/oncall"
until=$(date -u -v+1H +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d '+1 hour' +%Y-%m-%dT%H:%M:%SZ)
printf '{"shifts": [{"subject": "token:viewer", "until": "%s"}, {"subject": "token:cab", "until": "2020-01-01T00:00:00Z"}]}\n' "$until" \
  >"$WORK/uc/uc25-policy/oncall/data.json"
out=$(as pol manent policy push -d /uc/uc25-policy -m "uc25: team, proprietari dei tenant, reperibilità, contractor")
rev=$(printf '%s' "$out" | jq -r .revision)
expect_eq "i tre test della revisione superati al caricamento" "$(printf '%s' "$out" | jq '[.tests[] | select(.pass)] | length')" 3
sim=$(as pol manent policy access "$rev" --subject oidc:carol --group team-uc25 --claim employee_type=contractor --compare \
  --action deployment.submit -a $APP -e prod --json)
expect_eq "il team guadagna uc25 e i tenant di cui è proprietario" "$(printf '%s' "$sim" | jq -r '[.gained[].name] | sort | join(",")')" "owner team-uc25,team-uc25"
expect_eq "carol in produzione sarebbe negata..." "$(printf '%s' "$sim" | jq -r '"\(.check.allowed) \(.check.granted_by)"')" "false team-uc25"
expect_match "...con il motivo del divieto" "$(printf '%s' "$sim" | jq -r '.check.denied[0]')" "contractors cannot change production"
sim=$(as pol manent policy access "$rev" --subject oidc:dave --group team-uc25 --action deployment.submit -a uc24-shop -e prod --json)
expect_eq "il team rilascia uc24-shop (tag owner del tenant payments)" "$(printf '%s' "$sim" | jq -r .check.granted_by)" "owner team-uc25"
sim=$(as pol manent policy access "$rev" --subject oidc:dave --group team-uc25 --action deployment.submit -a uc24-legacy -e prod --json)
expect_eq "ma non uc24-legacy (tenant legacy, senza il tag)" "$(printf '%s' "$sim" | jq -r .check.allowed)" false
err=$(as viewer manent_err policy access "$rev" --subject oidc:dave)
expect_match "la simulazione richiede policy.write" "$err" "policy.write"

info "-- quattro occhi e attivazione"
err=$(as pol manent_err policy approve "$rev")
expect_match "chi scrive la policy non la attiva" "$err" "policy.approve"
as admin manent policy approve "$rev" >/dev/null 2>&1
sleep 3
expect_match "la revisione è in vigore" "$(as admin manent policy list | jq -r .current)" "^$rev:"

info "-- la revisione in azione"
id=$(with_jwt "$DAVE" deploy --no-wait -f "$d_test" | jq -r .id)
expect_eq "dave rilascia in test" "$(wait_state "$id" terminal 60)" SUCCESS
id=$(with_jwt "$DAVE" deploy --no-wait -f "$(descriptor uc25-b uc25-100-prod $APP prod 1.0.0)" | jq -r .id)
expect_eq "dave rilascia in produzione" "$(wait_state "$id" terminal 60)" SUCCESS
me=$(with_jwt "$DAVE" whoami)
expect_match "whoami riporta la revisione dei permessi" "$(printf '%s' "$me" | jq -r .authz_revision)" "^$rev:"
expect_eq "e i permessi della policy" "$(printf '%s' "$me" | jq -r '[.grants[].name] | sort | join(",")')" "owner team-uc25,team-uc25"
id=$(with_jwt "$CAROL" deploy --no-wait -f "$(descriptor uc25-c uc25-110-test $APP test 1.1.0)" | jq -r .id)
expect_eq "carol (contractor) rilascia in test" "$(wait_state "$id" terminal 60)" SUCCESS
err=$(with_jwt_err "$CAROL" deploy --no-wait -f "$(descriptor uc25-d uc25-110-prod $APP prod 1.1.0)")
expect_match "carol in produzione è negata, con il motivo e la revisione" "$err" "contractors cannot change production \\(policy $rev:"
expect_eq "la produzione resta sulla 1.0.0" "$(current eu1-a $APP)" 1.0.0
expect_match "l'audit registra la revisione e il permesso usato" \
  "$(dc logs --no-color api 2>/dev/null | grep 'uc25-100-prod' | grep 'authz_revision' | head -1)" "team-uc25"

info "-- reperibilità con until"
at=$(at_in 3600)
id=$(with_jwt "$DAVE" deploy --no-wait -f "$(descriptor uc25-e uc25-120-prod $APP prod 1.2.0 "schedule: {at: $at}")" | jq -r .id)
expect_eq "un deploy pianificato in produzione" "$(state "$id")" DELAYED
sim=$(as pol manent policy access current --subject token:cab --action deployment.cancel -a $APP -e prod --json)
expect_eq "il turno di cab è nella policy ma scaduto (until)" \
  "$(printf '%s' "$sim" | jq -r '"\(.grants[0].name) \(.grants[0].until) \(.check.allowed)"')" "oncall token:cab 2020-01-01T00:00:00Z false"
err=$(as cab manent_err cancel "$id")
expect_match "il turno di cab è scaduto: niente annullamento" "$err" "cannot perform deployment.cancel"
as viewer manent cancel "$id" >/dev/null 2>&1
expect_eq "il viewer di turno lo annulla" "$(wait_state "$id" CANCELLED 20)" CANCELLED

info "-- revisione che toglie tutto"
rm -rf "$WORK/uc/uc25-broken" && mkdir -p "$WORK/uc/uc25-broken"
printf 'package manent.authz\n\nimport rego.v1\n\ngrants := set()\n\ndeny := set()\n' >"$WORK/uc/uc25-broken/authz.rego"
broken=$(as pol manent policy push -d /uc/uc25-broken -m "uc25: errore, nessun permesso" | jq -r .revision)
as admin manent policy approve "$broken" >/dev/null 2>&1
sleep 3
err=$(manent_err deploy --no-wait -f "$(descriptor uc25-f uc25-120-test $APP test 1.2.0)")
expect_match "nessuno può più rilasciare" "$err" "token:ci cannot perform deployment.submit"
expect_eq "all'admin restano solo i permessi statici policy.*" \
  "$(as admin manent whoami | jq '[.grants[] | select(.name | endswith("(static)") | not)] | length')" 0
as admin manent policy activate "$rev" >/dev/null 2>&1
sleep 3
expect_match "chi ha policy.* in manent.yml torna alla revisione precedente" "$(as admin manent policy list | jq -r .current)" "^$rev:"
id=$(submit "$(descriptor uc25-f uc25-120-test $APP test 1.2.0)")
expect_eq "i permessi tornano: ci rilascia di nuovo" "$(wait_state "$id" terminal 60)" SUCCESS

info "-- ritorno alla policy predefinita"
rm -rf "$WORK/uc/uc25-default" && mkdir -p "$WORK/uc/uc25-default" && printf '{}\n' >"$WORK/uc/uc25-default/data.json"
back=$(as pol manent policy push -d /uc/uc25-default -m "ritorno ai binding di manent.yml" | jq -r .revision)
as admin manent policy approve "$back" >/dev/null 2>&1
sleep 3
expect_match "la nuova revisione è in vigore" "$(as admin manent policy list | jq -r .current)" "^$back:"
expect_eq "dave non ha più permessi della policy" "$(with_jwt "$DAVE" whoami | jq '.grants | length')" 0
