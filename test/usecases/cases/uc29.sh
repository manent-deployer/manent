# UC29: chain infrastructure -> configuration -> application in one composite release. The
# Terraform plugin "creates" two VMs and exports them with their labels; the Ansible plugin
# configures them on its own queue, served only by worker-ansible (dbos.plugins: the distroless
# workers have neither Python nor the plugin), and exports the application directory with
# set_stats; remote-copy releases the application there. Then: check mode, the release marker
# and a playbook failing on a host.
log "UC29 · catena Terraform → Ansible → applicazione"

dc --profile ansible up -d worker-ansible >/dev/null 2>&1 || dc --profile ansible up -d worker-ansible
t=0
until dc logs worker-ansible 2>/dev/null | grep -q 'worker started'; do
  t=$((t + 1)); [ "$t" -gt 60 ] && { dc logs worker-ansible | tail -20; break; }
  sleep 1
done

# Terraform module: the VMs eu1-b and eu1-c with region and az labels.
d=$(mktemp -d)
cat >"$d/main.tf" <<'TF'
terraform {
  backend "pg" {}
}
output "vm_hosts" {
  value = [for h in ["eu1-b", "eu1-c"] : { host = h, user = "deploy", labels = { region = "eu-1", az = "eu-1${substr(h, 4, 1)}" } }]
}
TF
COPYFILE_DISABLE=1 tar -czf "$WORK/artifacts/uc29-infra-1.0.0.tgz" -C "$d" .
rm -rf "$d"

# Ansible project: a role writes the configuration, set_stats exports the application directory.
# osproject <version> [fail-host]
osproject() {
  d=$(mktemp -d)
  mkdir -p "$d/roles/base/tasks"
  cat >"$d/site.yml" <<YML
- hosts: all
  gather_facts: false
  roles: [base]
  tasks:
    - name: fail on request
      ansible.builtin.fail: {msg: "simulated failure"}
      when: inventory_hostname == "${2:-none}"
    - name: export
      run_once: true
      ansible.builtin.set_stats:
        data: {app_dir: /home/deploy/apps/uc29-app}
YML
  cat >"$d/roles/base/tasks/main.yml" <<'YML'
- name: application directory
  ansible.builtin.file: {path: /home/deploy/apps/uc29-app, state: directory, mode: "0755"}
- name: os configuration
  ansible.builtin.copy:
    dest: /home/deploy/uc29-os.conf
    content: "{{ manent_version }} {{ group_names | sort | join(',') }} {{ motd }}\n"
YML
  COPYFILE_DISABLE=1 tar -czf "$WORK/artifacts/uc29-os-$1.tgz" -C "$d" .
  rm -rf "$d"
}
osproject 1.0.0
osproject 1.1.0
osproject 1.2.0 eu1-c
artifact uc29-app 1.0.0 >/dev/null

