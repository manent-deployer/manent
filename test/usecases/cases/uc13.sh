# UC13: integrations, against the lab fakes (Slack Web API, Teams webhook, ServiceNow Table API).
# test: the deploy message on Slack is posted at start and updated with the outcome. prod: one
# approval and the ServiceNow change are required; the approval request reaches Slack (with
# buttons) and Teams, the change is opened with the deploy as correlation_id; the click of the
# requester is refused (self-approval), that of the CAB is accepted (signed Slack request); the
# deploy starts only when ServiceNow approves the change, which is then closed with the outcome.
# A change rejected in ServiceNow rejects the deploy.
APP=uc13-api
log "UC13 · integrazioni: Slack, Teams e ServiceNow"

records() { curl -s "http://127.0.0.1:19100/records?kind=$1"; }
snow() { curl -s -u manent:uc-snow "http://127.0.0.1:19100/api/now/table/change_request/$1" | jq -r "$2"; }
# slack_click <user> <action> <deploy>: click on an approval button, signed like Slack does.
slack_click() {
  payload=$(jq -cn --arg u "$1" --arg a "$2" --arg v "$3" \
    '{type: "block_actions", user: {id: $u}, response_url: "https://hooks.slack.com/actions/T0UC/1/x", actions: [{action_id: $a, value: $v}]}')
  body="payload=$(printf '%s' "$payload" | jq -sRr @uri)"
  ts=$(date +%s)
  sig=$(printf 'v0:%s:%s' "$ts" "$body" | openssl dgst -sha256 -hmac uc-slack-signing | awk '{print $NF}')
  curl -s -o /dev/null -w '%{http_code}' -X POST http://127.0.0.1:18080/integrations/slack/interactions \
    -H 'Content-Type: application/x-www-form-urlencoded' -H "X-Slack-Request-Timestamp: $ts" -H "X-Slack-Signature: v0=$sig" --data "$body"
}
decisions() { manent status "$1" | jq -r '[.approval.decisions[]? | "\(.approver):\(.approve)"] | join(" ")'; }

artifact $APP 1.0.0 >/dev/null
artifact $APP 1.1.0 >/dev/null

info "-- test: il messaggio Slack del deploy è aggiornato con l'esito"
id=$(submit "$(descriptor uc13-a uc13-test-100 $APP test 1.0.0)")
expect_eq "deploy in test riuscito" "$(wait_state "$id" terminal 120)" SUCCESS
sleep 2
msgs=$(records slack)
post=$(printf '%s' "$msgs" | jq -c '[.[] | select(.path | endswith("chat.postMessage"))][0]')
upd=$(printf '%s' "$msgs" | jq -c '[.[] | select(.path | endswith("chat.update"))][-1]')
expect_eq "un messaggio all'avvio sul canale C0UC13, con il bot token" \
  "$(printf '%s' "$post" | jq -r '.body.channel + " " + .headers.Authorization')" "C0UC13 Bearer xoxb-uc-test"
expect_eq "l'esito aggiorna lo stesso messaggio (stesso ts)" "$(printf '%s' "$upd" | jq -r .body.ts)" "$(printf '%s' "$post" | jq -r '"\(.seq).000100"')"
expect_match "il messaggio aggiornato riporta il deploy e il link all'interfaccia web" "$(printf '%s' "$upd" | jq -c .body)" "$id.*127.0.0.1:18080"
expect_eq "nessun change e nessun messaggio Teams fuori da prod" "$(records servicenow | jq length) $(records teams | jq length)" "0 0"

info "-- prod: approvazione e change in ServiceNow"
id=$(submit "$(descriptor uc13-b uc13-prod-100 $APP prod 1.0.0)")
expect_eq "il deploy attende l'approvazione" "$(wait_state "$id" AWAITING_APPROVAL 60)" AWAITING_APPROVAL
t=0; until [ "$(manent status "$id" | jq -r '.approval.change.number // empty')" != "" ]; do t=$((t + 1)); [ "$t" -gt 30 ] && break; sleep 1; done
num=$(manent status "$id" | jq -r '.approval.change.number // empty')
expect_eq "change aperto in ServiceNow e visibile nello stato" "$num" CHG0000001
expect_eq "il change porta l'ID del deploy come correlation_id" "$(snow sys1 .result.correlation_id)" "$id"
sleep 2
expect_match "la richiesta di approvazione su Slack ha i pulsanti con l'ID del deploy" \
  "$(records slack | jq -c '[.[] | select(.body.blocks? and (.body.blocks | tostring | contains("manent_approve")))][-1].body')" "manent_approve.*$id"
# The lab webhook is plain http: manent sends Teams webhooks only over https (the URL comes
# from a secret), refuses it without retries and the approval goes on.
expect_eq "il webhook di Teams in http non riceve nulla" "$(records teams | jq length)" 0
expect_match "la notifica a Teams è rifiutata senza tentativi ripetuti" \
  "$(dc logs worker-1 worker-2 2>/dev/null | grep "notify:uc13-teams:approval_requested" | grep -c 'retrying' | tr -d ' ') $(dc logs worker-1 worker-2 2>/dev/null | grep -c 'notifica in chat non inviata.*route=uc13-teams.*solo https' | tr -d ' ')" "^0 [1-9]"

info "-- approvazioni dai pulsanti di Slack"
expect_eq "un clic con firma valida è accettato da manent" "$(slack_click U0CI manent_approve "$id")" 200
sleep 3
expect_eq "il clic di chi ha chiesto il deploy (token:ci) non conta: auto-approvazione" "$(decisions "$id")" ""
slack_click U0CAB manent_approve "$id" >/dev/null
t=0; until [ -n "$(decisions "$id")" ]; do t=$((t + 1)); [ "$t" -gt 20 ] && break; sleep 1; done
expect_eq "il clic del CAB (oidc:carla) approva" "$(decisions "$id")" "oidc:carla:true"
sleep 3
expect_eq "il deploy attende ancora l'approvazione del change" "$(state "$id")" AWAITING_APPROVAL
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST http://127.0.0.1:18080/integrations/slack/interactions \
  -H "X-Slack-Request-Timestamp: $(date +%s)" -H 'X-Slack-Signature: v0=00' --data 'payload={}')
expect_eq "una richiesta con firma non valida è rifiutata" "$code" 401

info "-- il CAB approva il change in ServiceNow"
curl -s -X POST "http://127.0.0.1:19100/control/servicenow/$num?approval=approved" >/dev/null
expect_eq "il deploy parte e riesce" "$(wait_state "$id" terminal 120)" SUCCESS
expect_eq "change chiuso con esito positivo" "$(snow sys1 '.result.state + " " + .result.close_code')" "3 successful"

info "-- change respinto in ServiceNow"
id=$(submit "$(descriptor uc13-c uc13-prod-110 $APP prod 1.1.0)")
wait_state "$id" AWAITING_APPROVAL 60 >/dev/null
t=0; until [ "$(manent status "$id" | jq -r '.approval.change.number // empty')" != "" ]; do t=$((t + 1)); [ "$t" -gt 30 ] && break; sleep 1; done
num=$(manent status "$id" | jq -r '.approval.change.number')
curl -s -X POST "http://127.0.0.1:19100/control/servicenow/$num?approval=rejected" >/dev/null
expect_eq "il deploy è respinto" "$(wait_state "$id" terminal 60)" REJECTED
expect_eq "il rifiuto del change compare tra le decisioni" "$(decisions "$id")" "servicenow:$num:false"
expect_eq "prod resta sulla 1.0.0" "$(current eu1-c uc13-api-prod)" 1.0.0
