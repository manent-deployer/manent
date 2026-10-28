# UC30: parallel deploy on the targets of a batch (deploy.parallel). maxUnavailable decides how
# many targets are out of service together, parallel how many the plugin updates at the same
# time; plugins.remote-copy.max_parallel (2 in the lab) caps it, descriptor_policy.max_parallel
# (4) limits what a descriptor can ask for.
# prod: parallel 100% of a single batch of three hosts; test: nothing in the environment, the
# descriptor chooses; prod-waves: canary one host at a time, then two hosts together.
APP=uc30-web
log "UC30 · deploy in parallelo sui target di un lotto"

for v in 1.0.0 1.1.0 1.2.0 1.3.0 1.4.0 1.5.0; do artifact $APP $v >/dev/null; done

# span <target> <version>: "start end" (epoch seconds) of the post_command run of the version.
span() {
  dc exec -T "$1" sh -c "awk '\$3 == \"$2\" && \$2 == \"start\" {s = \$1} \$3 == \"$2\" && \$2 == \"end\" {e = \$1} END {print s, e}' /home/deploy/fault/$APP.log 2>/dev/null"
}
# overlaps <a> <b> <version>: "yes" if the runs on the two targets overlapped in time.
overlaps() {
  set -- $(span "$1" "$3") $(span "$2" "$3")
  if [ "$1" -lt "$4" ] && [ "$3" -lt "$2" ]; then echo yes; else echo no; fi
}
started() { dc exec -T "$1" sh -c "grep -c ' start $2\$' /home/deploy/fault/$APP.log 2>/dev/null || true"; }
# deploy_output <id> <step> [key]: output key (default parallel) of the step, "none" if absent.
deploy_output() {
  manent status "$1" | jq -r --arg s "$2" --arg k "${3:-parallel}" '[.result.steps[] | select(.name == $s) | .output[$k]] | first // "none"'
}
for h in eu1-a eu1-b eu1-c; do fault $h $APP sleep 4; done

info "-- un lotto di tre host, parallel 100% limitato da max_parallel"
d=$(descriptor uc30-a uc30-100 $APP prod 1.0.0)
expect_eq "validate: il deploy effettivo chiede il 100% del lotto" "$(manent validate -f "$d" | jq -r .deploy.parallel)" "100%"
id=$(submit "$d")
expect_eq "deploy riuscito" "$(wait_state "$id" terminal 90)" SUCCESS
expect_eq "tutti gli host sulla 1.0.0" "$(currents $APP eu1-a eu1-b eu1-c)" "1.0.0 1.0.0 1.0.0"
expect_eq "output dello step: 2 host alla volta" "$(deploy_output "$id" deploy)" 2
expect_eq "output dello step: 3 richiesti (max_parallel: 2)" "$(deploy_output "$id" deploy parallel_requested)" 3
expect_eq "eu1-a ed eu1-b aggiornati insieme" "$(overlaps eu1-a eu1-b 1.0.0)" yes
expect_eq "eu1-c parte quando si libera un posto" "$(overlaps eu1-a eu1-c 1.0.0)" no

info "-- senza parallel: un host dopo l'altro"
id=$(submit "$(descriptor uc30-b uc30-test-100 $APP test 1.1.0)")
expect_eq "deploy riuscito" "$(wait_state "$id" terminal 90)" SUCCESS
expect_eq "nessun parallel nell'output dello step" "$(deploy_output "$id" deploy)" none
expect_eq "eu1-a ed eu1-b in sequenza" "$(overlaps eu1-a eu1-b 1.1.0)" no
expect_eq "eu1-b ed eu1-c in sequenza" "$(overlaps eu1-b eu1-c 1.1.0)" no

info "-- parallel dal descrittore, entro descriptor_policy.max_parallel"
id=$(submit "$(descriptor uc30-c uc30-test-120 $APP test 1.2.0 'deploy: {parallel: "3"}')")
expect_eq "deploy riuscito" "$(wait_state "$id" terminal 90)" SUCCESS
expect_eq "eu1-a ed eu1-b aggiornati insieme" "$(overlaps eu1-a eu1-b 1.2.0)" yes
expect_match "oltre il limite della policy: rifiutato" \
  "$(manent_err deploy --no-wait -f "$(descriptor uc30-d uc30-test-130 $APP test 1.3.0 'deploy: {parallel: "10"}')")" "descriptor_policy.max_parallel"
expect_match "prod definisce parallel: il descrittore non può cambiarlo" \
  "$(manent_err deploy --no-wait -f "$(descriptor uc30-e uc30-130 $APP prod 1.3.0 'deploy: {parallel: "1"}')")" "deploy.parallel"

info "-- guasto su un host: i target avviati finiscono, gli altri non partono"
# eu1-a fails at once while eu1-b is still running: no slot frees before the failure.
fault eu1-a $APP clear
fault eu1-a $APP fail
id=$(submit "$(descriptor uc30-f uc30-140 $APP prod 1.4.0)")
expect_eq "deploy ERROR" "$(wait_state "$id" terminal 90)" ERROR
expect_eq "eu1-b, avviato insieme a eu1-a, ha finito" "$(current eu1-b $APP)" 1.4.0
expect_eq "eu1-c non è mai partito" "$(started eu1-c 1.4.0)" 0
fault eu1-a $APP clear
fault eu1-a $APP sleep 4

info "-- rollout a ondate: canary da solo, poi due host insieme"
id=$(submit "$(descriptor uc30-g uc30-waves-150 $APP prod-waves 1.5.0)")
expect_eq "deploy riuscito" "$(wait_state "$id" terminal 120)" SUCCESS
expect_eq "il canary usa il parallel della sua ondata" "$(deploy_output "$id" 'deploy@canary#0')" 1
expect_eq "il lotto resto usa parallel: 2" "$(deploy_output "$id" 'deploy@resto#0')" 2
expect_eq "eu1-b ed eu1-c aggiornati insieme" "$(overlaps eu1-b eu1-c 1.5.0)" yes
expect_eq "il canary prima del resto" "$(overlaps eu1-a eu1-b 1.5.0)" no

for h in eu1-a eu1-b eu1-c; do fault $h $APP clear; done
