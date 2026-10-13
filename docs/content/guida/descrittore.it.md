# Descrittore di deploy

Il descrittore dichiara **cosa** rilasciare e **dove**. La CLI lo legge in YAML, l'API lo riceve in JSON: i campi sono gli stessi. I campi sconosciuti sono un errore, così un refuso non viene ignorato in silenzio.

## Esempio completo

=== "YAML (CLI)"

    ```yaml
    apiVersion: manent/v1
    id: myapp-1.2.3-test               # opzionale: chiave di idempotenza
    application: myapp
    environment: test

    artifact:
      type: application
      name: myapp
      version: 1.2.3
      source: https://artifacts.example.com/myapp/myapp-1.2.3.tar.gz
      sha256: 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08

    secrets:
      - name: deploy-key
        ref: env:DEPLOY_SSH_KEY

    deploy:
      plugin: remote-copy
      targets:
        - host: 192.168.1.10
          port: 22
          user: deploy
      params:
        dest_dir: /opt/myapp/releases
        ssh_key_secret: deploy-key

    steps:
      post-test: true

    postTest:
      http:
        - url: http://192.168.1.10:8080/health
          expectStatus: 200
    ```

=== "JSON (API)"

    ```json
    {
      "apiVersion": "manent/v1",
      "id": "myapp-1.2.3-test",
      "application": "myapp",
      "environment": "test",
      "artifact": {
        "type": "application",
        "name": "myapp",
        "version": "1.2.3",
        "source": "https://artifacts.example.com/myapp/myapp-1.2.3.tar.gz"
      },
      "secrets": [{"name": "deploy-key", "ref": "env:DEPLOY_SSH_KEY"}],
      "deploy": {
        "plugin": "remote-copy",
        "targets": [{"host": "192.168.1.10", "port": 22, "user": "deploy"}],
        "params": {"dest_dir": "/opt/myapp/releases", "ssh_key_secret": "deploy-key"}
      },
      "steps": {"post-test": true},
      "postTest": {"http": [{"url": "http://192.168.1.10:8080/health", "expectStatus": 200}]}
    }
    ```

## Campi

### Identificazione

| Campo | Obbligatorio | Descrizione |
|---|---|---|
| `apiVersion` | sì | Sempre `manent/v1`. |
| `id` | no | ID del deploy e chiave di idempotenza: lo stesso `id` non viene eseguito due volte. Non ammesso con `schedule.cron`. |
| `application` | sì | Nome dell'applicazione. |
| `environment` | sì | Ambiente di destinazione (es. `test`, `prod`). |

`id`, `application`, `environment` e il nome della schedule diventano nomi di directory e chiavi: sono ammessi lettere, cifre, `.`, `_`, `:` e `-`, con iniziale alfanumerica e al massimo 128 caratteri.

### `artifact`

| Campo | Obbligatorio | Descrizione |
|---|---|---|
| `type` | sì | Tipo di artefatto (es. `application`). Determina quali plugin possono gestirlo. |
| `name` | sì | Nome dell'artefatto. |
| `version` | no | Versione, riportata ai plugin e nei log. |
| `source` | sì | Da dove scaricarlo: percorso locale, `file://`, `http(s)://` oppure un'immagine di container `oci://registry/repository:tag` (vedi il plugin [container](../plugins/container.md)). |
| `sha256` | no | Hash atteso, verificato dallo step `qa`. |

### `secrets`

Elenco di segreti con un nome logico e un **riferimento**. Il valore non compare mai nel descrittore: viene letto dal worker solo quando serve.

```yaml
secrets:
  - name: deploy-key
    ref: env:DEPLOY_SSH_KEY      # variabile d'ambiente del worker
  - name: db-password
    ref: file:/run/secrets/db    # contenuto di un file sul worker
```

Schemi supportati: `env:`, `file:` e, se configurato, `vault:` (es. `vault:secret/data/myapp/prod#ssh_key`). Vedi [Segreti e sorgenti degli artefatti](../operazioni/segreti-artefatti.md). I nomi logici devono essere univoci. I plugin, i callback e i parametri riferiscono i segreti per nome logico (es. `ssh_key_secret: deploy-key`).

### `deploy`

