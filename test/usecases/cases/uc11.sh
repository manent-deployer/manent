# UC11: percentage rollout of a Helm chart on Kubernetes (k3s) with the kubernetes plugin and
# the Gateway API: the candidate goes next to the stable version in the other slot
# (uc11-web-a / uc11-web-b), the testers' rule of the HTTPRoute leads to it, the weights move
# by steps; the replaced slot is scaled to zero after scaleDownDelay. A faulty release brings
# the traffic back to the stable slot. The HTTPRoute is created upfront, as infrastructure.
APP=uc11-web
log "UC11 · rilascio per percentuale su Kubernetes con la Gateway API e un chart Helm"

route() { kube -n uc11-prod get httproute $APP -o json; }
# weights: "<backend>=<weight> ..." of the main rule (the one without the testers' header).
weights() { route | jq -r '[.spec.rules[] | select([.matches[]?.headers] | map(select(. != null)) | length == 0) | .backendRefs[] | "\(.name)=\(.weight // 1)"] | join(" ")'; }
# testers: backend of the rule matching the testers' header.
testers() { route | jq -r '[.spec.rules[] | select([.matches[]?.headers] | map(select(. != null)) | length > 0) | .backendRefs[].name] | join(" ")'; }
replicas() { kube -n uc11-prod get deploy "$1" -o jsonpath='{.spec.replicas}/{.status.readyReplicas}' 2>/dev/null; }
pointer() { kube -n uc11-prod get cm manent-$APP-prod -o jsonpath='{.data.version} {.data.slot}' 2>/dev/null; }

info "-- cluster, Gateway API e HTTPRoute dell'infrastruttura"
k3s_up && pass "cluster pronto" || fail "cluster k3s non pronto"
dcp exec -T k3s kubectl apply --server-side -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.2.1/standard-install.yaml >/dev/null 2>&1
kube wait --for condition=Established crd/httproutes.gateway.networking.k8s.io --timeout=60s >/dev/null 2>&1 &&
  pass "CRD della Gateway API installate" || fail "CRD della Gateway API non installate"
kube apply -f - >/dev/null <<'YAML'
apiVersion: v1
kind: Namespace
metadata: {name: uc11-prod}
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: uc11-web, namespace: uc11-prod}
spec:
  rules:
    - matches: [{headers: [{name: x-canary, value: always}]}]
      backendRefs: [{name: uc11-web-a, port: 80}]
    - backendRefs:
        - {name: uc11-web-a, port: 80, weight: 100}
        - {name: uc11-web-b, port: 80, weight: 0}
YAML
expect_eq "HTTPRoute creata: tutto il traffico allo slot a" "$(weights)" "uc11-web-a=100 uc11-web-b=0"

w100=$(image $APP 1.0.0)
w200=$(image $APP 2.0.0)
w300=$(image $APP 3.0.0 broken)
chart $APP 1.0.0 "$w100" >/dev/null
chart $APP 2.0.0 "$w200" >/dev/null
chart $APP 3.0.0 "$w300" >/dev/null
desc() { art_descriptor "$1" "$2" $APP prod "$3" kubernetes "file:///srv/artifacts/$APP-$3.tgz"; }

info "-- primo rilascio: nessuna versione stabile, la candidata va nello slot a"
id=$(submit "$(desc uc11-a uc11-100 1.0.0)")
expect_eq "pausa per l'approvazione dopo i test a peso 0" "$(wait_rollout "$id" paused-approval 180)" paused-approval
expect_eq "chart renderizzato nello slot a: Deployment uc11-web-a pronto" "$(replicas uc11-web-a)" "2/2"
expect_eq "etichetta dello slot sul Deployment" "$(kube -n uc11-prod get deploy uc11-web-a -o jsonpath='{.metadata.labels.manent\.io/track}')" a
as cab manent approve "$id" >/dev/null
expect_eq "primo rilascio riuscito" "$(wait_state "$id" terminal 120)" SUCCESS
expect_eq "tutto il traffico allo slot a" "$(weights)" "uc11-web-a=100 uc11-web-b=0"
expect_eq "lo storico indica la 1.0.0 nello slot a" "$(pointer)" "1.0.0 a"

info "-- secondo rilascio: candidata nello slot b, tester, gradini"
id=$(submit "$(desc uc11-b uc11-200 2.0.0)")
expect_eq "pausa dopo i test a peso 0" "$(wait_rollout "$id" paused-approval 180)" paused-approval
expect_eq "candidata uc11-web-b pronta con l'immagine 2.0.0" "$(replicas uc11-web-b) $(kube -n uc11-prod get deploy uc11-web-b -o jsonpath='{.spec.template.spec.containers[0].image}' | sed 's/.*@sha256://')" "2/2 $w200"
expect_eq "ancora nessun traffico alla candidata" "$(weights)" "uc11-web-a=100 uc11-web-b=0"
expect_eq "la regola dei tester porta alla candidata" "$(testers)" "uc11-web-b"
expect_eq "stato: stabile a, candidata b, tester instradati" \
  "$(manent status "$id" | jq -r '.rollout.traffic | "\(.stable) \(.candidate) \(.testers)"')" "a b true"
as cab manent approve "$id" >/dev/null
expect_eq "rilascio riuscito" "$(wait_state "$id" terminal 180)" SUCCESS
expect_eq "tutto il traffico allo slot b" "$(weights)" "uc11-web-a=0 uc11-web-b=100"
expect_eq "lo storico indica la 2.0.0 nello slot b" "$(pointer)" "2.0.0 b"
expect_match "gradini del 20% e del 100% nel risultato" "$(step_names "$id")" "traffic@20 .*traffic@100 .*traffic:promote"
t=0; until [ "$(replicas uc11-web-a | cut -d/ -f1)" = 0 ]; do t=$((t + 1)); [ "$t" -gt 60 ] && break; sleep 1; done
expect_eq "dopo scaleDownDelay lo slot sostituito è a zero repliche" "$(replicas uc11-web-a | cut -d/ -f1)" 0

info "-- release difettosa: la candidata non parte, il traffico resta sullo slot b"
id=$(submit "$(desc uc11-c uc11-300-broken 3.0.0)")
expect_eq "deploy in errore" "$(wait_state "$id" terminal 240)" ERROR
expect_eq "il traffico resta tutto allo slot b" "$(weights)" "uc11-web-a=0 uc11-web-b=100"
expect_eq "la candidata è riportata a zero repliche" "$(replicas uc11-web-a | cut -d/ -f1)" 0
expect_eq "lo storico indica ancora la 2.0.0 nello slot b" "$(pointer)" "2.0.0 b"
expect_eq "ritorno alla versione stabile completato" "$(manent status "$id" | jq -r '.rollout.rollback')" completed

info "-- spegnimento del cluster"
dcp rm -sf k3s registry >/dev/null 2>&1
