# UC23: governance in Rego evaluated by OPA. A policy revision is pushed, measured and approved
# by another identity without restarting anything; it limits the approvers and denies the
# downgrades. A freeze created at runtime pauses a wave rollout between two stages, a hard one
# blocks even the override; a break-glass closes it. At the end the default policy comes back.
APP=uc23-api
log "UC23 · governance in Rego (OPA): revisioni a runtime, quattro occhi, impatto, freeze d'emergenza"

for v in 1.0.0 1.1.0 1.2.0; do artifact $APP $v >/dev/null; done

info "-- policy predefinita"
pl=$(as admin manent policy list)
expect_eq "motore opa con le revisioni nello store di manent" "$(printf '%s' "$pl" | jq -r '.engine + "/" + .source')" "opa/store"
d=$(descriptor uc23-a uc23-100-prod $APP prod 1.0.0)
id=$(submit "$d")
expect_eq "senza regole per uc23 il deploy in prod non chiede approvazioni" "$(wait_state "$id" terminal 60)" SUCCESS

info "-- nuova revisione: caricamento, impatto, quattro occhi"
rm -rf "$WORK/uc/uc23-policy" && cp -R cases/uc23-policy "$WORK/uc/uc23-policy"
out=$(as pol manent policy push -d /uc/uc23-policy -m "uc23: approvatori del CAB, niente downgrade")
rev=$(printf '%s' "$out" | jq -r .revision)
expect_eq "revisione in bozza" "$(printf '%s' "$out" | jq -r .status)" draft
expect_eq "i tre test della revisione superati al caricamento" "$(printf '%s' "$out" | jq '[.tests[] | select(.pass)] | length')" 3
err=$(as pol manent_err policy approve "$rev")
expect_match "chi scrive la policy non la attiva" "$err" "policy.approve"
imp=$(as pol manent policy impact "$rev" --since 1h)
expect_match "l'impatto mostra il deploy di prod che avrebbe chiesto un'approvazione" \
  "$(printf '%s' "$imp" | jq -r '.changes[] | select(.application=="uc23-api") | .after')" "approvals=1"
own=$(as admin manent policy push -d /uc/uc23-policy -m "stessa revisione, caricata dall'admin" | jq -r .revision)
err=$(as admin manent_err policy approve "$own")
expect_match "nessuno approva la propria revisione (quattro occhi)" "$err" "cannot approve it"
as admin manent policy approve "$rev" >/dev/null 2>&1
sleep 3
expect_match "la revisione approvata è in vigore sull'API" "$(as admin manent policy list | jq -r .current)" "^$rev:"
expect_match "la revisione in vigore è esposta nelle metriche" \
  "$(curl -s http://127.0.0.1:18080/metrics | grep manent_policy_revision_info)" "revision=\"$rev:"

info "-- la revisione in azione"
d=$(descriptor uc23-b uc23-110-prod $APP prod 1.1.0)
id=$(submit "$d")
expect_eq "in prod il deploy attende l'approvazione" "$(wait_state "$id" AWAITING_APPROVAL 30)" AWAITING_APPROVAL
st=$(manent status "$id")
expect_eq "solo il CAB può approvare" "$(printf '%s' "$st" | jq -r '.approval.approvers.groups[0]')" cab
expect_match "lo stato riporta la revisione della decisione" "$(printf '%s' "$st" | jq -r .policy.revision)" "^$rev:"
err=$(as admin manent_err approve "$id")
expect_match "l'admin non è tra gli approvatori della policy" "$err" "not among the approvers"
as cab manent approve -m "ok dal CAB" "$id" >/dev/null
expect_eq "approvato dal CAB, il deploy riesce" "$(wait_state "$id" terminal 60)" SUCCESS
expect_eq "eu1-a sulla 1.1.0" "$(current eu1-a $APP)" 1.1.0
d=$(descriptor uc23-c uc23-100-down $APP prod 1.0.0)
err=$(manent_err deploy --no-wait -f "$d")
expect_match "un downgrade è negato dalla policy" "$err" "downgrade: 1.0.0 is older than 1.1.0"
expect_eq "validate mostra il divieto" "$(manent validate -f "$d" | jq -r '.governance.deny[0].code')" downgrade

info "-- freeze d'emergenza durante un rollout a ondate"
d=$(descriptor uc23-d uc23-110-waves $APP waves 1.1.0)
id=$(submit "$d")
t=0
while [ "$(current eu1-b $APP)" != 1.1.0 ] && [ "$t" -lt 60 ]; do sleep 2; t=$((t + 2)); done
expect_eq "la prima ondata (eu1-b) sulla 1.1.0" "$(current eu1-b $APP)" 1.1.0
as admin manent freeze add --name uc23-incidente --reason "incidente 23" -a $APP -e waves --for 1h >/dev/null
expect_eq "il rollout si ferma prima della seconda ondata" "$(wait_rollout "$id" paused-freeze 60)" paused-freeze
expect_match "la pausa cita il freeze creato a runtime" "$(manent status "$id" | jq -r .rollout.paused)" 'freeze "uc23-incidente"'
expect_eq "eu1-c non è stato toccato" "$(current eu1-c $APP)" none
as admin manent freeze delete uc23-incidente >/dev/null 2>&1
expect_eq "chiuso il freeze, il rollout riprende e finisce" "$(wait_state "$id" terminal 90)" SUCCESS
expect_eq "eu1-c sulla 1.1.0" "$(current eu1-c $APP)" 1.1.0

info "-- freeze hard e break-glass"
as admin manent freeze add --name uc23-hard --reason "chiusura contabile" -a $APP -e prod --for 1h --hard >/dev/null
expect_match "il freeze hard è elencato come creato a runtime" \
  "$(manent freezes -a $APP -e prod | jq -r '.[] | select(.name=="uc23-hard") | "\(.hard) \(.dynamic) \(.created_by)"')" "true true token:admin"
d=$(descriptor uc23-e uc23-120-prod $APP prod 1.2.0 'freezeOverride: "INC-23: correzione urgente"')
err=$(as admin manent_err deploy --no-wait -f "$d")
expect_match "il freeze hard blocca anche la deroga" "$err" 'hard freeze "uc23-hard".*cannot be overridden'
err=$(manent_err freeze delete uc23-hard)
expect_match "ci non può chiudere un freeze" "$err" "freeze.write"
dc exec -T api manent-server admin freeze-delete uc23-hard --reason "UC23: break-glass" >/dev/null 2>&1
expect_eq "chiuso dal break-glass sul database" "$(manent freezes -a $APP -e prod | jq '[.[] | select(.name=="uc23-hard")] | length')" 0

info "-- ritorno alla policy predefinita"
rm -rf "$WORK/uc/uc23-default" && mkdir -p "$WORK/uc/uc23-default" && printf '{}\n' >"$WORK/uc/uc23-default/data.json"
back=$(as pol manent policy push -d /uc/uc23-default -m "ritorno alle regole di manent.yml" | jq -r .revision)
as admin manent policy approve "$back" >/dev/null 2>&1
sleep 3
expect_match "la nuova revisione è in vigore" "$(as admin manent policy list | jq -r .current)" "^$back:"
d=$(descriptor uc23-f uc2-check $APP prod 1.2.0)
expect_eq "senza la policy di uc23 nessun approvatore richiesto" "$(manent validate -f "$d" | jq -r '.governance.approvals')" 0
d=$(descriptor uc23-g uc2-check uc2-api prod 1.0.0)
expect_eq "le regole di manent.yml valgono di nuovo (uc2)" "$(manent validate -f "$d" | jq -r '.governance.rule')" uc2-produzione