| Campo | Descrizione |
|---|---|
| `policy` | Policy di scelta del plugin (default: `default`). |
| `plugin` | Plugin da usare. Se assente, la policy sceglie il primo plugin attivo che supporta `artifact.type`. |
| `targets` | Destinazioni: `host` (obbligatorio), `port`, `user`, `labels` (etichette; `region` e `az` sono usate dai [rilasci a ondate](ondate.md)). |
| `params` | Parametri specifici del plugin, come mappa di stringhe. |

Tutti questi campi possono arrivare dalla [configurazione di ambiente](configurazione-ambiente.md): il descrittore può solo aggiungere ciò che non vi è definito. Dopo la fusione deve esserci almeno un target.

### `steps`

Attiva o disattiva gli step opzionali per nome. Vedi [Pipeline e step](../concetti/pipeline.md#il-piano).

### `postTest`

Controlli HTTP eseguiti dallo step `post-test`:

```yaml
postTest:
  http:
    - url: https://myapp.example.com/health
      expectStatus: 200   # default 200
```

### `schedule`

Pianifica il deploy invece di eseguirlo subito: `at` per un orario singolo, `cron` per i deploy ricorrenti. Vedi [Deploy pianificati](pianificazione.md).

### `dryRun`

`true` esegue il deploy in simulazione. Non ammesso con `schedule`. Vedi [Dry-run](dry-run.md).

### `callbacks`

Fino a 5 endpoint HTTP che ricevono l'esito degli step e del deploy. Vedi [Callback](callback.md).

### `rollback`

Attiva o disattiva il rollback automatico, nei limiti della policy del tipo di artefatto. Vedi [Rollback](rollback.md).

```yaml
rollback:
  enabled: true              # default: quello della policy del tipo di artefatto
  on: [deploy, post-test]    # step il cui fallimento avvia il rollback
  locator: plugin            # come individuare la release a cui tornare
```

### `rollout`

Strategia di [rilascio a ondate](ondate.md): ondate, tempo di osservazione, health gate, comportamento in caso di fallimento. Di norma sta nella configurazione di ambiente; nel descrittore è ammessa solo se l'ambiente non ne definisce una, o se è identica.

### `after` e `afterTimeout`

ID dei deploy che devono riuscire prima che questo parta, e l'attesa massima (default `24h`). Fino ad allora il deploy è `WAITING_DEPENDENCY` e non occupa uno slot della coda dell'ambiente. Vedi [Catene di deploy](catene.md).

```yaml
after: [shop-db-42-prod]
afterTimeout: 2h
```

### `freezeOverride`

Motivo con cui il deploy deroga a una [finestra di freeze](governance.md#finestre-di-freeze) (al massimo 500 caratteri, non ammesso con `schedule.cron`). Richiede il permesso `deployment.freeze-override` ed è registrato nell'audit.

```yaml
freezeOverride: "INC-4711: correzione del calcolo IVA"
```

## Validazione

Il descrittore viene validato appena letto, sia dalla CLI sia dall'API. Gli errori vengono restituiti tutti insieme, non solo il primo. Alcuni controlli avvengono più tardi, perché dipendono da altro:

- i conflitti con la configurazione di ambiente, la presenza di almeno un target e la [policy sui descrittori](../operazioni/sicurezza.md#cosa-puo-chiedere-un-descrittore) (parametri, segreti e target che il descrittore può aggiungere), all'accodamento (`422`) e di nuovo nello step `config`. Se all'accodamento la configurazione di ambiente non è raggiungibile, la verifica resta allo step `config`;
- le regole di [governance](governance.md) (freeze, promozione obbligatoria, approvazioni), all'accodamento; il freeze anche nello step `config`;
- la sorgente dell'artefatto, all'accodamento e al download (`artifacts.sources`);
- i permessi dell'utente sull'applicazione e sull'ambiente, all'accodamento;
- la risolvibilità dei segreti, nello step `secrets`;
- la policy sugli URL dei callback e la policy di rollback, all'accodamento. La decisione di rollback definitiva è presa nello step `config`, dopo la fusione con la configurazione di ambiente.

`manent validate` esegue la validazione e mostra il deploy effettivo senza accodare nulla.
