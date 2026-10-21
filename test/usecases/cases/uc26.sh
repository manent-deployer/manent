# UC26: exports and imports between deploys. The infrastructure deploy (uc26-infra) "creates"
# the VMs with the custom step uc26-provision on eu1-a and exports their hosts (format targets)
# and the release directory; the configuration deploy (uc26-os) imports them as targets and
# dest_dir: from a given deploy (after), with --import, in a composite release, and from the
# release in service through the environment configuration (stage). An export that does not
# match its pattern fails the producer and its dependants.
log "UC26 · export e import tra deploy"

# provision script on the infrastructure host: version "*-evil" returns a host outside the
# lab, which the export pattern must refuse, after a pause (the dependant is enqueued while the
# producer is still running).
dc exec -T -u deploy eu1-a sh -c 'mkdir -p ~/bin && cat > ~/bin/provision && chmod 755 ~/bin/provision && rm -f ~/uc26-provision.log' <<'SH'
#!/bin/sh
echo "provision $1" >>"$HOME/uc26-provision.log"
case "$1" in
  *evil*) sleep 8; echo 'vms=[{"host":"evil.example","user":"deploy"}]' >>"$MANENT_OUTPUT" ;;
  *) echo 'vms=[{"host":"eu1-b","user":"deploy"},{"host":"eu1-c","user":"deploy"}]' >>"$MANENT_OUTPUT" ;;
esac
echo "dest=/home/deploy/apps/uc26-os" >>"$MANENT_OUTPUT"
SH

for v in 1.0.0 1.1.0-evil 1.2.0; do artifact uc26-infra $v >/dev/null; done
for v in 1.0.0 1.0.1 1.1.0 1.1.1 1.2.0; do artifact uc26-os $v >/dev/null; done

infra() {
  descriptor "$1" "$2" uc26-infra prod "$3" "steps: {uc26-provision: true}
exports:
  vms: {from: uc26-provision.vms, format: targets, pattern: 'eu1-[a-c]'}
  dest: {from: uc26-provision.dest, pattern: '/home/deploy/apps/[a-z0-9-]+'}"
}
refs="deploy:
  targetsFrom: [{import: infra, export: vms}]
  paramsFrom: {dest_dir: {import: infra, export: dest}}"
consumer() {
  descriptor "$1" "$2" uc26-os prod "$3" "after: [$4]
imports:
  infra: {deployment: $4}
$refs"
}
meta() { manent status "$1" | jq -r --arg k "$2" '.result.steps[] | select(.name == "config") | .output[$k] // empty'; }

info "-- dipendenza con after: target e dest_dir dagli export del produttore"
infra_id=$(submit "$(infra uc26-infra-a uc26-infra-100 1.0.0)")
os_id=$(submit "$(consumer uc26-os-a uc26-os-100 1.0.0 "$infra_id")")
expect_eq "il produttore riesce" "$(wait_state "$infra_id" terminal 120)" SUCCESS
expect_eq "provisioning eseguito su eu1-a" "$(dc exec -T eu1-a cat /home/deploy/uc26-provision.log | tail -1)" "provision 1.0.0"
expect_eq "export del produttore" "$(manent status "$infra_id" | jq -c '[.exports.dest, (.exports.vms | fromjson | map(.host))]')" \
  '["/home/deploy/apps/uc26-os",["eu1-b","eu1-c"]]'
expect_eq "il consumatore riesce" "$(wait_state "$os_id" terminal 120)" SUCCESS
expect_eq "rilasciato sulle VM esportate, nella directory esportata" "$(currents uc26-os eu1-a eu1-b eu1-c)" "none 1.0.0 1.0.0"
expect_eq "lo step config registra la provenienza" "$(meta "$os_id" import.infra) $(meta "$os_id" targets)" "uc26-infra-100@1.0.0 2"

info "-- --import dal CLI: il descrittore usa l'import senza dichiararlo"
d=$(descriptor uc26-os-b uc26-os-101 uc26-os prod 1.0.1 "$refs")
id=$(submit "$d" --import "infra=$infra_id")
expect_eq "deploy con --import riuscito" "$(wait_state "$id" terminal 120)" SUCCESS
expect_eq "import aggiunto dal client" "$(meta "$id" import.infra)" "uc26-infra-100@1.0.0"
expect_eq "VM sulla 1.0.1" "$(currents uc26-os eu1-b eu1-c)" "1.0.1 1.0.1"

