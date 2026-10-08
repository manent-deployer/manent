# go-dart

Strumento di **deploy as code** basato su workflow durevoli [DBOS](https://github.com/dbos-inc/dbos-transact-golang).
Un descrittore di deploy (YAML da CLI, JSON da API) attiva una pipeline di step configurabili: segreti, artefatti, QA, deploy tramite plugin, test post deploy e attivazione in esercizio.
Se il processo si interrompe, il deploy riprende dall'ultimo step completato.

## Avvio rapido

```sh
make build
bin/go-dart -c configs/go-dart.yml validate -f examples/descriptor.yml   # mostra il piano
bin/go-dart -c configs/go-dart.yml deploy -f examples/descriptor.yml
bin/go-dart -c configs/go-dart.yml serve                                 # API su :8080
```

Di default DBOS usa SQLite (`sqlite:go-dart.db`); in produzione impostare `DBOS_SYSTEM_DATABASE_URL=postgres://...`.

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
