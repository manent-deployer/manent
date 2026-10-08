# go-dart

Strumento di **deploy as code** basato su workflow durevoli [DBOS](https://github.com/dbos-inc/dbos-transact-golang).
Un descrittore di deploy (YAML da CLI, JSON da API) attiva una pipeline di step configurabili: segreti, artefatti, QA, deploy tramite plugin, test post deploy e attivazione in esercizio.
Se il processo si interrompe, il deploy riprende dall'ultimo step completato.

## Avvio rapido

```sh
make build
bin/go-dart -c configs/go-dart.yml validate -f examples/descriptor.yml   # mostra il piano
bin/go-dart -c configs/go-dart.yml serve                                 # API su :8080 + worker
bin/go-dart -c configs/go-dart.yml deploy -f examples/descriptor.yml     # in un altro terminale
```

I deploy li esegue un **worker** (`serve` o `worker`). Gli altri comandi CLI (`deploy`, `status`, `list`, `cancel`, `schedule`) sono client: accodano e leggono lo stato dal database DBOS.
Di default DBOS usa SQLite (`sqlite:go-dart.db`); in produzione impostare `DBOS_SYSTEM_DATABASE_URL=postgres://...`.

## Scalabilità orizzontale

Più istanze condividono lo stesso Postgres e prelevano i deploy dalla stessa coda:

```sh
go-dart serve --no-worker       # N istanze API dietro un load balancer
go-dart worker                  # M worker, ciascuno con un executor_id diverso
```

- `dbos.executor_id` (default: hostname) deve essere **univoco e stabile**: un worker che si riavvia riprende i deploy interrotti con il proprio ID, dallo step in cui si erano fermati. In Kubernetes usare uno StatefulSet.
- `dbos.environment_concurrency` (default 1) impedisce deploy contemporanei della stessa application/environment su tutti i worker. `concurrency` limita i deploy per worker, `global_concurrency` il totale.
- La `workdir` resta locale: se un deploy riprende su un worker che non ha l'artefatto, questo viene riscaricato e ne viene verificato lo sha256.

## API

| Metodo | Percorso | Descrizione |
|---|---|---|
| `POST` | `/api/v1/deployments` | accoda un deploy (body: descrittore JSON), risponde `202` con l'`id`; con `schedule.cron` risponde `201` |
| `GET` | `/api/v1/deployments?schedule=&limit=` | elenca i deploy più recenti, anche solo quelli di una schedule |
| `POST` | `/api/v1/deployments/plan` | restituisce gli step che verrebbero eseguiti |
| `GET` | `/api/v1/deployments/{id}` | stato e risultato del deploy |
| `POST` | `/api/v1/deployments/{id}/cancel` | annulla un deploy in attesa o in esecuzione |
| `GET` | `/api/v1/schedules` · `/api/v1/schedules/{nome}` | schedule ricorrenti |
| `POST` | `/api/v1/schedules/{nome}/pause` · `/resume` | sospende o riattiva una schedule |
| `DELETE` | `/api/v1/schedules/{nome}` | elimina una schedule |
| `GET` | `/healthz` | health check |

## Deploy pianificati

Tutti i deploy, anche quelli pianificati, passano dalla coda DBOS `deployments` e ne rispettano la concorrenza.

```yaml
# Deploy singolo a un orario: resta DELAYED fino ad allora, annullabile con `go-dart cancel <id>`
schedule:
  at: 2026-10-10T02:00:00+02:00
```

```yaml
# Deploy ricorrente: ri-applicare il descrittore aggiorna la schedule con lo stesso nome
schedule:
  cron: "0 2 * * *"        # 5 o 6 campi (con secondi), oppure @daily, @hourly, ...
  timezone: Europe/Rome
  name: myapp-nightly      # default: <application>-<environment>
```

Ogni esecuzione ricorrente riceve l'ID `<nome>@<orario UTC>`. Per elencarle: `go-dart list --schedule myapp-nightly`. Per gestire le schedule: `go-dart schedule list|show|pause|resume|delete`.

## Pipeline

Gli step e il loro ordine si definiscono in `configs/go-dart.yml` (`pipeline.steps`).
Gli step `mandatory` vengono sempre eseguiti. Quelli `optional` hanno un default in configurazione che il descrittore può sovrascrivere:

```yaml
steps:
  qa: false
  post-test: true
```

## Scrivere un plugin di deploy

Un plugin implementa `plugin.Deployer` del package `github.com/joeg-ita/go-dart/pkg/plugin`:

```go
type Deployer interface {
	Name() string
	Supports(artifactType string) bool
	Validate(req *plugin.Request) error
	Deploy(ctx context.Context, req *plugin.Request) (*plugin.Result, error)
}
```

Va poi registrato con una `plugin.Factory` in `internal/app` (`NewPlugins`) e attivato nella sezione `plugins:` della configurazione.
`Deploy` deve essere idempotente: DBOS può rieseguirlo in caso di retry o recovery.
I segreti si leggono con `req.Secrets.Secret(ctx, nome)` solo quando servono e non vanno mai inseriti nel `Result`.

Il plugin di riferimento è [`plugins/remotecopy`](plugins/remotecopy): copia l'artefatto via SSH/SFTP, verifica lo sha256 remoto ed esegue un eventuale `post_command`.

## Licenza

GPL-3.0, vedi [LICENSE](LICENSE).
