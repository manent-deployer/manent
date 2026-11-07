# script

Il plugin `script` esegue uno script **sul worker** per rilasciare un artefatto su un sistema che nessun altro plugin copre: la CLI di un fornitore, un'appliance con una propria API, uno strumento interno. Lo script ha lo stesso deploy di ogni altro plugin: ondate, gate, approvazioni, freeze, rollback, riconciliazione e audit.

Gli script sono un **catalogo in `manent.yml`**, scritto da chi gestisce la piattaforma. La configurazione di ambiente sceglie uno script per nome e passa i parametri che il catalogo dichiara, verificati con i loro pattern. Non indica mai un comando: il worker ha la connessione al database, il secret provider e gli artefatti degli altri deploy, quindi chi può scrivere la configurazione di un'applicazione non deve poterci eseguire codice.

## Configurazione

```yaml title="manent.yml"
plugins:
  script:
    artifact_types: [tgz]          # obbligatorio: i tipi che questo plugin rilascia
    queue: scripts                 # consigliato: li eseguono solo i worker con gli script
    max_parallel: 4
    work_dir: /var/lib/manent/script   # default <directory temporanea>/manent-script
    env:                           # variabili fisse per ogni script; mai segreti
      HTTPS_PROXY: http://proxy.example.com:3128
    scripts:
      f5-legacy:
        timeout: 20m               # default 10m, al più 6h
        per_target: false          # true: un'esecuzione per target
        params:
          partition: {pattern: "[a-z0-9-]+", required: true}
          mode: {pattern: "fast|safe", default: safe}
        secrets: [f5-password]
        outputs:
          vip: {pattern: "[0-9.]+"}
        run:
          command: [/opt/manent/scripts/f5-deploy.sh, --verbose]
          sha256: "3b1f…"          # verificato prima di ogni esecuzione
        dry_run:  {command: [/opt/manent/scripts/f5-deploy.sh, --check]}
        current:  {command: [/opt/manent/scripts/f5-current.sh]}
        rollback: {command: [/opt/manent/scripts/f5-rollback.sh]}

dbos:
  plugins: [script]                # sui worker che hanno gli script
```

