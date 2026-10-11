# manent

> Il nome **manent** viene dal latino *manere*, "rimanere, restare saldo": vuole esprimere l'immutabilità dell'artefatto, che resta così com'è stato testato, validato e portato in esercizio.

Strumento di **deploy as code** basato su workflow durevoli [DBOS](https://github.com/dbos-inc/dbos-transact-golang).
Un descrittore di deploy (YAML da CLI, JSON da API) attiva una pipeline di step configurabili: segreti, artefatti, QA, deploy tramite plugin, test post deploy e attivazione in esercizio.
Se il processo si interrompe, il deploy riprende dall'ultimo step completato.

La documentazione completa è in [`docs/`](docs): `make docs` genera il sito statico in `docs/site`.

## Avvio rapido

```sh
make build
bin/manent-server -c configs/manent.yml validate -f examples/descriptor.yml   # mostra il piano
bin/manent-server -c configs/manent.yml serve                                 # API su :8080 + worker
bin/manent deploy -f examples/descriptor.yml     # in un altro terminale
```

manent ha due binari:

- **`manent`**, il client per sviluppatori e CI: legge descrittori YAML e parla solo con l'API (`--server`, `MANENT_SERVER` o `~/.config/manent/client.yml`; default `http://127.0.0.1:8080`; token in `MANENT_TOKEN` o in un file). Non contiene il motore, i plugin né i driver di database, quindi valgono sempre i permessi e l'audit del server.
- **`manent-server`**: `serve` (API e worker), `api`, `worker`, le operazioni di piattaforma `admin` e `audit verify` e `validate` di una configurazione in locale.

I deploy li esegue un **worker** (`serve` o `worker`).
Di default DBOS usa SQLite (`sqlite:manent.db`); in produzione impostare `DBOS_SYSTEM_DATABASE_URL=postgres://...`.

## Scalabilità orizzontale

Più istanze condividono lo stesso Postgres e prelevano i deploy dalla stessa coda:

```sh
manent-server api              # N istanze API dietro un load balancer
manent-server worker                  # M worker, ciascuno con un executor_id diverso
```

- `dbos.executor_id` (default: hostname) deve essere **univoco e stabile**: un worker che si riavvia riprende i deploy interrotti con il proprio ID, dallo step in cui si erano fermati. In Kubernetes usare uno StatefulSet.
- `dbos.environment_concurrency` (default 1) impedisce deploy contemporanei della stessa application/environment su tutti i worker. `concurrency` limita i deploy per worker, `global_concurrency` il totale.
- La `workdir` resta locale: se un deploy riprende su un worker che non ha l'artefatto, questo viene riscaricato e ne viene verificato lo sha256.

## Rilasci a ondate

Con target in più region e AZ, la configurazione di ambiente può definire una strategia `rollout`: i target portano le etichette `region` e `az`, le ondate li selezionano (`select: "region=eu-1, canary!=true"`) e li dividono in passi per AZ o region, con `maxUnavailable` dentro il passo, tempo di osservazione (`bake`) e health gate (sonde HTTP, query Prometheus, allarmi Alertmanager) tra uno stadio e il successivo. Un passo fallito ferma il rollout; `onFailure` decide se riportare indietro lo stadio (`rollback-wave`) o tutto (`rollback-all`). `approvalsBetween` mette in pausa per approvazione, i freeze con `regions` bloccano solo quelle region. Con `dbos.region_routing` ogni passo gira come workflow figlio sui worker della sua region (`dbos.region`). `manent status` mostra stadi, passi e gate; l'inventario segnala gli ambienti disallineati (`drift`). Dettagli in [docs/content/guida/ondate.md](docs/content/guida/ondate.md).

## Operatività

- **Worker persi:** ogni worker si registra con un heartbeat (`manent-server admin executors`). I deploy in sospeso di un worker che non torna si riprendono su un altro worker con `manent-server admin recover --executor <id>`, o da soli con `recovery.auto_recover_after`. Rivendicazione e riavvio del nodo si escludono a vicenda, così nessun deploy viene ripreso due volte.
- **Aggiornamenti:** all'arresto il worker smette di prelevare deploy e attende quelli in corso (`dbos.drain_timeout`). `app.version` è la versione dei workflow, non del binario; dopo un cambio incompatibile i workflow rimasti si migrano con `manent-server admin recover --from-version <v>`.
- **Retention:** `retention.max_age` elimina lo storico concluso conservando le ultime `keep_releases` release di ogni ambiente (`manent-server admin prune`).
- **Osservabilità:** metriche Prometheus su `/metrics` (worker: `observability.listen`), sonde `/healthz` e `/readyz`, tracing OpenTelemetry dall'API agli step, log di ogni deploy (`manent logs -f <id>`), timeline (`manent steps <id>`) e metriche DORA (`manent dora`).
- **Packaging:** `Dockerfile` (distroless, non root), chart Helm in `deploy/helm/manent` (worker in StatefulSet), unità systemd in `deploy/systemd`, rilasci con GoReleaser (checksum firmati con cosign, SBOM), CI in `.github/workflows`, [CHANGELOG](CHANGELOG.md).
- **Test:** `make test-postgres` (gli stessi test su Postgres), `make test-e2e` (SSH reale in Docker), `make test-load` (carico e caos).

Dettagli in [installazione](docs/content/operazioni/installazione.md), [recovery e retention](docs/content/operazioni/affidabilita.md), [aggiornamenti](docs/content/operazioni/aggiornamenti.md) e [osservabilità](docs/content/operazioni/osservabilita.md).

## API

| Metodo | Percorso | Descrizione |
|---|---|---|
| `POST` | `/api/v1/deployments` | accoda un deploy (body: descrittore JSON), risponde `202` con l'`id`; con `schedule.cron` risponde `201`; con `?dry_run=true` accoda un dry-run |
| `GET` | `/api/v1/deployments?schedule=&application=&environment=&state=&limit=` | elenca i deploy più recenti, con filtri |
| `POST` | `/api/v1/deployments/plan` | restituisce gli step che verrebbero eseguiti |
| `GET` | `/api/v1/deployments/{id}` | stato e risultato del deploy |
| `GET` | `/api/v1/deployments/{id}/steps` · `/logs?offset=` | timeline degli step e log del deploy |
| `POST` | `/api/v1/deployments/{id}/cancel` | annulla un deploy in attesa o in esecuzione |
| `POST` | `/api/v1/deployments/{id}/approve` · `/reject` | approva o respinge un deploy in attesa di approvazione |
| `POST` | `/api/v1/promotions` | promuove un artefatto da un ambiente a un altro |
| `POST` | `/api/v1/rollbacks` | torna a una versione precedente |
| `GET` | `/api/v1/inventory` | release in esercizio per applicazione e ambiente |
| `GET` | `/api/v1/freezes` | finestre di freeze attive e future |
| `GET` | `/api/v1/metrics/dora?application=&environment=&since=` | metriche DORA |
| `GET` | `/api/v1/schedules` · `/api/v1/schedules/{nome}` | schedule ricorrenti |
| `POST` | `/api/v1/schedules/{nome}/pause` · `/resume` | sospende o riattiva una schedule |
| `DELETE` | `/api/v1/schedules/{nome}` | elimina una schedule |
| `GET` | `/healthz` · `/readyz` · `/metrics` | sonde e metriche Prometheus, senza autenticazione |

Con `auth.mode: enabled` le rotte `/api/v1` richiedono `Authorization: Bearer <token>` (OIDC o token statico): `401` senza token valido, `403` se i permessi non consentono l'azione.

## Sicurezza

La configurazione di default è per lo sviluppo: `serve` e `worker` segnalano all'avvio ogni impostazione non adatta alla produzione. La checklist completa è in [docs/content/operazioni/sicurezza.md](docs/content/operazioni/sicurezza.md).

**Autenticazione** (`auth`):

- **utenti:** token JWT di un identity provider OIDC (Keycloak, Entra ID, Okta), verificati con issuer, audience e chiavi JWKS;
- **pipeline CI:** token statici, di cui in configurazione c'è solo lo sha256, con scadenza opzionale; in `auth.tokens_file` si aggiungono e revocano senza riavvio;
- **CLI:** il client `manent` usa un token come qualsiasi chiamata all'API; solo `manent-server admin`, riservato agli operatori della piattaforma, lavora sul database e registra nell'audit l'utente del sistema operativo (`cli:<utente>`).

**Permessi** (`rbac`). I ruoli si assegnano a utenti o gruppi per applicazione e ambiente, con pattern `*`:

- ruoli predefiniti: `viewer`, `tester` (+ dry-run), `approver` (+ approvazioni), `deployer` (+ deploy, annullamento e rollback manuale), `operator` (+ schedule), `admin` (anche deroghe al freeze);
- valgono per API e CLI, perché li applica il servizio comune;
- gli elenchi mostrano solo ciò che si può leggere, fino al limite richiesto.

```yaml
auth:
  mode: enabled
  oidc: {issuer: https://sso.example.com/realms/corp, audience: manent}
  tokens: [{name: ci-pipeline, sha256: "<sha256 del token>", groups: [ci]}]
rbac:
  bindings:
    - {groups: [team-a], role: deployer, applications: ["team-a-*"], environments: [dev, test]}
    - {groups: [release-managers], role: operator, applications: ["*"], environments: [prod]}
```

**Audit.** Si registrano:

- richieste, promozioni, rollback manuali, annullamenti e operazioni sulle schedule;
- approvazioni, rifiuti e deroghe al freeze, con il commento;
- gli accessi negati;
- l'esito finale di ogni deploy e di ogni approvazione.

Le voci vanno al log e al SIEM e, con `audit.file`, a un file JSON Lines con catena di hash (HMAC con `hmac_key_ref`) e rotazione (`max_size_mb`), verificabile con `manent-server audit verify`. Chi ha richiesto il deploy è salvato nel workflow DBOS e compare in stato, elenchi e callback (`requested_by`).

**Policy sui descrittori** (`descriptor_policy`). Limita ciò che un descrittore può aggiungere alla configurazione di ambiente:

- parametri per plugin (ad esempio mai `post_command`);
- pattern dei riferimenti ai segreti (es. `vault:secret/data/{application}/*`);
- target.

Una violazione viene rifiutata all'accodamento (`422`) e, se sfugge (es. Consul non raggiungibile in quel momento), fa fallire il deploy nello step `config`, senza retry.

**Segreti da Vault** (`secrets.vault`):

- riferimenti `vault:<percorso>#<campo>`, KV v1 e v2;
- login con token, AppRole o Kubernetes.

**Sorgenti degli artefatti** (`artifacts`):

- credenziali per prefisso dell'URL (basic, bearer, header, lette dal secret provider) e CA dedicate;
- con `allow_unlisted: false` solo le sorgenti elencate, verificate all'accodamento e al download.

## Deploy pianificati

Tutti i deploy, anche quelli pianificati, passano dalla coda DBOS `deployments` e ne rispettano la concorrenza.

```yaml
# Deploy singolo a un orario: resta DELAYED fino ad allora, annullabile con `manent cancel <id>`
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

Ogni esecuzione ricorrente riceve l'ID `<nome>@<orario UTC>`. Per elencarle: `manent list --schedule myapp-nightly`. Per gestire le schedule: `manent schedule list|show|pause|resume|delete`.

## Configurazione di ambiente (Consul)

Plugin, target, parametri del plugin e riferimenti ai segreti di ogni ambiente possono essere gestiti in un sistema di configurazione invece che nel descrittore. Il descrittore indica solo applicazione, ambiente e artefatto ([esempio](examples/descriptor-envconfig.yml)).

```yaml
# configs/manent.yml
envconfig:
  provider: consul                          # none | consul | file
  required: true
  consul:
    address: http://consul:8500             # default: CONSUL_HTTP_ADDR
    key: "manent/{application}/{environment}"
    # token ACL: CONSUL_HTTP_TOKEN oppure token_file
```

Il valore della chiave Consul è YAML o JSON ([esempio](examples/envconfig/myapp/test.yml)):

```sh
consul kv put manent/myapp/test @examples/envconfig/myapp/test.yml
manent-server -c configs/manent.yml validate -f examples/descriptor-envconfig.yml   # mostra il deploy effettivo
```

- **Quando viene letta:** all'inizio di ogni deploy, quindi un deploy pianificato usa la configurazione valida al momento dell'esecuzione. Una volta letta resta la stessa anche se il deploy viene ripreso dopo un crash. Il risultato riporta la chiave e il `ModifyIndex` usati.
- **Chi prevale:** la configurazione di ambiente. Il descrittore può aggiungere parametri e segreti non definiti, ma un valore in conflitto fa fallire il deploy.
- **Segreti:** la configurazione deve contenere solo **riferimenti** ai segreti (es. `env:DEPLOY_SSH_KEY`, `vault:secret/data/myapp/prod#ssh_key`), perché viene salvata nello storico del deploy.

## Dry-run

Il dry-run esegue la pipeline su un worker, come un deploy reale, ma senza modificare gli ambienti:

```sh
manent deploy -f examples/descriptor.yml --dry-run
curl -X POST 'localhost:8080/api/v1/deployments?dry_run=true' -d @examples/descriptor.json
```

In alternativa si può usare `dryRun: true` nel descrittore. Rispetto a `validate`, che calcola il piano in locale, il dry-run legge la configurazione di ambiente, verifica i segreti, scarica l'artefatto ed esegue la QA. Poi chiede al plugin di controllare i target:

- in dry-run uno step viene eseguito solo se implementa `pipeline.DryRunner`, altrimenti viene saltato e riportato come `"dry_run": "skipped"`. `secrets`, `artifacts` e `qa` girano normalmente, `post-test` e `activation` vengono saltati;
- lo step `deploy` seleziona e valida il plugin come in un deploy reale, poi chiama `DryRun` se il plugin implementa `plugin.DryRunner`. In caso contrario si ferma alla validazione (`"dry_run": "validated"`);
- `remote-copy` si connette a ogni target, verifica che `dest_dir` sia scrivibile (o creabile) e che `sha256sum` sia disponibile. Non copia nulla e non esegue `post_command`;
- con un `id` nel descrittore, il workflow del dry-run usa l'ID `<id>.dry-run`, così il deploy reale con lo stesso `id` resta eseguibile;
- il dry-run usa la stessa coda dei deploy e quindi rispetta `environment_concurrency`. Non è ammesso insieme a `schedule`.

## Callback

Nel descrittore si possono indicare fino a 5 endpoint che ricevono in `POST` (JSON) l'esito degli step intermedi e quello finale:

```yaml
secrets:
  - name: callback-key
    ref: env:MANENT_CALLBACK_KEY
callbacks:
  - url: https://ci.example.com/hooks/manent
    events: [step, deployment]      # opzionale, default: tutti
    auth:
      type: hmac                    # bearer | basic (+ username) | header (+ header) | hmac
      secret: callback-key          # nome di un segreto di secrets, mai il valore
```

Eventi, nell'ordine:

- `deployment` con stato `started`;
- `step` per `config` e per ogni step del piano, con stato `succeeded`, `failed` o, in dry-run, `skipped`. L'evento riporta l'`output` dello step o l'`error`;
- `step` per i checkpoint e le compensazioni del rollback (`checkpoint:deploy`, `rollback:deploy`, ...), se attivo;
- `deployment` finale con stato `succeeded` o `failed`, inviato dopo la pulizia. Se è stato eseguito il rollback, il campo `rollback` ne riporta l'esito (`completed`, `skipped`, `failed`).

```json
{"id": "myapp-1.2.3-test:4", "event": "step", "sequence": 4, "deployment_id": "myapp-1.2.3-test",
 "application": "myapp", "environment": "test", "step": "artifacts", "status": "succeeded",
 "output": {"sha256": "...", "size": "1024", "local_path": "..."}, "timestamp": "2026-10-09T10:00:00Z"}
```

Comportamento:

- **consegna:** ogni invio è uno step DBOS, ritentato fino a `callbacks.max_retries`. Un callback non consegnato viene registrato a log ma non fa fallire il deploy. Dopo una recovery un evento può arrivare due volte: il destinatario lo deduplica su `id` (anche nell'header `X-Manent-Delivery`);
- **risposta:** solo i codici `2xx` contano come consegna. I redirect non vengono seguiti;
- **firma `hmac`:** la richiesta porta `X-Manent-Timestamp` e `X-Manent-Signature: sha256=<hex>`, cioè l'HMAC-SHA256 di `<timestamp>.<body>` calcolato con il segreto;
- **limiti sugli URL:** di default sono ammessi solo URL `https`, con `callbacks.allow_http` e `callbacks.allowed_hosts` in configurazione. Gli URL fuori policy vengono rifiutati già all'accodamento (`422`);
- **deploy annullati:** per un deploy annullato con `cancel` non viene inviato l'evento finale.

## Rollback

Se un deploy fallisce dopo aver modificato i target, manent può riportare l'ambiente alla release che era in esercizio prima del deploy, nello stesso workflow DBOS.

**Policy.** In configurazione si stabilisce per quali tipi di artefatto il rollback è possibile. I tipi non elencati non lo ammettono, e le migration di database sono escluse di default:

```yaml
# configs/manent.yml
rollback:
  locator: plugin                 # come individuare la release a cui tornare
  artifact_types:
    application:
      allowed: true
      enabled: true               # default per i deploy del tipo
      on: [deploy, post-test, activation]   # step il cui fallimento avvia il rollback
    migration:
      allowed: false
```

Descrittore e configurazione di ambiente possono attivarlo, disattivarlo o restringere `on` entro `allowed`, con le solite regole di fusione: l'ambiente è autorevole, quindi la produzione può imporlo. Un rollback non ammesso viene rifiutato all'accodamento (`422`) e da `validate`.

```yaml
rollback:
  enabled: true
  on: [deploy, post-test]
```

**Funzionamento:**

1. **Decisione.** La policy viene applicata nello step `config`, dopo la fusione con la configurazione di ambiente, e resta registrata con il deploy.
2. **Checkpoint.** Prima di `deploy` lo step `checkpoint:deploy` verifica che il plugin supporti il rollback e chiede al **locator** la release in esercizio. Essendo uno step DBOS separato, i retry del deploy non lo ripetono.
3. **Compensazione.** Se fallisce uno step elencato in `on`, il worker esegue `rollback:<step>` per gli step già avviati che sanno annullarsi (`deploy`, `activation`), in ordine inverso, compreso quello fallito.
4. **Esito.** Il deploy resta `ERROR` e l'errore termina con `rollback completato`, `rollback: nessuna release da ripristinare` (primo deploy) oppure `rollback fallito: ...`. Una compensazione fallita ferma il rollback e viene loggata in `ERROR`, anche al SIEM.

**Locator.** Il locator individua la release precedente (`plugin.ReleaseLocator`):

- `plugin` (default) la legge dai target tramite `plugin.ReleaseInspector`;
- altre strategie, ad esempio un sistema aziendale che registra le release in esercizio, si aggiungono in `app.NewLocators`.

**Plugin.** Il plugin implementa `plugin.Rollbacker`. `remote-copy` lo supporta con `layout: releases`:

- ogni versione va in `<dest_dir>/releases/<versione>`, con il symlink `current` sulla release attiva e scambio atomico del link;
- il rollback riporta `current` alla release precedente ed esegue `rollback_command` (default `post_command`);
- `keep_releases` (default 5) limita le release conservate;
- `prepare_command` viene eseguito prima dello switch.

**Altri casi.**

- **Dry-run:** esegue i checkpoint ma mai le compensazioni.
- **Deploy annullato:** con `cancel` non viene compensato.
- **Rollback manuale:** `manent rollback -a <app> -e <ambiente> [--to <versione>]` rilascia di nuovo una versione precedente (vedi [Governance](#governance-dei-rilasci)).

Dettagli nella [documentazione](docs/content/guida/rollback.md).

## Governance dei rilasci

```yaml
# configs/manent.yml
governance:
  rules:                                  # vale la prima che corrisponde
    - name: produzione
      applications: ["*"]
      environments: [prod]
      requires: [preprod]                 # stesso artefatto già rilasciato in preprod
      approvals: {required: 1, timeout: 72h}
  freezes:
    - {name: weekend, applications: ["*"], environments: [prod], cron: "0 18 * * FRI", duration: 62h, timezone: Europe/Rome}
```

```sh
manent inventory -e prod                                     # release in esercizio, con sha256 e richiedente
manent promote -a myapp --from preprod --to prod             # stesso artefatto, target dell'ambiente di destinazione
manent approve <id> -m "CHG-1234"                            # ruolo approver; chi ha richiesto il deploy non può approvarlo
manent rollback -a myapp -e prod [--to 1.3.2]                # torna a una versione precedente (non per le migration)
manent freezes -e prod                                       # finestre di freeze attive e future
```

- **Inventario:** ricostruito dallo storico dei deploy riusciti; segnala i deploy falliti successivi.
- **Promozione:** con `requires` un deploy di un artefatto mai rilasciato negli ambienti indicati è rifiutato (`422`). `promote` riusa il descrittore di origine e ne fissa lo `sha256`, ma non i target.
- **Approvazioni:** il deploy resta `AWAITING_APPROVAL` su una coda separata, che non occupa i worker, e parte con lo stesso ID dopo le approvazioni. Può essere respinto, scadere o essere annullato.
- **Freeze:** blocca i deploy all'accodamento e all'avvio sul worker; `freezeOverride: "<motivo>"` nel descrittore deroga, con il permesso `deployment.freeze-override`.
- **Dry-run:** non soggetti alle regole.

Dettagli nella [documentazione](docs/content/guida/governance.md).

## Pipeline

Gli step e il loro ordine si definiscono in `configs/manent.yml` (`pipeline.steps`).
Gli step `mandatory` vengono sempre eseguiti. Quelli `optional` hanno un default in configurazione che il descrittore può sovrascrivere:

```yaml
steps:
  qa: false
  post-test: true
```

## Scrivere un plugin di deploy

Un plugin implementa `plugin.Deployer` del package `github.com/joeg-ita/manent/pkg/plugin`:

```go
type Deployer interface {
	Name() string
	Supports(artifactType string) bool
	Validate(req *plugin.Request) error
	Deploy(ctx context.Context, req *plugin.Request) (*plugin.Result, error)
}
```

Per il dry-run il plugin può implementare anche `plugin.DryRunner` (`DryRun(ctx, req) (*plugin.Result, error)`): deve solo verificare i target, senza modificarli, e riportare `plugin.StatusReady` per quelli pronti.

Per il rollback implementa `plugin.Rollbacker` (`Rollback(ctx, req, previous plugin.Release)`, idempotente e applicato a tutti i target) e, per il locator `plugin`, `plugin.ReleaseInspector` (`CurrentRelease(ctx, req)`, senza modifiche ai target).

Va poi registrato con una `plugin.Factory` in `internal/app` (`NewPlugins`) e attivato nella sezione `plugins:` della configurazione.
`Deploy` deve essere idempotente: DBOS può rieseguirlo in caso di retry o recovery.
I segreti si leggono con `req.Secrets.Secret(ctx, nome)` solo quando servono e non vanno mai inseriti nel `Result`.

Il plugin di riferimento è [`plugins/remotecopy`](plugins/remotecopy): copia l'artefatto via SSH/SFTP, verifica lo sha256 remoto ed esegue un eventuale `post_command`; con `layout: releases` supporta il rollback.

### Plugin esterni

Un plugin può anche essere distribuito come binario separato, senza ricompilare manent. Usa la stessa `plugin.Factory`:

```go
func main() { external.Serve(mioplugin.Factory) }   // github.com/joeg-ita/manent/pkg/plugin/external
```

```yaml
plugins:
  mio-plugin:
    external:
      command: /opt/manent/plugins/mio-plugin
      sha256: "<sha256 del binario>"
    artifact_types: [application]
```

manent avvia il processo al primo deploy e comunica con lui via gRPC ([hashicorp/go-plugin](https://github.com/hashicorp/go-plugin)). Il plugin non eredita le variabili d'ambiente del worker e riceve i segreti solo tramite `req.Secrets`. Dettagli nella [documentazione](docs/content/plugins/esterni.md).

## Licenza

Apache-2.0, vedi [LICENSE](LICENSE).
