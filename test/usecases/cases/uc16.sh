# UC16: canary analysis on four metrics systems, emulated by the fakes service: Datadog (query
# API with API and application keys), New Relic (NerdGraph NRQL with the user key), Elastic
# (ES|QL with an API key) and CloudWatch (GetMetricData with a SigV4 signature, through the AWS
# SDK). Every metric compares candidate and stable with Mann-Whitney; the score weighs the
# non-critical ones, the critical CloudWatch metric stops the rollout on its own.
log "UC16 · analisi canary su Datadog, New Relic, Elastic e CloudWatch"

APP=uc16-web
records() { curl -s "http://127.0.0.1:19100/records?kind=$1"; }
# series <provider> <key> <values...>: series the fake returns for the queries containing key.
series() {
  p=$1 k=$2; shift 2
  jq -cn --arg p "$p" --arg k "$k" --arg v "$*" '{provider: $p, key: $k, values: ($v | split(" ") | map(tonumber))}' |
    curl -s -o /dev/null -X POST -H 'Content-Type: application/json' --data-binary @- http://127.0.0.1:19100/control/metrics
}
# slot <a|b> <dd> <nr> <es> <cw>: the four series of a slot ("low" or "high").
slot() {
  s=$1; shift
  for pk in "datadog:track:$s}" "newrelic:track = '$s'" "elastic:track == \"$s\"" "cloudwatch:track = '$s'"; do
    case "$1" in
      low) v="1 2 1 2 1 2 1 2 1 2" ;;
      high) v="7 8 7 9 8 7 8 9 8 7" ;;
    esac
    series "${pk%%:*}" "${pk#*:}" $v
    shift
  done
}
uc16_weights() {
  for s in a b; do
    curl -s -u admin:uc-dataplane "http://127.0.0.1:15555/v3/services/haproxy/configuration/backends/$APP/servers/$s" | jq -r '.weight // 1'
  done | tr '\n' ' ' | sed 's/ $//'
}

dc --profile lb up -d haproxy >/dev/null 2>&1
t=0
until curl -fs -u admin:uc-dataplane http://127.0.0.1:15555/v3/services/haproxy/configuration/backends/$APP >/dev/null 2>&1; do
  t=$((t + 1)); [ "$t" -gt 60 ] && break; sleep 1
done
for v in 1.0.0 2.0.0 3.0.0 4.0.0; do artifact $APP $v >/dev/null; done
slot a low low low low
slot b low low low low

info "-- primo rilascio nello slot a"
id=$(submit "$(descriptor uc16-a uc16-web-100 $APP prod 1.0.0)")
expect_eq "rilascio riuscito senza confronti" "$(wait_state "$id" terminal 120)" SUCCESS
expect_eq "nessuna query alle sorgenti senza versione stabile" "$(records datadog | jq length)" 0

info "-- serie equivalenti: punteggio pieno sulle quattro sorgenti"
id=$(submit "$(descriptor uc16-b uc16-web-200 $APP prod 2.0.0)")
expect_eq "rilascio riuscito" "$(wait_state "$id" terminal 120)" SUCCESS
expect_eq "punteggio canary del gradino del 50%" "$(manent status "$id" | jq -r '.rollout.stages[0].score')" 100
expect_eq "tutto il traffico allo slot b" "$(uc16_weights)" "0 256"
expect_eq "Datadog: chiavi API e applicazione" "$(records datadog | jq -r 'map(.path | test("^/datadog/api/v1/query\\?")) | all')" true
expect_match "Datadog: query per slot, candidata e stabile" "$(records datadog | jq -r '[.[].path] | join(" ")')" "track%3Ab%7D.*track%3Aa%7D"
expect_eq "New Relic: chiave nell'header e NRQL TIMESERIES" \
  "$(records newrelic | jq -r '[.[0].headers["Content-Type"], (.[0].body.variables.nrql | test("TIMESERIES"))] | join(" ")')" "application/json true"
expect_eq "Elastic: ES|QL con l'API key" \
  "$(records elastic | jq -r '[.[0].headers.Authorization, (.[0].path | test("^/elastic/_query"))] | join(" ")')" "ApiKey uc-es-key true"
expect_eq "CloudWatch: GetMetricData firmata con SigV4" \
  "$(records cloudwatch | jq -r '[.[0].body.operation, (.[0].headers.Authorization | test("^AWS4-HMAC-SHA256 Credential=AKIDUCTEST/"))] | join(" ")')" "GetMetricData true"

info "-- due metriche non critiche peggiori: punteggio 50, marginale nel bake, rosso alla fine"
slot a high high low low
id=$(submit "$(descriptor uc16-c uc16-web-300 $APP prod 3.0.0)")
expect_eq "deploy in errore" "$(wait_state "$id" terminal 120)" ERROR
st=$(manent status "$id")
expect_eq "punteggio 50 sotto la soglia di superamento" "$(echo "$st" | jq -r '.rollout.stages[0].score')" 50
expect_match "il gate cita le due metriche peggiori" "$(echo "$st" | jq -r '.error')" "score 50/100; metric dd-errori.*metric nr-durata"
expect_eq "il traffico è tornato allo slot b" "$(uc16_weights)" "0 256"

info "-- metrica critica peggiore su CloudWatch: stop immediato"
slot a low low low high
id=$(submit "$(descriptor uc16-d uc16-web-400 $APP prod 4.0.0)")
expect_eq "deploy in errore" "$(wait_state "$id" terminal 120)" ERROR
st=$(manent status "$id")
expect_match "fermato dalla metrica critica prima della fine del bake" "$(echo "$st" | jq -r '.error')" "after stage .*metric cw-errori"
expect_eq "il traffico è tornato allo slot b" "$(uc16_weights)" "0 256"
expect_eq "la 2.0.0 resta in esercizio" "$(current eu1-b $APP)" 2.0.0

dc --profile lb rm -sf haproxy >/dev/null 2>&1
