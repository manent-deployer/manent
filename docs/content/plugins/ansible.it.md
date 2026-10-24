# ansible

Il plugin `ansible` rilascia la configurazione dei sistemi: l'artefatto è un progetto Ansible (playbook, ruoli e collection), manent costruisce l'inventario dai target che ogni esecuzione riceve e un deploy esegue `ansible-playbook`. Le modifiche di configurazione passano così dalla stessa pipeline delle applicazioni: ondate e lotti, approvazioni, freeze, audit, inventario e [catene di deploy](../guida/catene.md). Un [dry-run](../guida/dry-run.md) è il check mode.

È un [plugin esterno](esterni.md), sviluppato in un repository a sé: [manent-deployer/manent-plugin-ansible](https://github.com/manent-deployer/manent-plugin-ansible).

## Dove gira

Un plugin esterno è un processo figlio del worker, quindi Python e `ansible-core` devono stare sulla macchina del worker. L'immagine distroless di manent non li ha: eseguite il plugin su un gruppo di worker costruiti dal target `server-ansible` del Dockerfile (Python, `ansible-core`, le collection `ansible.posix` e `community.general`, il binario del plugin; `ANSIBLE_COLLECTIONS` ne aggiunge altre) e date al plugin una [coda propria](../operazioni/scalabilita.md#plugin-queues):

```yaml title="manent.yml (tutti i processi)"
plugins:
  ansible:
    external:
      command: /usr/local/bin/manent-plugin-ansible
      sha256: "<sha256 del binario>"
    artifact_types: [ansible]
    queue: ansible
```

```yaml title="manent.yml (solo i worker del gruppo)"
dbos:
  plugins: [ansible]
```

```sh
docker build --target server-ansible -t manent-server-ansible .
```

L'API e i worker distroless conoscono il plugin per nome e tipi di artefatto e non lo avviano mai.

## Come si svolge un deploy

1. Il progetto viene estratto in una directory dell'esecuzione; il playbook (`site.yml` di default) può stare alla radice dell'archivio o nella sua unica directory di primo livello. Le collection in `collections/` e i ruoli in `roles/` vengono trovati da soli.
2. L'inventario elenca i target dell'esecuzione con le variabili di connessione e un gruppo per etichetta: `region=eu-1` diventa il gruppo `region_eu_1`, `az=eu-1a` il gruppo `az_eu_1a`. Le [ondate](../guida/ondate.md) e `maxUnavailable` di manent sostituiscono `serial`: ogni esecuzione riceve solo i target del suo lotto.
3. `ansible-playbook` viene eseguito con argomenti come argv (nessuna shell), il callback JSON e un file di configurazione vuoto: non vengono letti né `/etc/ansible/ansible.cfg` né un `ansible.cfg` dell'artefatto.
4. Un host fallito o irraggiungibile fa fallire l'esecuzione, con i totali di ogni host nell'errore e nel risultato (`ok`, `changed`, `failed`, `unreachable`, `skipped`). Un errore di sintassi, un ruolo mancante o opzioni non valide la fanno fallire senza retry.
5. Con `release_marker: true` un piccolo playbook registra la versione sui target.

I playbook devono essere idempotenti: retry e recovery possono rieseguirli.

## L'artefatto

```sh
ansible-galaxy collection install -r requirements.yml -p collections
tar -czf os-config-2.1.0.tgz site.yml roles collections group_vars
```

```yaml title="deploy.yml"
apiVersion: manent/v1
application: os
environment: prod
artifact:
  type: ansible
  name: os-config
  version: 2.1.0
  source: https://nexus.example.com/repository/ansible/os-config-2.1.0.tgz
```

## Configurazione

```yaml title="Configurazione di ambiente: os/os-config/prod"
plugin: ansible
targets:
  - {host: web-1.prod.internal, user: deploy, labels: {region: eu-1, az: eu-1a}}
  - {host: web-2.prod.internal, user: deploy, labels: {region: eu-1, az: eu-1b}}
params:
  playbook: site.yml
  ssh_key_secret: deploy-key
  become: "true"
  var.ntp_server: ntp.example.com
  secret_var.db_password: db-password
  vault_password_secret: vault-pass
  release_marker: "true"
secrets:
  - {name: deploy-key, ref: "vault:secret/data/os/prod#ssh_key"}
  - {name: db-password, ref: "vault:secret/data/os/prod#db_password"}
  - {name: vault-pass, ref: "vault:secret/data/os/prod#vault_password"}
```

| Parametro | Descrizione |
|---|---|
| `playbook` | Playbook da eseguire, relativo al progetto (default `site.yml`). |
| `ssh_key_secret` | Segreto con la chiave SSH privata, scritta in un file 0600 dell'esecuzione. |
| `become` | `true` aggiunge `--become`. |
| `var.<nome>` | Variabile extra. `manent_*` e `ansible_*` sono riservate. |
| `secret_var.<nome>` | Variabile extra con il valore del segreto indicato. |
| `vault_password_secret` | Segreto con la password di Ansible Vault (`--vault-password-file`). |
| `forks` | `--forks` (1-500). |
| `tags`, `skip_tags` | `--tags` e `--skip-tags` separati da virgole. |
| `timeout` | Durata massima di un'esecuzione (default `1h`). |
| `release_marker` | `true` registra la versione sui target dopo il playbook. |
| `release_marker_dir` | Directory dei marcatori (default `/var/lib/manent/releases`); scritti con `become` quando è impostato. |

Ogni esecuzione riceve anche `manent_deployment`, `manent_tenant`, `manent_application`, `manent_environment`, `manent_artifact`, `manent_version` e `manent_sha256`. I parametri sconosciuti vengono rifiutati.

Configurazione del plugin: `ansible_playbook` (il comando, di default da `PATH`), `known_hosts` (default `~/.ssh/known_hosts` del worker), `insecure_ignore_host_key` (solo per lo sviluppo), `collections_path` (collection del worker, dopo quelle dell'artefatto; default `/usr/share/ansible/collections`).

## Isolamento

- Ogni esecuzione ha directory di lavoro e `HOME` proprie, rimosse alla fine; il processo riceve dal worker solo `PATH`, `TZ` e `LANG`.
- Variabili, variabili segrete, chiave SSH e password di Vault vengono scritte in file 0600 dell'esecuzione. I segreti sono redatti dai messaggi di errore e non compaiono mai negli output degli step; `--diff` non viene mai usato, perché potrebbe mostrarli.

## Output e catene

I dati di `ansible.builtin.set_stats` non per host diventano output del plugin: stringhe così come sono, liste e oggetti come JSON compatto. Di default `set_stats` aggrega i valori di tutti gli host, quindi usate `run_once: true`:

```yaml
- name: export the application directory
  run_once: true
  ansible.builtin.set_stats:
    data: {app_dir: /srv/shop}
```

Una catena tipica è infrastruttura → configurazione → applicazione in una [release composta](../guida/catene.md#release-composte): il plugin [terraform](terraform.md) esporta le VM come target (`format: targets`, con le loro etichette), il deploy ansible le legge con `targetsFrom` ed esporta ciò che serve all'applicazione, che il deploy dell'applicazione legge con `paramsFrom`.

## Release in esercizio, rollback e limiti

- Con `release_marker: true` il plugin legge la versione in esercizio dai marcatori dei target (locator del rollback, [riconciliazione](../guida/governance.md#stato-reale)) e segnala un errore se i target non coincidono. La riconciliazione la legge solo su un worker che serve il plugin.
- Il rollback è manuale: `manent rollback` esegue il playbook della versione rilasciata in precedenza. Sono previsti il rollback automatico con un `rollback_playbook` e `galaxy: install` al momento del deploy.
