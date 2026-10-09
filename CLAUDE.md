# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

# Progetto manent

manent è uno strumento di **deploy as code**: un motore a workflow configurabili che esegue una pipeline di deploy a step.
Il nome viene dal latino *manere* ("rimanere, restare saldo") ed esprime l'immutabilità dell'artefatto, che resta così com'è stato testato, validato e portato in esercizio.

## Requisiti tecnici

- Linguaggio: Go >= 1.27
- Workflow, step, code (queueing), retry e scheduling: **DBOS** (`github.com/dbos-inc/dbos-transact-golang/dbos`). Seguire le linee guida in [dbos-skills.md](./dbos-skills.md).
- CLI: `github.com/jessevdk/go-flags` — accetta descrittori di deploy in **YAML**.
- Web server API: `github.com/gin-gonic/gin` — accetta descrittori di deploy in **JSON**.
- CLI e API devono condividere lo stesso core (stesso modello di descrittore, stesso motore di workflow); cambia solo il formato di input.

## Architettura

Flusso: CLI (`internal/cli`, YAML) o API (`internal/api`, JSON) → `descriptor` → `engine.Service` → workflow DBOS → step della pipeline → plugin di deploy.

- `internal/app` è l'unico punto di composizione: collega le implementazioni concrete alle interfacce. I nuovi plugin si registrano in `app.NewPlugins`, i nuovi step in `app.NewSteps`. Ci sono due modalità:
  - `app.NewWorker` (comandi `serve` e `worker`): esegue i deploy.
  - `app.NewClient` (tutti gli altri comandi CLI e `serve --no-worker`): usa `dbos.NewClient`, accoda e interroga ma **non esegue e non recupera** workflow. Un comando CLI non deve mai avviare un `dbos.Context`: con `Launch` recupererebbe i workflow in corso del proprio ExecutorID e li rieseguirebbe.
- `internal/engine` è diviso in due parti:
  - `Worker` (`worker.go`) possiede il `dbos.Context`, registra i workflow `manent.deploy` e `manent.scheduled-deploy` e la coda. Esegue in ordine i `PlannedStep`, ciascuno con `dbos.RunAsStep` (nome e `max_retries` dalla configurazione), e alla fine, sia in caso di successo sia di errore, esegue lo step `cleanup` (`pipeline.Cleaner`).
  - `Engine` (`engine.go`, `schedule.go`) implementa `Service` solo con API che accettano un `dbos.Client` (`dbos.Enqueue` per nome del workflow, `RetrieveWorkflow`, `ListWorkflows`, ...), quindi funziona sia sul contesto del worker sia su un client standalone.
  - Se il descrittore ha un `id`, questo diventa l'ID del workflow e fa da chiave di idempotenza. I nomi dei workflow non vanno cambiati: DBOS li usa per il recovery e per le schedule.
- Scalabilità orizzontale: più worker condividono lo stesso Postgres.
  - Ogni worker deve avere un `dbos.executor_id` univoco e stabile (default: hostname), perché al riavvio riprende solo i propri workflow in sospeso. Senza DBOS Conductor, un nodo che non torna con lo stesso ID lascia i suoi deploy in `PENDING`.
  - La coda è partizionata con chiave `<application>/<environment>`: `environment_concurrency` (default 1) vale su tutti i worker, `concurrency` per singolo worker, `global_concurrency` sul totale. I workflow accodati senza chiave di partizione non vengono mai prelevati: per questo gli scatti delle schedule usano la coda interna di DBOS.
  - `NewWorker` riprova l'inizializzazione, perché su un Postgres vuoto più worker avviati insieme vanno in race sulla migrazione dello schema DBOS.
- Configurazione di ambiente (`internal/envconfig`): plugin, policy, target, parametri e riferimenti ai segreti di ogni application/environment arrivano da un `Provider` (`consul` via API HTTP del KV, `file`, `none`), configurato in `envconfig`.
  - Il worker la legge nello step DBOS `config`, eseguito prima del piano (`Worker.run`), e passa a tutti gli step il descrittore **effettivo**. Così i deploy pianificati usano la configurazione valida al momento dell'esecuzione, e un deploy ripreso dopo un crash riusa quella già registrata.
  - Nel risultato lo step `config` riporta `source` e `revision` (`ModifyIndex` di Consul).
  - Regole di fusione (`envconfig.Merge`): la configurazione di ambiente è autorevole. Il descrittore può solo aggiungere ciò che non vi è definito, e i valori in conflitto sono un errore, mai una sovrascrittura. Per questo `deploy.targets` non è più obbligatorio nel descrittore: il controllo "almeno un target" avviene dopo la fusione.
  - La configurazione viene persistita da DBOS: deve contenere solo riferimenti ai segreti. Il token Consul si legge da `CONSUL_HTTP_TOKEN` o da `token_file`, mai dal file di configurazione.