art_descriptor uc29-r1-infra "" uc29-infra prod 1.0.0 terraform "file:///srv/artifacts/uc29-infra-1.0.0.tgz" >/dev/null
os_desc() { art_descriptor "$1" "$2" uc29-os prod "$3" ansible "file:///srv/artifacts/uc29-os-$3.tgz" "${5:-}imports:
  infra: {deployment: $4}
deploy:
  targetsFrom: [{import: infra, export: vms}]
  params: {var.motd: welcome}"; }
os_desc uc29-r1-os "" 1.0.0 infra >/dev/null
art_descriptor uc29-r1-app "" uc29-app prod 1.0.0 application "file:///srv/artifacts/uc29-app-1.0.0.tar.gz" "imports:
  infra: {deployment: infra}
  os: {deployment: os}
deploy:
  targetsFrom: [{import: infra, export: vms}]
  paramsFrom: {dest_dir: {import: os, export: app_dir}}" >/dev/null
cat >"$WORK/uc/uc29-r1.yml" <<'YML'
apiVersion: manent/v1
kind: Release
id: uc29-r1
deploys:
  - {name: infra, file: uc29-r1-infra.yml}
  - {name: os, file: uc29-r1-os.yml, after: [infra]}
  - {name: app, file: uc29-r1-app.yml, after: [infra, os]}
YML
conf() { dc exec -T "$1" cat /home/deploy/uc29-os.conf 2>/dev/null | tr -d '\r'; }

info "-- release composta: infrastruttura, configurazione e applicazione"
out=$(manent release submit -f /uc/uc29-r1.yml --interval 1s)
expect_eq "la release riesce" "$(printf '%s' "$out" | jq -r .state)" SUCCESS
st=$(manent status uc29-r1.os)
expect_eq "il deploy Ansible gira sulla coda del plugin, nel worker che la serve" "$(printf '%s' "$st" | jq -r '"\(.queue) \(.executor)"')" "ansible worker-ansible"
expect_eq "configurazione scritta con i gruppi delle etichette" "$(conf eu1-b) / $(conf eu1-c)" \
  "1.0.0 az_eu_1b,region_eu_1 welcome / 1.0.0 az_eu_1c,region_eu_1 welcome"
expect_eq "export da set_stats" "$(printf '%s' "$st" | jq -r .exports.app_dir)" /home/deploy/apps/uc29-app
expect_eq "conteggi per host nel risultato" "$(printf '%s' "$st" | jq -r '.result.steps[] | select(.name == "deploy") | .output.result | fromjson | .targets[] | select(.host == "eu1-b") | .details.changed')" 2
expect_eq "applicazione nella directory esportata, sulle VM create" "$(currents uc29-app eu1-a eu1-b eu1-c)" "none 1.0.0 1.0.0"
expect_match "marcatore della release sui target" "$(dc exec -T eu1-b cat /home/deploy/.manent/releases/untenanted/uc29-os/uc29-os/prod.json 2>/dev/null)" '"version": *"1.0.0"'
expect_eq "nessun deploy Ansible sui worker distroless" "$(manent list -a uc29-os -e prod | jq -r '[.[] | select(.executor != null and .executor != "worker-ansible")] | length')" 0
expect_eq "coda del plugin in manent queues" "$(manent queues --json | jq -r '.[] | select(.name == "ansible") | .kind')" plugin

info "-- dry-run: check mode, nessuna modifica"
# A dry-run cannot wait for (nor import) another deploy: the target is in the descriptor.
d=$(art_descriptor uc29-os-dry uc29-os-110 uc29-os prod 1.1.0 ansible "file:///srv/artifacts/uc29-os-1.1.0.tgz" "deploy:
  targets: [{host: eu1-b, user: deploy, labels: {region: eu-1, az: eu-1b}}]
  params: {var.motd: welcome}")
id=$(manent deploy --no-wait --dry-run -f "$d" | jq -r .id)
expect_eq "dry-run riuscito" "$(wait_state "$id" terminal 180)" SUCCESS
expect_eq "il check mode prevede la modifica della configurazione" "$(manent status "$id" | jq -r '.result.steps[] | select(.name == "deploy") | .output.result | fromjson | .targets[0].details.changed')" 1
expect_eq "configurazione invariata" "$(conf eu1-b | cut -d' ' -f1)" 1.0.0

info "-- host che fallisce: errore con il riepilogo per host"
id=$(submit "$(os_desc uc29-os-c uc29-os-120 1.2.0 uc29-r1.infra "after: [uc29-r1.infra]
")")
expect_eq "deploy fallito" "$(wait_state "$id" terminal 240)" ERROR
expect_match "il motivo nomina l'host" "$(manent status "$id" | jq -r .error)" 'eu1-c \(failed 1, unreachable 0\)'
expect_eq "su eu1-b la configurazione è già della 1.2.0, su eu1-c anche (il ruolo gira prima del fallimento)" "$(conf eu1-b | cut -d' ' -f1) $(conf eu1-c | cut -d' ' -f1)" "1.2.0 1.2.0"

dc --profile ansible stop -t 5 worker-ansible >/dev/null 2>&1
