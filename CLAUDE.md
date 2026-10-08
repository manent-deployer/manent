# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

# Progetto go-dart

go-dart è uno strumento di **deploy as code**: un motore a workflow configurabili che esegue una pipeline di deploy a step.

## Requisiti tecnici

- Linguaggio: Go >= 1.27
- Workflow, step, code (queueing), retry e scheduling: **DBOS** (`github.com/dbos-inc/dbos-transact-golang/dbos`). Seguire le linee guida in [dbos-skills.md](./dbos-skills.md).
- CLI: `github.com/jessevdk/go-flags` — accetta descrittori di deploy in **YAML**.
- Web server API: `github.com/gin-gonic/gin` — accetta descrittori di deploy in **JSON**.
- CLI e API devono condividere lo stesso core (stesso modello di descrittore, stesso motore di workflow); cambia solo il formato di input.

## Architettura

Flusso: CLI (`internal/cli`, YAML) o API (`internal/api`, JSON) → `descriptor` → `engine.Service` → workflow DBOS → step della pipeline → plugin di deploy.

- `internal/app` è l'unico punto di composizione: collega le implementazioni concrete alle interfacce. I nuovi plugin si registrano in `app.NewPlugins`, i nuovi step in `app.NewSteps`.
- `internal/engine`: un solo workflow DBOS (`go-dart.deploy`) che esegue in ordine i `PlannedStep`, ciascuno con `dbos.RunAsStep` (nome dello step e `max_retries` presi dalla configurazione). I deploy vengono accodati sulla coda DBOS (`dbos.queue`). Se il descrittore ha un `id`, questo diventa l'ID del workflow e fa da chiave di idempotenza. Il nome del workflow non va cambiato: DBOS lo usa per recuperare i workflow interrotti.
- Pianificazione (sezione `schedule` del descrittore), sempre sulla stessa coda DBOS dei deploy, così vale lo stesso limite di concorrenza:
  - `schedule.at`: il deploy viene accodato subito con `dbos.WithDelay` e resta `DELAYED` fino all'orario; si annulla con `Cancel`.
  - `schedule.cron`: `dbos.ApplySchedules` crea o aggiorna la schedule (upsert per nome, default `<application>-<environment>`). Il descrittore e il piano sono salvati nel `Context` della schedule. A ogni tick il workflow `go-dart.scheduled-deploy` accoda un `go-dart.deploy` figlio con ID deterministico `<schedule>@<tick UTC>`: un tick non genera mai due deploy, e `List` filtra le esecuzioni di una schedule per prefisso dell'ID. Il workflow del tick non attende il figlio, per non occupare uno slot della coda.
  - Le espressioni cron a 5 campi vengono convertite nel formato a 6 campi (con i secondi) richiesto da DBOS (`Schedule.CronExpr`).
- `internal/pipeline`: contratto `Step` e `ConfigPlanner`. Il piano viene calcolato **prima** di avviare il workflow e gli viene passato come input. Regole: gli step obbligatori non sono disattivabili; quelli opzionali partono dal default di configurazione e il descrittore (`steps:`) può sovrascriverlo; l'ordine segue sempre `pipeline.steps` della configurazione.
- Gli step si scambiano dati solo tramite `pipeline.Output` (`map[string]string`), che DBOS persiste. **Gli output non devono mai contenere segreti**: lo step `secrets` si limita a verificare che i riferimenti siano risolvibili, mentre i plugin leggono i valori al momento dell'uso tramite `plugin.SecretResolver`.
- `pkg/plugin` è l'unico package pubblico: il contratto `Deployer` e il `Registry` di factory. I plugin dei team esterni dipendono solo da qui. `Deploy` deve essere idempotente, perché può essere rieseguito dai retry o da una recovery.
- `internal/steps`: le implementazioni standard degli step (secrets, artifacts, qa, deploy, post-test, activation). Lo step deploy sceglie il plugin tramite una `Policy` (`deploy.policy` del descrittore; la policy di default usa `deploy.plugin`, altrimenti il primo plugin attivo che supporta il tipo di artefatto, in ordine alfabetico).
- `plugins/remotecopy`: primo plugin. Copia l'artefatto via SSH/SFTP passando da un file `.part` e poi un rename atomico, verifica lo sha256 remoto e può eseguire un `post_command`. L'SSH reale è nascosto dietro le interfacce `Dialer` e `Session`.
- `internal/logging`: un `*slog.Logger` con un handler fan-out (log applicativo + `rfc5424.Message` inviati al SIEM via UDP o TCP). Lo stesso logger viene passato a DBOS, quindi anche i log di DBOS arrivano al SIEM. Ogni componente riceve il logger per iniezione.
- Gli artefatti vengono scaricati in `workdir/<deployment-id>`: con più executor DBOS questa directory deve stare su storage condiviso.

## Convenzioni

- Ogni servizio va definito tramite interfacce, per facilitare mock e test.
- Ogni componente deve loggare sia a livello applicativo sia inviando i log al SIEM come `rfc5424.Message`.
- Commenti nel codice **in italiano**, descrittivi ma sintetici.

## Comandi

```sh
make build                       # bin/go-dart
make test                        # go test ./...
make lint                        # gofmt + go vet
go test ./internal/engine -run TestWorkflowFallito -v   # singolo test

bin/go-dart -c configs/go-dart.yml validate -f examples/descriptor.yml   # piano, senza DBOS
bin/go-dart -c configs/go-dart.yml deploy -f examples/descriptor.yml     # --no-wait per non attendere
bin/go-dart -c configs/go-dart.yml status <id>
bin/go-dart -c configs/go-dart.yml list [--schedule <nome>]
bin/go-dart -c configs/go-dart.yml cancel <id>
bin/go-dart -c configs/go-dart.yml schedule list|show|pause|resume|delete [nome]
bin/go-dart -c configs/go-dart.yml serve                                 # API su :8080
```

API: `POST /api/v1/deployments` (202 + id; 201 per una schedule cron), `GET /api/v1/deployments?schedule=&limit=`, `POST /api/v1/deployments/plan`, `GET /api/v1/deployments/:id`, `POST /api/v1/deployments/:id/cancel`, `GET /api/v1/schedules[/:name]`, `POST /api/v1/schedules/:name/pause|resume`, `DELETE /api/v1/schedules/:name`, `GET /healthz`.

I test dell'engine avviano un DBOS reale su SQLite in una directory temporanea, senza bisogno di Postgres. Usano `SchedulePollInterval` basso: con il default di 30s una nuova schedule partirebbe solo dopo il successivo ricaricamento. Il `go.mod` fissa la toolchain 1.27.x: con una Go più vecchia viene scaricata automaticamente (`GOTOOLCHAIN=auto`).
