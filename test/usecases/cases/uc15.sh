# UC15: canary analysis on a real Prometheus. HAProxy (uc15-web, servers named after the slots)
# exports its responses per server; while the host sends load, every read of the gate compares
# the series of candidate and stable with the Mann-Whitney test. The defective release lacks a
# page requested by one request in five: its 4xx share grows significantly and the critical
# metric stops the rollout, bringing the traffic back to the stable slot.
log "UC15 · analisi canary su Prometheus: Mann-Whitney tra candidata e stabile"

APP=uc15-web
# uc15_artifact <version> [broken]: like artifact, plus the page "extra" (missing if broken).
uc15_artifact() {
  d=$(mktemp -d)
  printf '%s\n' "$1" >"$d/version"
  printf 'ok %s\n' "$1" >"$d/healthz"
  [ "${2:-}" = broken ] || printf 'extra %s\n' "$1" >"$d/extra"
  COPYFILE_DISABLE=1 tar -czf "$WORK/artifacts/$APP-$1.tar.gz" -C "$d" .
  rm -rf "$d"
}
# prom <query>: instant query on the lab Prometheus, first value.
prom() { curl -s --data-urlencode "query=$1" http://127.0.0.1:19090/api/v1/query | jq -r '.data.result[0].value[1] // empty'; }
# uc15_weights: weights of the servers a and b of the uc15-web backend.
uc15_weights() {
  for s in a b; do
    curl -s -u admin:uc-dataplane "http://127.0.0.1:15555/v3/services/haproxy/configuration/backends/$APP/servers/$s" | jq -r '.weight // 1'
  done | tr '\n' ' ' | sed 's/ $//'
}

info "-- avvio di HAProxy e Prometheus"
dc --profile lb --profile metrics up -d haproxy prometheus >/dev/null 2>&1
t=0
until [ "$(prom 'count(haproxy_server_http_responses_total{proxy="uc15-web"})')" != "" ]; do
  t=$((t + 1)); [ "$t" -gt 90 ] && break; sleep 1
done
expect_match "Prometheus legge le risposte per server di HAProxy" \
  "$(prom 'count(haproxy_server_http_responses_total{proxy="uc15-web"})')" "^[1-9]"

uc15_artifact 1.0.0
uc15_artifact 2.0.0
uc15_artifact 3.0.0 broken

# Load from the host for the whole case: the current page, plus "extra" every fourth one. The
# cycle has an odd number of requests (5), so with roundrobin "extra" reaches both slots.
( i=0; while [ ! -f "$WORK/uc15-stop" ]; do
    curl -s -o /dev/null "http://127.0.0.1:18081/$APP/current/version"
    i=$((i + 1)); [ $((i % 4)) -eq 0 ] && curl -s -o /dev/null "http://127.0.0.1:18081/$APP/current/extra"
    sleep 0.05
  done ) &
loader=$!

info "-- primo rilascio: nessuna versione stabile da confrontare"
id=$(submit "$(descriptor uc15-a uc15-web-100 $APP prod 1.0.0)")
expect_eq "rilascio riuscito" "$(wait_state "$id" terminal 120)" SUCCESS
expect_eq "la 1.0.0 nello slot a con tutto il traffico" "$(current eu1-a $APP) $(uc15_weights)" "1.0.0 256 0"

info "-- release sana: punteggio pieno"
id=$(submit "$(descriptor uc15-b uc15-web-200 $APP prod 2.0.0)")
expect_eq "rilascio riuscito dopo il bake del 50%" "$(wait_state "$id" terminal 180)" SUCCESS
st=$(manent status "$id")
expect_eq "punteggio canary del gradino del 50%" "$(echo "$st" | jq -r '.rollout.stages[0].score')" 100
expect_eq "la 2.0.0 nello slot b con tutto il traffico" "$(current eu1-b $APP) $(uc15_weights)" "2.0.0 0 256"

info "-- release difettosa: più errori 4xx sulla candidata, il gate la respinge"
id=$(submit "$(descriptor uc15-c uc15-web-300 $APP prod 3.0.0)")
expect_eq "deploy in errore" "$(wait_state "$id" terminal 180)" ERROR
st=$(manent status "$id")
expect_match "il gate riporta la metrica critica con il p-value" "$(echo "$st" | jq -r '.error')" "metric errori[^;]*: candidate .* p=[0-9.e-]+ < 0\\.05"
expect_eq "il traffico è tornato tutto allo slot b" "$(uc15_weights)" "0 256"
expect_eq "ritorno alla versione stabile completato" "$(echo "$st" | jq -r '.rollout.rollback')" completed

touch "$WORK/uc15-stop"
wait "$loader" 2>/dev/null
info "-- spegnimento di HAProxy e Prometheus"
dc --profile lb --profile metrics rm -sf haproxy prometheus >/dev/null 2>&1