- Pianificazione (sezione `schedule` del descrittore), sempre sulla stessa coda DBOS dei deploy, così vale lo stesso limite di concorrenza:
  - `schedule.at`: il deploy viene accodato subito con `dbos.WithDelay` e resta `DELAYED` fino all'orario; si annulla con `Cancel`.
  - `schedule.cron`: `dbos.ApplySchedules` crea o aggiorna la schedule (upsert per nome, default `<application>-<environment>`). Il descrittore e il piano sono salvati nel `Context` della schedule. A ogni tick il workflow `manent.scheduled-deploy` accoda un `manent.deploy` figlio con ID deterministico `<schedule>@<tick UTC>`: un tick non genera mai due deploy, e `List` filtra le esecuzioni di una schedule per prefisso dell'ID. Il workflow del tick non attende il figlio, per non occupare uno slot della coda.
  - Le espressioni cron a 5 campi vengono convertite nel formato a 6 campi (con i secondi) richiesto da DBOS (`Schedule.CronExpr`).
- `internal/pipeline`: contratto `Step` e `ConfigPlanner`. Il piano viene calcolato **prima** di avviare il workflow e gli viene passato come input. Regole: gli step obbligatori non sono disattivabili; quelli opzionali partono dal default di configurazione e il descrittore (`steps:`) può sovrascriverlo; l'ordine segue sempre `pipeline.steps` della configurazione.
- Gli step si scambiano dati solo tramite `pipeline.Output` (`map[string]string`), che DBOS persiste. **Gli output non devono mai contenere segreti**: lo step `secrets` si limita a verificare che i riferimenti siano risolvibili, mentre i plugin leggono i valori al momento dell'uso tramite `plugin.SecretResolver`.
- `pkg/plugin` è l'unico package pubblico: il contratto `Deployer` e il `Registry` di factory. I plugin dei team esterni dipendono solo da qui. `Deploy` deve essere idempotente, perché può essere rieseguito dai retry o da una recovery.
- `internal/steps`: le implementazioni standard degli step (secrets, artifacts, qa, deploy, post-test, activation). Lo step deploy sceglie il plugin tramite una `Policy` (`deploy.policy` del descrittore; la policy di default usa `deploy.plugin`, altrimenti il primo plugin attivo che supporta il tipo di artefatto, in ordine alfabetico).
- `plugins/remotecopy`: primo plugin. Copia l'artefatto via SSH/SFTP passando da un file `.part` e poi un rename atomico, verifica lo sha256 remoto e può eseguire un `post_command`. L'SSH reale è nascosto dietro le interfacce `Dialer` e `Session`.
- `internal/logging`: un `*slog.Logger` con un handler fan-out (log applicativo + `rfc5424.Message` inviati al SIEM via UDP o TCP). Lo stesso logger viene passato a DBOS, quindi anche i log di DBOS arrivano al SIEM. Ogni componente riceve il logger per iniezione.
- Gli artefatti vengono scaricati in `workdir/<deployment-id>` (`artifact.Store`). Lo step `deploy` chiama `Store.Ensure`: se il file manca sul worker corrente, lo riscarica e verifica lo sha256 registrato dallo step `artifacts`. Per questo non serve che la `workdir` stia su uno storage condiviso. L'`id` del descrittore e il nome della schedule sono validati con una regex, perché diventano nomi di directory.

## Convenzioni

- Ogni servizio va definito tramite interfacce, per facilitare mock e test.
- Ogni componente deve loggare sia a livello applicativo sia inviando i log al SIEM come `rfc5424.Message`.
- Commenti nel codice **in italiano**, descrittivi ma sintetici.

## Comandi

```sh
make build                       # bin/manent
make test                        # go test ./...
make lint                        # gofmt + go vet
go test ./internal/engine -run TestWorkflowFallito -v   # singolo test

bin/manent -c configs/manent.yml validate -f examples/descriptor.yml   # piano e deploy effettivo (legge Consul; --offline per evitarlo)
bin/manent -c configs/manent.yml deploy -f examples/descriptor.yml     # client: richiede un worker attivo; --no-wait, --timeout
bin/manent -c configs/manent.yml status <id>
bin/manent -c configs/manent.yml list [--schedule <nome>]
bin/manent -c configs/manent.yml cancel <id>
bin/manent -c configs/manent.yml schedule list|show|pause|resume|delete [nome]
bin/manent -c configs/manent.yml serve [--no-worker]                   # API su :8080 (+ worker)
bin/manent -c configs/manent.yml worker                                # solo worker
```

API: `POST /api/v1/deployments` (202 + id; 201 per una schedule cron), `GET /api/v1/deployments?schedule=&limit=`, `POST /api/v1/deployments/plan`, `GET /api/v1/deployments/:id`, `POST /api/v1/deployments/:id/cancel`, `GET /api/v1/schedules[/:name]`, `POST /api/v1/schedules/:name/pause|resume`, `DELETE /api/v1/schedules/:name`, `GET /healthz`.

I test dell'engine avviano un DBOS reale su SQLite in una directory temporanea, senza bisogno di Postgres. `scaling_test.go` avvia più worker sullo stesso file per verificare recovery per ExecutorID, serializzazione per ambiente e assenza di doppi deploy dalle schedule. Usano `SchedulePollInterval` basso: con il default di 30s una nuova schedule partirebbe solo dopo il successivo ricaricamento. Il `go.mod` fissa la toolchain 1.27.x: con una Go più vecchia viene scaricata automaticamente (`GOTOOLCHAIN=auto`).
