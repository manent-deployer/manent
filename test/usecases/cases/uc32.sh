# UC32: deploy scripts on the worker (script plugin). The catalog is in manent.yml; the
# environment configuration chooses a script by name and passes its declared parameters. The
# plugin has its own queue, served only by worker-ansible: the distroless workers have no shell.
# The script (test/usecases/scripts/uc32.sh) releases a bundle on eu1-a and eu1-b over SSH.
# prod: one run for both targets, outputs, dry-run, temporary failure, failure with automatic
# rollback, manual rollback; test: one run per target, two together; faults: what the catalog
# refuses (unknown script, undeclared parameter, tampered file) and the timeout.
APP=uc32-bundle
log "UC32 · script sul worker (plugin script)"

dc --profile ansible up -d worker-ansible >/dev/null 2>&1 || dc --profile ansible up -d worker-ansible
t=0
until dc logs worker-ansible 2>/dev/null | grep -q 'worker started'; do
  t=$((t + 1)); [ "$t" -gt 60 ] && { dc logs worker-ansible | tail -20; break; }
  sleep 1
done

for v in 1.0.0 1.1.0 1.2.0; do artifact $APP $v >/dev/null; done
bundle() { art_descriptor "$1" "$2" $APP "$3" "$4" bundle "file:///srv/artifacts/$APP-$4.tar.gz" "${5:-}"; }
# on_target <host> <env> <file>: a file the script wrote under /home/deploy/uc32/<env>.
on_target() { dc exec -T "$1" cat "/home/deploy/uc32/$2/$3" 2>/dev/null | tr -d '\n'; }
versions() { echo "$(on_target eu1-a "$1" current) $(on_target eu1-b "$1" current)"; }
deploy_out() { manent status "$1" | jq -r --arg k "$2" '.result.steps[] | select(.name == "deploy") | .output[$k] // "none"'; }
for h in eu1-a eu1-b; do dc exec -T -u deploy $h rm -rf /home/deploy/uc32 /home/deploy/fault/uc32.fail /home/deploy/fault/uc32.flaky; done

info "-- deploy: lo script del catalogo scelto dall'ambiente"
id=$(submit "$(bundle uc32-a uc32-100 prod 1.0.0)")
expect_eq "deploy riuscito" "$(wait_state "$id" terminal 120)" SUCCESS
expect_eq "bundle sui due host" "$(versions prod)" "1.0.0 1.0.0"
expect_eq "eseguito da worker-ansible" "$(manent status "$id" | jq -r .executor)" worker-ansible
expect_eq "coda del plugin in manent queues" "$(manent queues --json | jq -r '.[] | select(.name == "scripts") | .kind')" plugin
expect_eq "output dichiarato dello script" "$(deploy_out "$id" out.endpoint)" "http://eu1-a:8080/uc32"
expect_eq "MANENT_PARALLELISM senza parallel" "$(deploy_out "$id" out.parallelism)" 1
expect_eq "segreto passato come file" "$(on_target eu1-a prod token)" uc32-api-t0ken
env_names=$(on_target eu1-a prod env)
expect_match "variabili MANENT_* e TMPDIR della directory dell'esecuzione" "$env_names" "MANENT_ACTION .*MANENT_ARTIFACT_PATH .*MANENT_PARAM_DEST .*TMPDIR"
expect_eq "nessuna variabile del worker (MANENT_CONFIG)" "$(printf '%s' "$env_names" | grep -c MANENT_CONFIG)" 0
logs=$(manent logs "$id")
expect_match "output dello script nel log del deploy" "$logs" "released 1.0.0 on eu1-b"
expect_match "segreto redatto nel log" "$logs" 'token in use: \*\*\*'
expect_eq "il valore del segreto non compare nel log" "$(printf '%s' "$logs" | grep -c uc32-api-t0ken)" 0

info "-- dry-run: il comando dry_run, nessuna modifica"
id=$(manent deploy --no-wait --dry-run -f "$(bundle uc32-b uc32-110 prod 1.1.0)" | jq -r .id)
expect_eq "dry-run riuscito" "$(wait_state "$id" terminal 120)" SUCCESS
expect_eq "target pronti" "$(manent status "$id" | jq -r '[.result.steps[] | select(.name == "deploy") | .output.result | fromjson | .targets[].status] | join(" ")')" "ready ready"
expect_match "il dry_run ha raggiunto i target" "$(manent logs "$id")" "dry-run: eu1-b reachable"
expect_eq "versioni invariate" "$(versions prod)" "1.0.0 1.0.0"