| Chiave | Descrizione |
|---|---|
| `artifact_types` | Tipi di artefatto che il plugin rilascia. Obbligatoria: non c'è un default. |
| `work_dir` | Directory delle esecuzioni, creata con permessi `0700`. |
| `env` | Variabili fisse per ogni script. I nomi che iniziano con `MANENT_` e `TMPDIR` li imposta manent. |
| `scripts.<nome>.timeout` | Durata massima di un'esecuzione. Poi lo script e i suoi figli vengono fermati e lo step viene ripetuto. |
| `scripts.<nome>.per_target` | `false`: un'esecuzione per tutti i target del lotto. `true`: un'esecuzione per target, fino al [parallelismo](../guida/ondate.md#target-in-parallelo) del deploy insieme. |
| `scripts.<nome>.params` | Parametri accettati: `pattern` (espressione regolare che deve corrispondere a tutto il valore, obbligatoria), `required`, `default`. |
| `scripts.<nome>.secrets` | Nomi logici dei segreti passati allo script come file. Devono essere dichiarati dal descrittore o dalla configurazione di ambiente. |
| `scripts.<nome>.outputs` | Le sole chiavi che lo script può scrivere negli output, con il loro `pattern`. |
| `scripts.<nome>.run` | Il deploy: `command` (percorso assoluto e argomenti, eseguito senza shell) e `sha256` facoltativo del file. Obbligatorio. |
| `scripts.<nome>.dry_run` | Verifica senza modifiche, per i [dry-run](../guida/dry-run.md). Senza, un dry-run verifica solo i parametri. |
| `scripts.<nome>.current` | Stampa la release in esercizio: serve al rollback automatico con il locator `plugin` e alla [riconciliazione](../guida/governance.md#stato-reale). |
| `scripts.<nome>.rollback` | Riporta i target alla release precedente. |

Le chiavi sono verificate all'avvio del worker: una chiave sconosciuta, un percorso relativo o un pattern non valido lo fermano.

!!! warning "L'immagine e gli script"
    L'immagine `manent-server` è distroless: non ha una shell né interpreti. Esegui gli script su worker di un'immagine tua, con gli script e gli strumenti che usano, e dai al plugin una [coda propria](../operazioni/scalabilita.md#plugin-queues), come fa l'immagine `server-ansible` per Ansible.

## Configurazione di ambiente

```yaml title="envconfig/payments/shop/shop-vip/prod.yml"
plugin: script
targets:
  - {host: bigip1.example.com, labels: {region: eu-1}}
  - {host: bigip2.example.com, labels: {region: eu-1}}
params:
  script: f5-legacy
  partition: shop-prod
secrets:
  - {name: f5-password, ref: "vault:secret/data/payments/shop/f5#password"}
```

Il parametro `script` è obbligatorio. Ogni altro parametro deve essere dichiarato dallo script e rispettarne il pattern, altrimenti il deploy viene rifiutato prima di partire.

Perché il descrittore non possa scegliere lo script né i suoi parametri, non elencarli in [`descriptor_policy.params`](../operazioni/configurazione.md): così arrivano solo dalla configurazione di ambiente.

## Cosa riceve lo script

Il comando viene eseguito senza shell, con la directory dell'esecuzione come directory di lavoro e senza standard input. L'ambiente contiene solo `PATH`, `HOME`, `TZ` e `LANG` del worker, le `env` del catalogo e queste variabili. Le altre variabili del worker non vengono passate, perché possono contenere credenziali.

| Variabile | Contenuto |
|---|---|
| `MANENT_ACTION` | `deploy`, `dry-run`, `current` o `rollback`. |
| `MANENT_DRY_RUN` | `true` in un dry-run. |
| `MANENT_SCRIPT` | Nome dello script nel catalogo. |
| `MANENT_DEPLOYMENT_ID` | ID del deploy. Usalo come chiave di idempotenza. |
| `MANENT_TENANT`, `MANENT_APPLICATION`, `MANENT_ARTIFACT`, `MANENT_ENVIRONMENT` | La release. `MANENT_TENANT` è `untenanted` per le applicazioni di nessun tenant. |
| `MANENT_VERSION`, `MANENT_ARTIFACT_TYPE` | Versione e tipo dell'artefatto. |
| `MANENT_ARTIFACT_PATH`, `MANENT_ARTIFACT_SHA256` | L'artefatto scaricato, già verificato con lo sha256. Vuote per `current`. |
| `MANENT_TARGETS` | File JSON con i target dell'esecuzione: `host`, `port`, `user`, `labels`. |
| `MANENT_TARGET`, `MANENT_TARGET_PORT`, `MANENT_TARGET_USER`, `MANENT_TARGET_LABEL_<NOME>` | Con `per_target`: il target dell'esecuzione. |
| `MANENT_PARALLELISM` | Quanti target l'esecuzione può aggiornare insieme, per uno script senza `per_target` che li gestisce da sé. |
| `MANENT_PARAM_<NOME>` | I parametri, default compresi. Il nome è in maiuscolo con `_` al posto degli altri caratteri (`db.name` → `MANENT_PARAM_DB_NAME`). |
| `MANENT_SECRETS_DIR` | Directory con un file per segreto, con il nome del segreto. |
| `MANENT_OUTPUT` | File in cui lo script scrive gli output. |
| `MANENT_WORK_DIR`, `TMPDIR` | Directory dell'esecuzione e la sua `tmp`. |
| `MANENT_PREVIOUS_VERSION`, `MANENT_PREVIOUS_<CHIAVE>` | Per `rollback`: la release a cui tornare, con i dettagli stampati da `current`. |

La directory dell'esecuzione viene rimossa alla fine, segreti compresi. Una directory lasciata da un crash viene rimossa da un'esecuzione successiva dopo 24 ore. Quando lo script termina, i processi lasciati in background vengono fermati.

## Esito e output

| Exit code | Esito |
|---|---|
| `0` | Successo. |
| `75` | Errore temporaneo: lo step viene ripetuto (`max_retries` dello step `deploy`). |
| timeout, segnale | Errore temporaneo. |
| qualunque altro | Errore permanente: nessun retry, e parte il [rollback automatico](../guida/rollback.md) se la policy lo prevede. |

Lo script scrive gli output in `$MANENT_OUTPUT`, una riga `chiave=valore` ciascuno. Sono accettate solo le chiavi dichiarate in `outputs`, con un valore che rispetta il pattern e non contiene segreti: vengono registrate con il deploy e possono diventare [export](../guida/catene.md#export-import) per altri deploy. Con `per_target` gli output vanno nei dettagli di ciascun target e non sono esportati.

Standard output e standard error finiscono nel log del deploy riga per riga, con i valori dei segreti sostituiti da `***`, fino a 256 KiB per esecuzione.

!!! note "Idempotenza"
    Un retry o una recovery dopo un crash possono eseguire di nuovo lo script per lo stesso deploy, anche dopo che ha modificato i target. Lo script deve arrivare allo stesso risultato se eseguito due volte, per esempio controllando se la versione è già in esercizio.

## Rollback e release in esercizio

Il comando `current` scrive `version=<versione>` in `$MANENT_OUTPUT`, più gli output dichiarati come dettagli. Se non scrive nulla, non c'è una release in esercizio (primo deploy). Con `per_target` tutti i target devono avere la stessa release.

Il comando `rollback` riceve la release a cui tornare in `MANENT_PREVIOUS_VERSION`. Come per ogni plugin, il rollback automatico richiede anche il tipo di artefatto in [`rollback.artifact_types`](../guida/rollback.md#per-tipo-di-artefatto) con `allowed: true`. Un deploy che chiede il rollback con uno script senza `rollback` o `current` fallisce al checkpoint, prima di modificare qualcosa.

!!! note "Riconciliazione e coda del plugin"
    La riconciliazione gira sul worker che ne ha il lease, non sulla coda del plugin. Se quel worker non serve il plugin, non può eseguire `current`, e l'ambiente viene registrato come non leggibile. Senza una coda del plugin ogni worker esegue gli script, e la riconciliazione li legge.

## Esempio di script

```sh title="/opt/manent/scripts/f5-deploy.sh"
#!/bin/sh
set -eu
pass=$(cat "$MANENT_SECRETS_DIR/f5-password")
for host in $(jq -r '.[].host' "$MANENT_TARGETS"); do
  # Stessa versione già attiva: niente da fare (retry e recovery).
  if f5ctl --host "$host" --partition "$MANENT_PARAM_PARTITION" active-version | grep -qx "$MANENT_VERSION"; then
    continue
  fi
  F5_PASSWORD=$pass f5ctl --host "$host" --partition "$MANENT_PARAM_PARTITION" \
    upload "$MANENT_ARTIFACT_PATH" --version "$MANENT_VERSION" || exit 75
done
first=$(jq -r '.[0].host' "$MANENT_TARGETS")
echo "vip=$(f5ctl --host "$first" --partition "$MANENT_PARAM_PARTITION" vip)" >> "$MANENT_OUTPUT"
```
