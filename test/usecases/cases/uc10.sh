# UC10: load balancer and traffic router on a real HAProxy with the Data Plane API.
# uc10-shop: wave rollout one AZ at a time; every batch leaves the HAProxy backend (drain) before
# the deploy and comes back once healthy, with the capacity check (min_healthy). uc10-blue:
# percentage rollout between two slots of targets with the HAProxy router (server weights),
# with the governance approvals moved into the rollout (approvals.when: rollout).
log "UC10 · HAProxy: svuotamento a lotti e router per percentuale"

# dp <path> [curl options]: Data Plane API v3 of the lab HAProxy, from the host.
dp() { p=$1; shift; curl -s -u admin:uc-dataplane "$@" "http://127.0.0.1:15555/v3/services/haproxy$p"; }
# srv <backend> <server>: runtime state "admin_state/operational_state".
srv() { dp "/runtime/backends/$1/servers/$2" | jq -r '"\(.admin_state)/\(.operational_state)"'; }
# weights: weights of the eu1-a and eu1-b servers of the uc10-blue backend.
weights() { echo "$(dp /configuration/backends/uc10-blue/servers/eu1-a | jq -r '.weight // 1') $(dp /configuration/backends/uc10-blue/servers/eu1-b | jq -r '.weight // 1')"; }
# lb <path>: answer of HAProxy's frontend.
lb() { curl -s "http://127.0.0.1:18081$1" | tr -d '\r\n'; }
# lb_versions <n>: distinct versions served by uc10-blue over n requests.
lb_versions() { i=0; while [ "$i" -lt "$1" ]; do lb /uc10-blue/current/version; echo; i=$((i + 1)); done | sort -u | tr '\n' ' ' | sed 's/ $//'; }
up() { for h in "$@"; do dc exec -T -u deploy "$h" touch /home/deploy/apps/up; done; }

info "-- avvio di HAProxy"
dc --profile lb up -d haproxy >/dev/null 2>&1
up eu1-a eu1-b eu1-c
t=0
until [ "$(srv uc10-shop eu1-a) $(srv uc10-shop eu1-b) $(srv uc10-shop eu1-c)" = "ready/up ready/up ready/up" ]; do
  t=$((t + 1)); [ "$t" -gt 60 ] && break; sleep 1
done
expect_eq "Data Plane API pronta, i tre server di uc10-shop sani" \
  "$(srv uc10-shop eu1-a) $(srv uc10-shop eu1-b) $(srv uc10-shop eu1-c)" "ready/up ready/up ready/up"

APP=uc10-shop
artifact $APP 1.0.0 >/dev/null
artifact $APP 1.1.0 >/dev/null

