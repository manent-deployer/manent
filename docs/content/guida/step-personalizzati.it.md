# Step personalizzati

Gli step personalizzati aggiungono fasi alla pipeline senza scrivere codice Go: una **richiesta HTTP** (invalidare una CDN, avvisare un sistema, avviare un job e attenderne l'esito) o un **comando sui target** via SSH (una migrazione del database, il riscaldamento di una cache). Hanno le stesse garanzie degli step standard: ogni esecuzione è uno step DBOS con retry, un deploy ripreso dopo un crash riparte da dove si era fermato, l'output finisce nel log del deploy e ogni esecuzione è registrata nell'audit.

Comandi e URL si scrivono **solo in `manent.yml`**. La configurazione di ambiente fornisce i parametri di ogni applicazione; il descrittore può solo attivare o disattivare gli step opzionali e, se la policy lo ammette, impostare alcuni parametri.

```yaml title="manent.yml"
pipeline:
  custom:
    spec_key_refs: ["vault:secret/data/manent/pipeline#spec_key"]
  steps:
    - {name: secrets, mode: mandatory}
    - {name: artifacts, mode: mandatory, max_retries: 3}
    - name: db-migrate
      mode: optional
      enabled: false                 # attivato per applicazione dal descrittore
      type: command
      max_retries: 0
      timeout: 15m
      secrets: [db-admin]            # nomi logici, dalla sezione secrets
      ssh: {user: migrator, key_secret: ssh-migrate}
      params:
        database: {pattern: "[a-z][a-z0-9_]{0,62}", required: true}
      outputs:
        applied: {pattern: "[0-9]{1,6}"}
      run:
        run_once: true               # su un solo target, non su tutti
        command: ["/opt/tools/migrate", "up", "--to", "{{ .Version }}", "--db", "{{ .Params.database }}"]
      dry_run:
        command: ["/opt/tools/migrate", "plan", "--to", "{{ .Version }}", "--db", "{{ .Params.database }}"]
        secrets: [db-readonly]       # il dry-run ha segreti propri, di sola lettura
    - {name: deploy, mode: mandatory, max_retries: 2}
    - {name: activation, mode: optional, enabled: true}
    - name: cdn-purge
      mode: optional
      enabled: true
      type: http
      secrets: [cdn-token]
      params:
        zone: {pattern: "[a-f0-9]{32}", required: true}
      outputs:
        purge_id: {pattern: "[a-z0-9-]{1,64}", path: "$.result.id"}
      run:
        method: POST
        url: "https://api.cdn.example.com/zones/{{ .Params.zone }}/purge"
        auth: {type: bearer, secret: cdn-token}
        json: {tags: ["{{ .Application }}"]}
        poll:
          url: "https://api.cdn.example.com/purges/{{ .Response.result.id }}"
          until: {path: "$.status", in: [done]}
          fail: {path: "$.status", in: [failed]}
```

```yaml title="envconfig/shop/shop/prod.yml"
step_params:
  db-migrate: {database: shop}
  cdn-purge: {zone: 0f3a9c1e5b7d42a8a1c3e5f7091b2d4c}
secrets:
  - {name: db-admin, ref: "vault:secret/data/shop/prod#db_admin"}
  - {name: db-readonly, ref: "vault:secret/data/shop/prod#db_readonly"}
  - {name: ssh-migrate, ref: "vault:secret/data/shop/prod#ssh_migrate"}
  - {name: cdn-token, ref: "vault:secret/data/shop/prod#cdn_token"}
```

La posizione in `pipeline.steps` decide quando lo step viene eseguito. Con i [rilasci a ondate](ondate.md) uno step tra `deploy` e `activation` gira per ogni lotto, con i target del lotto; uno step prima di `deploy` o dopo `activation` gira una volta sola.

## Chi decide cosa

Uno step command personalizzato è esecuzione di codice remoto: chi scrive `manent.yml` può eseguire comandi su tutti i target. È accettato, perché è la stessa fiducia di chi configura i plugin. Tutto il resto fa sì che **nessun altro** ottenga la stessa capacità.

| Elemento | `manent.yml` | Configurazione di ambiente | Descrittore |
|---|---|---|---|
| Comando, URL, metodo, posizione, tipo, `mode` | sì | no | no |
| Dichiarazione di parametri e output (pattern) | sì | no | no |
| Attivazione di uno step opzionale | default | no | sì |
| Valori dei parametri | default | sì (`step_params`) | solo quelli ammessi da `descriptor_policy.step_params` (`stepParams`) |
| Riferimenti ai segreti | nomi logici | sì | secondo `descriptor_policy.secret_refs` |

Uno step che fa da controllo (una scansione, la verifica di una firma) va dichiarato `mandatory`: il descrittore può disattivare uno step opzionale.

```yaml title="manent.yml"
descriptor_policy:
  step_params:
    db-migrate: [database]   # il descrittore può impostare stepParams.db-migrate.database
```

```yaml title="Descrittore"
steps: {db-migrate: true}
stepParams:
  db-migrate: {database: shop}
```

## Parametri e segnaposto

Ogni parametro è dichiarato con un `pattern` (un'espressione regolare che il valore deve rispettare per intero) e, facoltativamente, `required` e `default`. Un valore non conforme, un parametro non dichiarato o uno obbligatorio mancante rifiutano il deploy all'accodamento, se la configurazione di ambiente è leggibile, o al più tardi quando lo step viene eseguito.

I segnaposto usano la sintassi dei template di Go, solo con questi valori:

| Segnaposto | Valore |
|---|---|
| `{{ .Application }}`, `{{ .Environment }}` | Applicazione e ambiente. |
| `{{ .Version }}`, `{{ .Artifact }}` | Versione e nome dell'artefatto. La versione deve rispettare `^[0-9A-Za-z][0-9A-Za-z.+_-]{0,127}$`, altrimenti lo step fallisce. |
| `{{ .DeploymentID }}` | ID del deploy. |
| `{{ .SHA256 }}` | SHA-256 dell'artefatto. |
| `{{ .DryRun }}` | `true` in dry-run. |
| `{{ .Params.<nome> }}` | Un parametro dichiarato. |
| `{{ .Outputs.<step>.<chiave> }}` | Un output di uno step precedente (`{{ index .Outputs "post-test" "chiave" }}` per i nomi con `-`). |
| `{{ .Response.<campo> }}` | Solo in `poll.url`: un campo della prima risposta. |

I valori arrivano da chi rilascia, dalla configurazione di ambiente o da altri target: **non sono fidati** e vengono controllati in base al contesto in cui finiscono.

- Un segnaposto sconosciuto o un parametro non dichiarato è un errore di configurazione, rilevato all'avvio.
- Un valore mancante è un errore, mai `<no value>` o una stringa vuota.
- In un comando un valore sostituito non può rendere vuoto l'argomento, aggiungere caratteri di controllo o iniziare con `-` se l'argomento scritto non inizia così (`-oProxyCommand=…` verrebbe letto come un'opzione).
- In un URL ogni valore sostituito è codificato in percentuale, quindi resta dentro un solo segmento del percorso o un solo valore della query, e i segmenti `.` e `..` sono rifiutati. Schema e host vanno scritti per intero, senza segnaposto.
- Il corpo JSON è una struttura che manent serializza: i segnaposto sono ammessi solo nei valori stringa e un valore non può cambiarne la struttura.

## Step command

`type: command` esegue un comando sui target del deploy via SSH, un target dopo l'altro, o solo sul primo con `run_once`. Il comando è una lista di argomenti il cui primo elemento è un percorso assoluto senza segnaposto: non è mai una stringa passata a una shell.

| Chiave | Descrizione |
|---|---|
| `ssh.key_secret`, `ssh.password_secret` | Nomi logici delle credenziali SSH (almeno una). |
| `ssh.user` | Utente del comando al posto di quello del target: meglio un utente dedicato, con i soli privilegi che servono allo step. |
| `run.run_once` | Esegue solo sul primo target. Obbligatorio se lo step dichiara output. |
| `run.command` | Il comando e i suoi argomenti. |
| `run.sha256` | SHA-256 di `command[0]`, verificato sul target prima di ogni esecuzione (`sha256sum` o `shasum -a 256`). Protegge da un binario sbagliato, non da un target compromesso. |

Su ogni target e a ogni tentativo manent:

1. crea `<secrets_dir>/<deploy>/<step>/<casuale>` con `umask 077`, rimuovendo le directory dei tentativi precedenti, anche quelle lasciate da un crash. `secrets_dir` è `/dev/shm/manent` per default, quindi i segreti restano in memoria;
2. scrive il file d'ambiente e un file per segreto (`0600`) sul canale SSH, mai sulla riga di comando;
3. esegue `/bin/sh -c '<script>' manent <dir> <argomenti…>`:
   - ogni argomento è quotato per una shell POSIX e passato come parametro posizionale;
   - le variabili `MANENT_*` sono caricate dal file, quindi non compaiono in `ps`;
   - se `timeout(1)` è disponibile, il comando viene fermato allo scadere del timeout dello step;
4. legge gli output e rimuove la directory.

La shell di login del target deve essere compatibile POSIX (`sh`, `bash`, `dash`, `zsh`); i target Windows non sono supportati. La host key è sempre verificata con `pipeline.custom.known_hosts`.

### Contratto

| Variabile | Valore |
|---|---|
| `MANENT_DEPLOYMENT_ID`, `MANENT_STEP` | ID del deploy e nome dello step: insieme sono la chiave di idempotenza. |
| `MANENT_APPLICATION`, `MANENT_ENVIRONMENT`, `MANENT_VERSION`, `MANENT_TARGET` | Il rilascio e il target corrente. |
| `MANENT_ARTIFACT_SHA256` | SHA-256 dell'artefatto verificato. |
| `MANENT_DRY_RUN` | `true` quando gira `dry_run`. |
| `MANENT_PARAM_<NOME>` | Parametri dello step (`database` → `MANENT_PARAM_DATABASE`). |
| `MANENT_OUTPUT_<STEP>_<CHIAVE>` | Output degli step precedenti (`post-test` → `POST_TEST`). Dati non fidati: possono arrivare da un altro target. |
| `MANENT_SECRETS_DIR` | Directory con un file per segreto dichiarato. |
| `MANENT_OUTPUT` | File in cui lo step scrive righe `chiave=valore`. |

| Esito | Effetto |
|---|---|
| exit `0` | Successo. Le righe di `$MANENT_OUTPUT` diventano l'output dello step **solo** per le chiavi dichiarate in `outputs` e solo se il valore rispetta il loro pattern; qualunque altra cosa fa fallire lo step. Al più 16 KiB. |
| exit `75` (`EX_TEMPFAIL`) | Errore temporaneo: ritentato fino a `max_retries`. |
| altri codici | Errore permanente: nessun retry, rollback se la policy lo prevede. |
| timeout, connessione persa | Errore temporaneo. |

stdout e stderr finiscono nel log del deploy riga per riga, al più 1 MiB per esecuzione e 4 KiB per riga. I valori dei segreti dichiarati vengono sostituiti da `***`, anche in base64, codificati negli URL e con escape JSON. È una difesa aggiuntiva, non una garanzia: un segreto spezzato su più righe, troncato o trasformato dallo script non viene riconosciuto, e il log è leggibile da chi ha `deployment.read`.

!!! warning "Almeno una volta"
    `max_retries: 0` limita i retry, non le riesecuzioni: uno step interrotto da un crash del worker viene rieseguito dalla recovery, perché DBOS non ne ha registrato l'esito. Gli script non idempotenti (una migrazione, uno svuotamento) devono usare `MANENT_DEPLOYMENT_ID` e `MANENT_STEP` come chiave di deduplica.

## Step http

`type: http` invia una richiesta dal worker.

| Chiave | Descrizione |
|---|---|
| `run.method` | `GET`, `POST` (default), `PUT`, `PATCH`, `DELETE`. |
| `run.url` | URL con schema e host scritti per intero; solo `https`, salvo `pipeline.custom.allow_http`. |
| `run.headers` | Header aggiuntivi, con segnaposto nei valori. `Host`, `Content-Type` e gli altri header gestiti da manent non sono ammessi. |
| `run.auth` | `bearer`, `basic` (`username`) o `header` (`header`) con un segreto elencato in `secrets`. |
| `run.json` | Corpo della richiesta. |
| `run.expect` | Status accettati; default qualunque 2xx. Fuori da `expect`: 5xx, 408 e 429 sono temporanei, gli altri permanenti. |
| `run.poll` | Attesa di un'operazione asincrona: `url`, `interval` (default 10s), `timeout` (default quello dello step), `until` e `fail` con `path` e `in`. |
| `outputs.<chiave>.path` | Dove leggere l'output nella risposta JSON (`$.a.b[0]`, `$["a.b"]`). |

Ogni richiesta ha l'header `Idempotency-Key: <deploy>:<step>`. I redirect non vengono seguiti e le risposte sono lette fino a 1 MiB. L'attesa avviene dentro lo step: dopo un crash lo step riparte e ripete la prima richiesta con la stessa chiave. Il log riporta metodo, host, percorso e status, mai query né corpo.

## Dry-run

Uno step con la sezione `dry_run` la esegue nei [dry-run](dry-run.md); senza, lo step viene saltato. Il dry-run non è soggetto alla [governance](governance.md) (né approvazioni né freeze), eppure `dry_run` gira sui target di produzione. Per questo:

- `dry_run` riceve **solo i propri** `secrets`, mai quelli di `run`: dagli credenziali di sola lettura;
- durante un freeze gli step command vengono saltati anche in dry-run (`dry_run: skipped`, `reason: freeze`), salvo `freezeOverride` nel descrittore;
- manent non può verificare che `dry_run` sia davvero di sola lettura: è responsabilità di chi lo scrive.

## La specifica nel piano

All'accodamento il piano registra la **specifica completa** di ogni step personalizzato, in forma canonica, con il suo SHA-256 (`spec_hash`). Un deploy ripreso dopo un crash, uno in attesa di approvazione e le esecuzioni di una [schedule ricorrente](pianificazione.md) eseguono la definizione con cui sono partiti, anche se nel frattempo `manent.yml` cambia. Il piano mostrato da `manent validate`, dall'API e dall'interfaccia web riporta tipo e `spec_hash` di ogni step, mai comandi e URL.

**Firma.** Con `pipeline.custom.spec_key_refs` la specifica viene firmata con HMAC-SHA256 quando si calcola il piano, e il worker verifica la firma prima di eseguirla: chi può scrivere nel database DBOS non può trasformare un piano in comandi arbitrari. La prima chiave firma, tutte verificano. Per ruotare una chiave si aggiunge la nuova in testa e si toglie la vecchia quando non restano deploy o schedule firmati con essa. Senza chiavi le specifiche non sono firmate e il server lo segnala all'avvio.

**Revoca.** Una correzione a `manent.yml` non raggiunge i piani già registrati, soprattutto quelli delle schedule cron. Per fermare una specifica se ne aggiunge l'hash a `pipeline.custom.revoked_specs`: il worker rifiuta di eseguirla e lo step fallisce con un errore permanente, senza cambiare la sequenza degli step dei workflow in corso. L'hash è nel piano (`manent validate`, `POST /api/v1/deployments/plan`) e nelle voci di audit.

```yaml title="manent.yml"
pipeline:
  custom:
    revoked_specs:
      - 3b0f4c…e91a   # db-migrate prima della correzione del 14/10/2026
```

Per sapere quali deploy falliranno e quali schedule vanno riapplicate, `manent-server admin specs` elenca i workflow non ancora conclusi (in coda, pianificati con `schedule.at`, in attesa di approvazione o di dipendenze, in esecuzione) e le schedule cron il cui piano contiene una specifica revocata. Legge `revoked_specs` dalla configurazione, oppure gli hash indicati con `--hash`; `--all` elenca tutte le specifiche fotografate nei piani.

```sh
manent-server -c manent.yml admin specs
manent-server -c manent.yml admin specs --hash 3b0f4c…e91a
```

```json
{
  "hashes": ["3b0f4c…e91a"],
  "uses": [
    {"kind": "schedule", "id": "shop-prod", "status": "ACTIVE", "application": "shop", "environment": "prod", "step": "db-migrate", "spec_hash": "3b0f4c…e91a"},
    {"kind": "workflow", "id": "shop-1.5.0", "workflow": "manent.approval", "status": "PENDING", "application": "shop", "environment": "prod", "step": "db-migrate", "spec_hash": "3b0f4c…e91a"}
  ]
}
```

Un deploy in coda si annulla (`manent cancel`) e si invia di nuovo, così il suo piano fotografa la specifica attuale; una schedule si aggiorna inviando di nuovo il suo descrittore. L'hash viene ricalcolato dalla specifica fotografata: `mismatch` segnala un piano il cui `spec_hash` registrato non corrisponde.

## Audit e visibilità

Ogni esecuzione di uno step personalizzato, compresi tentativi falliti e dry-run, scrive una voce di audit `step.custom.run` con l'identità che ha richiesto il deploy, lo step, l'hash della specifica, i target e l'esito. Gli output restano nello stato del deploy, leggibili con `deployment.read`. I [callback](callback.md), definiti dal descrittore, ricevono l'esito dello step ma **non i suoi output**.

## Riferimento

| `pipeline.steps[]` | Default | Descrizione |
|---|---|---|
| `type` | | `command` o `http`. Senza, lo step è standard. |
| `timeout` | `10m` | Durata massima di un'esecuzione (da 1s a 6h). |
| `secrets` | | Nomi logici dei segreti passati a `run`. |
| `params.<nome>` | | `pattern` (obbligatorio), `required`, `default`. |
| `outputs.<chiave>` | | `pattern` (obbligatorio), `path` per gli step http. |
| `ssh` | | `user`, `key_secret`, `password_secret` (solo command). |
| `run`, `dry_run` | | Esecuzione e dry-run; `dry_run.secrets` elenca i segreti del dry-run. |

| `pipeline.custom` | Default | Descrizione |
|---|---|---|
| `spec_key_refs` | | Riferimenti alle chiavi HMAC (almeno 32 byte) che firmano le specifiche. |
| `revoked_specs` | | SHA-256 delle specifiche da non eseguire più. |
| `known_hosts` | `~/.ssh/known_hosts` | Host key dei target. |
| `insecure_ignore_host_key` | `false` | Solo per lo sviluppo. |
| `secrets_dir` | `/dev/shm/manent` | Directory dei segreti sui target, su tmpfs. |
| `allow_http` | `false` | Ammette URL `http`. |

!!! note "In questa versione"
    I comandi girano solo sui target (`on: targets`). L'esecuzione in un Job Kubernetes, checkpoint e compensazioni per il [rollback](rollback.md) e gli step scritti come plugin esterni sono previsti nelle fasi successive. L'esecuzione di comandi sul worker non è supportata: un processo con l'utente del worker potrebbe leggerne la configurazione e il database.
