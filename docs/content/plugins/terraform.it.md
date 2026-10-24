# terraform

Il plugin `terraform` rilascia infrastruttura: l'artefatto è un root module Terraform, ogni target dell'ambiente è uno stato Terraform e un deploy è `init`, `plan` e `apply` di quel piano. Le modifiche all'infrastruttura passano così dalla stessa pipeline delle applicazioni: governance, approvazioni, freeze, audit, dry-run, inventario e [catene di deploy](../guida/catene.md). Il binario di default è [OpenTofu](https://opentofu.org); funziona anche `terraform`.

È un [plugin esterno](esterni.md), sviluppato in un repository a sé: [manent-deployer/manent-plugin-terraform](https://github.com/manent-deployer/manent-plugin-terraform). L'immagine `manent-server` contiene sia il binario del plugin sia `tofu`.

## Come si svolge un deploy

Per ogni target, uno dopo l'altro:

1. `init -input=false -lockfile=readonly`, con `-backend-config` dai parametri `backend.*`;
2. `workspace select -or-create`, se `workspace` è impostato;
3. `plan -detailed-exitcode -out=<file>` e `show -json` del piano: lo step riporta quante risorse vengono aggiunte, modificate e distrutte e i loro indirizzi, mai i valori;
4. se il piano distrugge più risorse di `max_destroy` (default 0), il deploy fallisce **senza retry**, prima di applicare qualunque cosa;
5. `apply` esattamente di quel file di piano, solo se contiene modifiche;
6. `output -json`: gli output non sensibili diventano output del plugin.

Un [dry-run](../guida/dry-run.md) si ferma dopo il controllo del punto 4: chi approva il deploy vede le modifiche previste nello step `deploy` del dry-run. Rieseguire la stessa versione non pianifica modifiche e non applica nulla, quindi retry e recovery di DBOS sono sicuri.

## L'artefatto

Un `.tgz` con il root module e il suo `.terraform.lock.hcl`: i provider si installano solo con le versioni e i checksum fissati dal lock file, e un artefatto che non lo contiene viene rifiutato (`require_lock_file: false` lo consente).

```sh
tofu init -backend=false
tofu providers lock -platform=linux_amd64 -platform=linux_arm64
tar -czf network-1.4.0.tgz --exclude=.terraform *.tf .terraform.lock.hcl
```

```yaml title="deploy.yml"
apiVersion: manent/v1
application: infra
environment: prod
artifact:
  type: terraform
  name: network
  version: 1.4.0
  source: https://nexus.example.com/repository/infra/network-1.4.0.tgz
```

Il modulo può stare alla radice dell'archivio, nella sua unica directory di primo livello o nella directory indicata da `module_dir`.

## Configurazione

```yaml title="manent.yml"
plugins:
  terraform:
    external:
      command: /usr/local/bin/manent-plugin-terraform
      sha256: "<sha256 del binario>"
    artifact_types: [terraform]               # default
    binary: tofu                              # default; oppure terraform, o un percorso assoluto
    plugin_cache_dir: /var/cache/manent/tf    # cache dei provider condivisa dalle esecuzioni (facoltativa)
    provider_mirror: https://mirror.example.com/providers/   # facoltativo, per gli ambienti isolati
```

Lo stato non passa mai da manent: ogni target usa un backend remoto con lock (S3, `azurerm`, `gcs`, `pg`, `consul`, `http`), cifrato e leggibile solo dalle identità dei worker. Le credenziali del backend e dei provider arrivano dai segreti, come variabili d'ambiente del processo.

```yaml title="Configurazione di ambiente: infra/network/prod"
plugin: terraform
targets:                                       # un target = uno stato
  - {host: prod-eu, labels: {region: eu-1}}
  - {host: prod-us, labels: {region: us-1}}
params:
  workspace: "{host}"
  backend.bucket: acme-tfstate
  backend.key: network/{environment}/{host}.tfstate
  backend.region: eu-west-1
  var.cidr: 10.20.0.0/16
  secret_var.db_password: db-password          # TF_VAR_db_password da un segreto
  env.AWS_ACCESS_KEY_ID: aws-id                # nome di un segreto
  env.AWS_SECRET_ACCESS_KEY: aws-secret
  max_destroy: "0"
secrets:
  - {name: aws-id, ref: "vault:secret/data/infra/aws#id"}
  - {name: aws-secret, ref: "vault:secret/data/infra/aws#secret"}
  - {name: db-password, ref: "vault:secret/data/infra/db#password"}
exports:
  vms: {from: deploy.vm_hosts, format: targets}
```

| Parametro | Descrizione |
|---|---|
| `workspace` | Workspace di ogni target, creato se manca. Vuoto: il workspace di default. |
| `backend.<chiave>` | Impostazione del backend passata come `-backend-config=<chiave>=<valore>`. |
| `var.<nome>` | Variabile passata come `TF_VAR_<nome>`. |
| `secret_var.<nome>` | Variabile passata come `TF_VAR_<nome>`, con il valore del segreto indicato. |
| `env.<NOME>` | Variabile d'ambiente con il valore del segreto indicato (credenziali di provider e backend). |
| `max_destroy` | Numero massimo di risorse che un piano può distruggere (default `0`). Una sostituzione conta come distruzione. |
| `module_dir` | Directory del root module dentro l'artefatto. |
| `parallelism` | `-parallelism` di `plan` e `apply` (1-256). |
| `timeout` | Durata massima dell'esecuzione su un target (default `1h`). |
| `require_lock_file` | `false` consente un modulo senza `.terraform.lock.hcl` (default `true`). |

`workspace`, `backend.*` e `var.*` accettano i segnaposto `{host}`, `{application}`, `{artifact}`, `{environment}` e `{tenant}`. Due target che condividerebbero lo stesso stato (stesso workspace e stesse impostazioni del backend) vengono rifiutati in validazione. Anche i parametri sconosciuti vengono rifiutati.

!!! tip "Cosa può impostare il descrittore"
    Il plugin esegue codice dell'artefatto con credenziali potenti. Tenete `backend.*` ed `env.*` nella configurazione di ambiente e concedete ai descrittori al più `var.*`, con [`descriptor_policy.params`](../operazioni/sicurezza.md#cosa-puo-chiedere-un-descrittore).

## Isolamento

- Ogni esecuzione ha directory di lavoro, `HOME`, `TF_DATA_DIR` e file di configurazione della CLI propri, rimossi alla fine: nulla arriva dalla home del worker o da una configurazione della CLI dell'host.
- Il processo riceve dal worker solo `PATH`, `TZ` e `LANG`, più le variabili impostate dal plugin.
- I segreti vengono risolti a ogni esecuzione e passati solo nell'ambiente del processo; sono redatti dai messaggi di errore e non compaiono mai negli output degli step. Gli output sensibili non vengono mai esportati.

## Output e catene

Con un target gli output del plugin sono i nomi degli output Terraform; con più target ogni output è `<host>.<nome>`, più `<nome>` quando tutti i target hanno lo stesso valore. Liste e oggetti diventano JSON compatto, quindi una lista di host si esporta con `format: targets` e diventa l'elenco dei target del deploy successivo:

```yaml title="Configurazione di ambiente dell'applicazione che gira su quelle VM"
imports:
  infra: {current: {application: infra, artifact: network}}
targets_from: [{import: infra, export: vms}]
```

Vedi [Export e import](../guida/catene.md#export-import) per la catena completa.

## Ondate, rollback e limiti

- **Rilasci a ondate:** i target con le etichette `region` e `az` funzionano con i [rilasci a ondate](../guida/ondate.md): ogni stadio riceve solo i suoi stati, con bake, health gate e approvazioni in mezzo.
- **Rollback:** solo manuale. `manent rollback` applica la versione del modulo rilasciata in precedenza. Non elencate il tipo `terraform` in `rollback.artifact_types`: un apply fallito lascia lo stato come l'ha lasciato Terraform, e ricreare infrastruttura in automatico può distruggere dati.
- **Non supportato:** `destroy` come operazione di manent. Sono previsti l'applicazione esatta del piano approvato in un dry-run e la deriva dell'infrastruttura nella [riconciliazione](../guida/governance.md#stato-reale).