info "-- svuotamento di ogni lotto intorno al deploy"
fault eu1-b $APP sleep 8
id=$(submit "$(descriptor uc10-a uc10-shop-100 $APP prod 1.0.0)")
seen="" t=0
while [ "$t" -lt 90 ]; do
  s=$(srv $APP eu1-b)
  case "$s" in drain/*|maint/*) seen=$s; break ;; esac
  t=$((t + 1)); sleep 1
done
expect_match "durante il deploy su eu1-b il server è fuori da HAProxy" "$seen" "^(drain|maint)/"
expect_eq "intanto gli altri due server restano in servizio" "$(srv $APP eu1-a | cut -d/ -f1) $(srv $APP eu1-c | cut -d/ -f1)" "ready ready"
expect_eq "deploy riuscito" "$(wait_state "$id" terminal 180)" SUCCESS
fault eu1-b $APP clear
expect_eq "alla fine tutti i server sono di nuovo in servizio e sani" \
  "$(srv $APP eu1-a) $(srv $APP eu1-b) $(srv $APP eu1-c)" "ready/up ready/up ready/up"
expect_eq "tutte le AZ sulla 1.0.0" "$(currents $APP eu1-a eu1-b eu1-c)" "1.0.0 1.0.0 1.0.0"
expect_match "per ogni lotto svuotamento, deploy e rientro prima dell'attivazione" "$(step_names "$id")" \
  "drain@eu/eu-1/eu-1b#0 checkpoint:deploy@eu/eu-1/eu-1b#0 deploy@eu/eu-1/eu-1b#0 restore@eu/eu-1/eu-1b#0 activation@eu/eu-1/eu-1b#0"
expect_match "nella timeline le attese durevoli di svuotamento e rientro" "$(manent steps "$id" | jq -r '[.[].name] | join(" ")')" \
  "drain-wait@eu/eu-1/eu-1b#0\\.[0-9]+ .*restore-wait@eu/eu-1/eu-1b#0\\.[0-9]+"

info "-- capacità insufficiente: lo svuotamento aspetta"
dc exec -T -u deploy eu1-c rm -f /home/deploy/apps/up
t=0; until [ "$(srv $APP eu1-c)" = "ready/down" ]; do t=$((t + 1)); [ "$t" -gt 20 ] && break; sleep 1; done
expect_eq "eu1-c risulta guasto per HAProxy" "$(srv $APP eu1-c)" "ready/down"
codes=$WORK/uc10-codes
: >"$codes"
( i=0; while [ "$i" -lt 120 ]; do curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:18081/uc10-shop/current/version >>"$codes"; i=$((i + 1)); sleep 0.25; done ) &
loader=$!
id=$(submit "$(descriptor uc10-b uc10-shop-110 $APP prod 1.1.0)")
sleep 10
expect_eq "con un solo server sano rimasto, eu1-a non viene svuotato" "$(state "$id") $(srv $APP eu1-a | cut -d/ -f1) $(current eu1-a $APP)" "PENDING ready 1.0.0"
up eu1-c
expect_eq "eu1-c di nuovo sano: il deploy prosegue e riesce" "$(wait_state "$id" terminal 180)" SUCCESS
expect_match "il primo svuotamento è stato ripetuto (drain@...#0.1)" "$(step_names "$id")" "drain@eu/eu-1/eu-1a#0\\.1"
expect_eq "tutte le AZ sulla 1.1.0" "$(currents $APP eu1-a eu1-b eu1-c)" "1.1.0 1.1.0 1.1.0"
wait "$loader" 2>/dev/null
expect_eq "nessuna richiesta fallita attraverso HAProxy durante il rilascio" \
  "$(grep -vc '^200$' "$codes" | tr -d ' ') su $(wc -l <"$codes" | tr -d ' ')" "0 su 120"

APP=uc10-blue
artifact $APP 1.0.0 >/dev/null
artifact $APP 2.0.0 >/dev/null
artifact $APP 3.0.0 broken >/dev/null

info "-- router: primo rilascio nello slot a, approvazione prima del traffico"
expect_eq "nessuno slot riceve traffico" "$(weights)" "0 0"
id=$(submit "$(descriptor uc10-c uc10-blue-100 $APP prod 1.0.0)")
expect_eq "con approvals.when: rollout nessuna approvazione prima del deploy, una prima del traffico" \
  "$(wait_rollout "$id" paused-approval 90)" paused-approval
expect_eq "la candidata è nello slot a, ancora senza traffico" "$(currents $APP eu1-a eu1-b) $(weights)" "1.0.0 none 0 0"
as cab manent approve "$id" >/dev/null
expect_eq "approvato: rilascio riuscito" "$(wait_state "$id" terminal 120)" SUCCESS
expect_eq "tutto il traffico allo slot a" "$(weights)" "256 0"
expect_eq "HAProxy serve la 1.0.0" "$(lb_versions 6)" "1.0.0"

info "-- secondo rilascio nello slot b: 50%, approvazione, 100%"
id=$(submit "$(descriptor uc10-d uc10-blue-200 $APP prod 2.0.0)")
expect_eq "pausa per l'approvazione dopo il gradino del 50%" "$(wait_rollout "$id" paused-approval 120)" paused-approval
expect_eq "pesi uguali sui due slot" "$(weights)" "256 256"
expect_eq "HAProxy serve entrambe le versioni" "$(lb_versions 10)" "1.0.0 2.0.0"
expect_eq "lo stato riporta stabile a, candidata b, quota 50" \
  "$(manent status "$id" | jq -r '.rollout.traffic | "\(.stable) \(.candidate) \(.weight)"')" "a b 50"
as cab manent approve "$id" >/dev/null
expect_eq "rilascio riuscito" "$(wait_state "$id" terminal 120)" SUCCESS
expect_eq "tutto il traffico allo slot b" "$(weights)" "0 256"
expect_eq "HAProxy serve solo la 2.0.0" "$(lb_versions 6)" "2.0.0"

info "-- release difettosa nello slot a: gate rosso e ritorno allo slot b"
id=$(submit "$(descriptor uc10-e uc10-blue-300-broken $APP prod 3.0.0)")
expect_eq "deploy in errore" "$(wait_state "$id" terminal 120)" ERROR
expect_eq "il traffico è tornato tutto allo slot b" "$(weights)" "0 256"
expect_eq "HAProxy serve ancora la 2.0.0" "$(lb_versions 6)" "2.0.0"
expect_eq "ritorno alla versione stabile completato" "$(manent status "$id" | jq -r '.rollout.rollback')" completed

info "-- spegnimento di HAProxy"
dc --profile lb rm -sf haproxy >/dev/null 2>&1