info "-- errore temporaneo (exit 75): lo step viene ripetuto"
dc exec -T -u deploy eu1-a touch /home/deploy/fault/uc32.flaky
id=$(submit "$(bundle uc32-c uc32-110 prod 1.1.0)")
expect_eq "deploy riuscito al secondo tentativo" "$(wait_state "$id" terminal 120)" SUCCESS
expect_match "il primo tentativo ha chiesto un retry" "$(manent logs "$id")" "asking for a retry"
expect_eq "bundle 1.1.0 sui due host" "$(versions prod)" "1.1.0 1.1.0"

info "-- errore permanente: rollback automatico con il comando rollback"
dc exec -T -u deploy eu1-b touch /home/deploy/fault/uc32.fail
id=$(submit "$(bundle uc32-d uc32-120 prod 1.2.0)")
expect_eq "deploy fallito" "$(wait_state "$id" terminal 120)" ERROR
expect_match "exit code nell'errore" "$(manent status "$id" | jq -r .error)" "exited with code 3"
expect_eq "rollback automatico completato" "$(manent status "$id" | jq -r .rollback)" completed
expect_match "eu1-a, già aggiornato, riportato alla release letta da current" "$(manent logs "$id")" "rolled back eu1-a to 1.1.0"
expect_eq "entrambi gli host sulla 1.1.0" "$(versions prod)" "1.1.0 1.1.0"
dc exec -T -u deploy eu1-b rm -f /home/deploy/fault/uc32.fail

info "-- rollback manuale: la release precedente di nuovo con run"
id=$(manent rollback --no-wait -a $APP -e prod | jq -r .id)
expect_eq "rollback riuscito" "$(wait_state "$id" terminal 120)" SUCCESS
expect_eq "entrambi gli host sulla 1.0.0" "$(versions prod)" "1.0.0 1.0.0"

info "-- per_target: un'esecuzione per host, due insieme"
id=$(submit "$(bundle uc32-e uc32-test-100 test 1.0.0)")
expect_eq "deploy riuscito" "$(wait_state "$id" terminal 120)" SUCCESS
expect_eq "output nei dettagli di ogni target" "$(manent status "$id" | jq -r '[.result.steps[] | select(.name == "deploy") | .output.result | fromjson | .targets[].details.endpoint] | join(" ")')" \
  "http://eu1-a:8080/uc32 http://eu1-b:8080/uc32"
expect_match "MANENT_TARGET nell'esecuzione di eu1-b" "$(on_target eu1-b test env)" "MANENT_TARGET "
ta=$(on_target eu1-a test runs | cut -d' ' -f2) tb=$(on_target eu1-b test runs | cut -d' ' -f2)
expect_eq "i due host aggiornati insieme (fine a meno di 3s)" "$([ $((ta > tb ? ta - tb : tb - ta)) -lt 3 ] && echo yes || echo no)" yes

info "-- il catalogo decide cosa può girare"
fault_deploy() { submit "$(bundle "$1" "$2" faults 1.0.0 "deploy:
  params: $3")"; }
id=$(fault_deploy uc32-f uc32-faults-1 '{script: /bin/sh}')
expect_eq "script fuori dal catalogo: deploy fallito" "$(wait_state "$id" terminal 120)" ERROR
expect_match "motivo" "$(manent status "$id" | jq -r .error)" "not in the catalog"
id=$(fault_deploy uc32-g uc32-faults-2 '{script: uc32-slow, command: "rm -rf /"}')
expect_eq "parametro non dichiarato: deploy fallito" "$(wait_state "$id" terminal 120)" ERROR
expect_match "motivo" "$(manent status "$id" | jq -r .error)" "command is not declared"
id=$(fault_deploy uc32-h uc32-faults-3 '{script: uc32-tampered}')
expect_eq "file diverso dallo sha256 del catalogo: deploy fallito" "$(wait_state "$id" terminal 120)" ERROR
expect_match "motivo" "$(manent status "$id" | jq -r .error)" "does not match the catalog"
id=$(fault_deploy uc32-i uc32-faults-4 '{script: uc32-slow}')
expect_eq "timeout: deploy fallito dopo il retry" "$(wait_state "$id" terminal 120)" ERROR
expect_match "motivo" "$(manent status "$id" | jq -r .error)" "terminated by timeout"
expect_eq "nessun processo dello script rimasto sul worker" \
  "$(dc exec -T worker-ansible sh -c 'for f in /proc/[0-9]*/cmdline; do tr "\0" " " <"$f"; echo; done 2>/dev/null | grep -c "^sleep 300"')" 0
expect_eq "directory delle esecuzioni rimosse" "$(dc exec -T worker-ansible sh -c 'ls /tmp/manent-script 2>/dev/null | wc -l' | tr -d ' ')" 0

dc --profile ansible stop -t 5 worker-ansible >/dev/null 2>&1