info "-- export non valido: il produttore fallisce, chi dipende non parte"
bad=$(submit "$(infra uc26-infra-b uc26-infra-110 1.1.0-evil)")
dep=$(submit "$(consumer uc26-os-c uc26-os-110 1.1.0 "$bad")")
expect_eq "il produttore finisce in ERROR" "$(wait_state "$bad" terminal 120)" ERROR
expect_match "l'errore cita l'export e il pattern" "$(manent status "$bad" | jq -r .error)" "exports\.vms.*evil\.example.*does not match"
expect_eq "il consumatore finisce in DEPENDENCY_FAILED" "$(wait_state "$dep" terminal 60)" DEPENDENCY_FAILED
expect_eq "le VM restano sulla 1.0.1" "$(currents uc26-os eu1-b eu1-c)" "1.0.1 1.0.1"
expect_eq "nessun export registrato per il deploy fallito" "$(manent status "$bad" | jq -r '.exports // "nessuno"')" nessuno
expect_match "un import di un deploy già fallito è rifiutato all'invio" \
  "$(manent_err deploy --no-wait -f "$(consumer uc26-os-g uc26-os-112 1.1.1 "$bad")")" "uc26-infra-110 ended in ERROR"
# The descriptors with inline YAML maps are written first: inside a nested command
# substitution the shell would mangle them.
d=$(descriptor uc26-os-d uc26-os-111 uc26-os prod 1.1.1 "after: [$infra_id]
imports:
  infra: {deployment: $infra_id}
deploy:
  targetsFrom: [{import: infra, export: nope}]")
expect_match "un export inesistente è rifiutato all'invio" "$(manent_err deploy --no-wait -f "$d")" 'no export "nope"'

info "-- stage: import dalla release in esercizio nella configurazione di ambiente"
id=$(submit "$(descriptor uc26-os-e uc26-os-stage-110 uc26-os stage 1.1.0)")
expect_eq "deploy in stage riuscito senza target nel descrittore" "$(wait_state "$id" terminal 120)" SUCCESS
expect_eq "letta la release in esercizio (il deploy fallito non conta)" "$(meta "$id" import.infra)" "uc26-infra-100@1.0.0"
expect_eq "VM sulla 1.1.0" "$(currents uc26-os eu1-b eu1-c)" "1.1.0 1.1.0"
d=$(descriptor uc26-os-f uc26-os-stage-111 uc26-os stage 1.1.1 "deploy:
  targets: [{host: eu1-a, user: deploy}]")
expect_match "con targets_from il descrittore non può aggiungere target" "$(manent_err deploy --no-wait -f "$d")" "targets_from"

info "-- release composta: l'import nomina un altro deploy della release"
infra uc26-r1-infra "" 1.2.0 >/dev/null
descriptor uc26-r1-os "" uc26-os prod 1.2.0 "imports:
  infra: {deployment: infra}
$refs" >/dev/null
cat >"$WORK/uc/uc26-r1.yml" <<'EOF'
apiVersion: manent/v1
kind: Release
id: uc26-r1
deploys:
  - {name: os, file: uc26-r1-os.yml, after: [infra]}
  - {name: infra, file: uc26-r1-infra.yml}
EOF
out=$(manent release submit -f /uc/uc26-r1.yml --interval 1s)
expect_eq "la release riesce" "$(printf '%s' "$out" | jq -r .state)" SUCCESS
expect_eq "os ha letto gli export di uc26-r1.infra" "$(meta uc26-r1.os import.infra)" "uc26-r1.infra@1.2.0"
expect_eq "VM sulla 1.2.0" "$(currents uc26-os eu1-b eu1-c)" "1.2.0 1.2.0"

info "-- promozione: niente import del deploy di origine, i target vengono da stage"
id=$(manent promote -a uc26-os --from prod --to stage --no-wait | jq -r .id)
expect_eq "promozione in stage riuscita" "$(wait_state "$id" terminal 120)" SUCCESS
expect_eq "la promozione legge la release in esercizio di uc26-infra" "$(meta "$id" import.infra)" "uc26-r1.infra@1.2.0"
expect_eq "la promozione non eredita la dipendenza" "$(manent status "$id" | jq -r '.after // "nessuna"')" nessuna
