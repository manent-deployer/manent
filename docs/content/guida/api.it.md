# API HTTP

Il contratto completo è la specifica OpenAPI in [`api/openapi.yaml`](https://github.com/joeg-ita/manent/blob/main/api/openapi.yaml): un test verifica che corrisponda alle rotte registrate dal server, e l'[interfaccia web](interfaccia-web.md) ne genera i tipi.

L'API è servita da `manent-server serve` (default `:8080`). Accetta e restituisce JSON. Il body di un descrittore può essere al massimo di 1 MiB.

## Autenticazione

Con `auth.mode: enabled` le rotte `/api/v1` richiedono un token bearer, OIDC o statico:

```http
Authorization: Bearer <token>
```

| Esito | Risposta |
|---|---|
| Token assente o non valido | `401 Unauthorized`, header `WWW-Authenticate: Bearer realm="manent"` |
| Azione non consentita dai permessi | `403 Forbidden`, con il motivo in `error` |

`/healthz` non richiede autenticazione. Elenchi di deploy, schedule e inventario contengono solo ciò che l'identità può leggere. Vedi [Autenticazione, permessi e audit](../operazioni/accesso.md).

## Riepilogo

| Metodo | Percorso | Descrizione |
|---|---|---|
| `POST` | `/api/v1/deployments` | Accoda un deploy, lo pianifica o crea una schedule. |
| `GET` | `/api/v1/deployments` | Elenca i deploy più recenti. |
| `POST` | `/api/v1/deployments/plan` | Calcola il piano senza avviare nulla. |
| `POST` | `/api/v1/deployments/validate` | Verifica un descrittore come all'accodamento e restituisce il deploy effettivo. |
| `POST` | `/api/v1/environments/validate` | Verifica una configurazione di ambiente candidata (`application`, `environment`, `config`, `descriptor` facoltativo): vedi [CI e infrastruttura come codice](ci-iac.md#env-validate). |
| `GET` | `/api/v1/deployments/{id}` | Stato e risultato di un deploy. |
| `GET` | `/api/v1/deployments/{id}/steps` | Timeline degli step. |
| `GET` | `/api/v1/deployments/{id}/logs?offset=` | Log del deploy. |
| `GET` | `/api/v1/deployments/{id}/events` | Stato e log come Server-Sent Events. |
| `POST` | `/api/v1/deployments/{id}/cancel` | Annulla un deploy. |
| `POST` | `/api/v1/deployments/{id}/approve` | Approva un deploy in attesa. |
| `POST` | `/api/v1/deployments/{id}/reject` | Respinge un deploy in attesa. |
| `GET` | `/api/v1/approvals` | Deploy in attesa di una decisione che il chiamante può prendere. |
| `GET` | `/api/v1/me` | Identità del chiamante e ruoli che le si applicano. |
| `GET` | `/api/v1/activity` | Deploy non ancora conclusi: in esecuzione, in attesa di approvazione, in coda, pianificati. |
| `POST` | `/api/v1/releases` | Invia una [release composta](catene.md#release-composte). |
| `GET` | `/api/v1/releases` | Elenca le release più recenti. |
| `GET` | `/api/v1/releases/{id}` | Stato di una release e dei suoi deploy. |
| `POST` | `/api/v1/releases/{id}/cancel` | Annulla i deploy non conclusi e la release. |
| `GET` | `/api/v1/workers` | Worker registrati con stato, heartbeat, risorse, code e workflow in esecuzione (`platform.read`). |
| `GET` | `/api/v1/queues` | Code con limiti, workflow in attesa, in esecuzione e pianificati e worker in ascolto (`platform.read`). |
| `POST` | `/api/v1/promotions` | Promuove un artefatto da un ambiente a un altro. |
| `POST` | `/api/v1/rollbacks` | Torna a una versione precedente. |
| `GET` | `/api/v1/inventory` | Release in esercizio per applicazione e ambiente. |
| `GET` | `/api/v1/freezes` | Finestre di freeze attive e future. |
| `GET` | `/api/v1/tenants?application=&environment=&selector=` | [Tenant](tenant.md) del registro, filtrati dai permessi. |
| `GET` | `/api/v1/tenants/{name}` | Un tenant (parametri e nomi dei segreti). |
| `POST` | `/api/v1/freezes` | Crea una finestra di freeze a runtime (`freeze.write`). |
| `DELETE` | `/api/v1/freezes/{name}` | Elimina una finestra di freeze creata a runtime. |
| `GET` | `/api/v1/policies` | Revisioni della policy di governance e quella in vigore (`policy.read`). |
| `POST` | `/api/v1/policies` | Carica una revisione in bozza (`policy.write`). |
| `GET` | `/api/v1/policies/{revision}` | Una revisione con i suoi file. |
| `POST` | `/api/v1/policies/{revision}/evaluate` | Decisione di una revisione su un descrittore (`policy.write`). |
| `POST` | `/api/v1/policies/{revision}/access` | Permessi di un'identità indicata a mano, differenze rispetto alla revisione attiva ed esito di un'azione (`policy.write`, `rbac.engine: opa`). |
| `GET` | `/api/v1/policies/{revision}/impact` | Decisioni dei deploy reali ricalcolate con una revisione (`policy.write`). |
| `POST` | `/api/v1/policies/{revision}/approve` | Approva e attiva una bozza (`policy.approve`). |
| `POST` | `/api/v1/policies/{revision}/activate` | Torna a una revisione precedente (`policy.approve`). |
| `GET` | `/api/v1/metrics/dora` | Metriche DORA. |
| `GET` | `/api/v1/schedules` | Elenca le schedule ricorrenti. |
| `GET` | `/api/v1/schedules/{nome}` | Dettaglio di una schedule. |
| `POST` | `/api/v1/schedules/{nome}/pause` | Sospende una schedule. |
| `POST` | `/api/v1/schedules/{nome}/resume` | Riattiva una schedule. |
| `DELETE` | `/api/v1/schedules/{nome}` | Elimina una schedule. |
| `GET` | `/ui/config.json` | Configurazione pubblica dell'[interfaccia web](interfaccia-web.md). Senza autenticazione. |
| `POST` | `/integrations/slack/interactions` | Pulsanti di approvazione dei messaggi Slack, autenticati dalla firma di Slack (vedi [Chat e change management](integrazioni.md#approvazioni-da-chat)). Registrata solo con `integrations.slack.signing_secret_ref`. |
| `GET` | `/healthz` | Il processo è attivo. Senza autenticazione. |
| `GET` | `/readyz` | Il processo raggiunge il database e non è in arresto. Senza autenticazione. |
| `GET` | `/metrics` | Metriche Prometheus (con `observability.metrics`). Senza autenticazione. |

## Deploy

### Accodare un deploy

```http
POST /api/v1/deployments?dry_run=false
Content-Type: application/json

{ ...descrittore... }
```

| Esito | Risposta |
|---|---|
| Deploy accodato o pianificato con `schedule.at` | `202 Accepted`, header `Location: /api/v1/deployments/{id}` |
| Schedule ricorrente creata o aggiornata (`schedule.cron`) | `201 Created`, header `Location: /api/v1/schedules/{nome}` |
| Descrittore non valido o parametro `dry_run` non booleano | `400 Bad Request` |
| Richiesta non accettabile (piano non valido, orario nel passato, callback, rollback o sorgente dell'artefatto non ammessi, descrittore non ammesso dalla `descriptor_policy` o incompatibile con la configurazione di ambiente, freeze attivo, versione non promossa) | `422 Unprocessable Entity` |
| Azione non consentita (es. `deployment.submit` sull'ambiente) | `403 Forbidden` |

```json title="202 Accepted"
{"id": "myapp-1.2.3-test", "state": "ENQUEUED"}
```

```json title="202 Accepted, deploy pianificato"
{"id": "4f1c…", "state": "DELAYED", "scheduled_at": "2026-10-10T02:00:00+02:00"}
```

```json title="202 Accepted, approvazione richiesta"
{"id": "7c0e…", "state": "AWAITING_APPROVAL"}
```

La compatibilità con la configurazione di ambiente e la `descriptor_policy` si verifica già qui. Se la configurazione di ambiente non è raggiungibile (es. Consul non disponibile) la richiesta viene accettata e la verifica resta al worker.

Con `?dry_run=true` (oppure `"dryRun": true` nel descrittore) viene accodato un [dry-run](dry-run.md) e la risposta contiene `"dry_run": true`.

### Calcolare il piano

```http
POST /api/v1/deployments/plan
```

```json title="200 OK"
{"plan": [{"name": "secrets", "max_retries": 0}, {"name": "artifacts", "max_retries": 3}]}
```

Richiede `deployment.read` sull'applicazione e l'ambiente del descrittore.

### Verificare un descrittore

`POST /api/v1/deployments/validate` esegue le verifiche dell'accodamento (policy sui descrittori, configurazione di ambiente, callback, sorgente dell'artefatto, rollback) senza accodare nulla. È quello che usa `manent validate`. Richiede `deployment.read`.

```json
{
  "valid": true,
  "plan": [{"name": "secrets", "max_retries": 0}, {"name": "deploy", "max_retries": 2}],
  "deploy": {"plugin": "remote-copy", "targets": [{"host": "10.0.0.5", "user": "deploy"}], "params": {"dest_dir": "/opt/myapp"}},
  "secrets": ["deploy-key"],
  "config": {"source": "consul", "revision": "1842"},
  "governance": {"rule": "produzione", "requires": ["preprod"], "approvals": 1},
  "warnings": []
}
```

Con un [rilascio a ondate](ondate.md) la risposta contiene anche `rollout`, con gli stadi e i passi che verrebbero eseguiti. Un descrittore non valido produce `422` con il motivo. Se la configurazione di ambiente non è raggiungibile la risposta è comunque `200`, con l'avviso in `warnings`.

### Stato di un deploy

```http
GET /api/v1/deployments/{id}
```

```json title="200 OK"
{
  "id": "myapp-1.2.3-test",
  "state": "SUCCESS",
  "application": "myapp",
  "environment": "test",
  "artifact": "myapp",
  "version": "1.2.3",
  "sha256": "2d27fbdf…",
  "kind": "deploy",
  "requested_by": "oidc:alice",
  "created_at": "2026-10-09T10:00:00Z",
  "updated_at": "2026-10-09T10:01:12Z",
  "result": {
    "deployment_id": "myapp-1.2.3-test",
    "steps": [
      {"name": "config", "output": {"source": "consul:manent/myapp/test", "revision": "42", "targets": "1"}},
      {"name": "artifacts", "output": {"local_path": "…", "sha256": "…", "size": "10240"}},
      {"name": "deploy", "output": {"plugin": "remote-copy", "result": "{…}"}}
    ]
  }
}
```

| Stato | Significato |
|---|---|
| `ENQUEUED` | In coda, in attesa di un worker. |
| `DELAYED` | Pianificato con `schedule.at`, in attesa dell'orario. |
| `PENDING` | In esecuzione. |
| `SUCCESS` | Concluso con successo: `result` contiene l'output di ogni step. |
| `ERROR` | Concluso con errore: `error` contiene il messaggio. |
| `CANCELLED` | Annullato. |
| `AWAITING_APPROVAL` | In attesa delle [approvazioni](governance.md#approvazioni): `approval` riporta le decisioni. |
| `REJECTED`, `EXPIRED` | Respinto o non approvato in tempo: il deploy non è stato eseguito. |
| `WAITING_DEPENDENCY` | In attesa che i deploy in [`after`](catene.md) riescano: `after` ne riporta lo stato. |
| `DEPENDENCY_FAILED` | Una dipendenza non è riuscita o l'attesa è scaduta: il deploy non è stato eseguito. |

Un ID inesistente restituisce `404`.

| Campo | Significato |
|---|---|
| `requested_by` | Identità che ha richiesto il deploy; per i deploy di una schedule, chi l'ha creata o aggiornata per ultimo. |
| `kind`, `source` | `deploy`, `promotion` o `rollback`; per gli ultimi due, il deploy da cui derivano. |
| `sha256` | Hash dell'artefatto rilasciato, registrato al termine del deploy. |
| `rollback` | Esito del [rollback automatico](rollback.md), se eseguito. |
| `dry_run` | `true` per un dry-run. |

### Elenco

```http
GET /api/v1/deployments?application=myapp&environment=prod&state=SUCCESS&limit=50
```

Restituisce `{"deployments": [...]}` dal più recente, senza il risultato degli step (da leggere con il dettaglio).

| Parametro | Descrizione |
|---|---|
| `schedule` | Solo le esecuzioni di una schedule ricorrente. |
| `application`, `environment` | Solo i deploy dell'applicazione o dell'ambiente. |
| `state` | Solo i deploy nello stato indicato (es. `AWAITING_APPROVAL`). |
| `limit` | Numero massimo di risultati (default 50), intero positivo. |
| `cursor` | `next_cursor` della pagina precedente: l'elenco riparte dal deploy successivo. |

Filtri e permessi non riducono il numero di risultati: l'elenco prosegue sui deploy più vecchi fino a `limit` (esaminandone al più 10.000). Quando la pagina è piena la risposta ha anche `next_cursor`, l'ID del suo ultimo deploy.

### Annullare

```http
POST /api/v1/deployments/{id}/cancel
```

Risponde `204 No Content`, oppure `404` se il deploy non esiste.

## Schedule

`GET /api/v1/schedules` restituisce `{"schedules": [...]}`:

```json
{
  "name": "myapp-nightly",
  "cron": "0 0 2 * * *",
  "timezone": "Europe/Rome",
  "status": "ACTIVE",
  "application": "myapp",
  "environment": "test",
  "requested_by": "oidc:bob",
  "last_fired_at": "2026-10-09T00:00:00Z"
}
```

`pause`, `resume` e `DELETE` rispondono `204 No Content` oppure `404`.

## Governance

Vedi [Governance dei rilasci](governance.md) per il significato di ogni operazione.

### Approvare o respingere

```http
POST /api/v1/deployments/{id}/approve
Content-Type: application/json

{"comment": "CHG-1234"}
```

Il body è facoltativo. Risponde `204`; `403` senza il permesso `deployment.approve` o se chi approva ha richiesto il deploy; `422` se il deploy non è in attesa di approvazione o l'identità ha già deciso. `/reject` ha la stessa forma.

### Promuovere

```http
POST /api/v1/promotions

{"application": "myapp", "from": "preprod", "to": "prod", "version": "1.4.0",
 "deploy": {"targets": [{"host": "prod-1"}]}, "id": "myapp-1.4.0-prod", "dry_run": false, "freeze_override": ""}
```

Solo `application`, `from` e `to` sono obbligatori. `deploy` sostituisce la sezione deploy del descrittore di origine, i cui target non vengono mai copiati. Risponde come `POST /deployments`.

### Rollback manuale

```http
POST /api/v1/rollbacks

{"application": "myapp", "environment": "prod", "version": "1.3.2"}
```

Senza `version` torna alla versione precedente a quella in esercizio. Accetta anche `id`, `dry_run` e `freeze_override`. Risponde come `POST /deployments`; `422` se non c'è una versione a cui tornare o il tipo di artefatto non ammette il rollback.

### Inventario e freeze

```http
GET /api/v1/inventory?application=myapp
GET /api/v1/freezes?environment=prod
```

Restituiscono `{"inventory": [...]}` e `{"freezes": [...]}`. Le finestre di freeze sono visibili a ogni identità autenticata.

```json title="Finestra di freeze"
{"name": "weekend", "applications": ["*"], "environments": ["prod"], "active": false,
 "start": "2026-10-16T18:00:00+02:00", "end": "2026-10-19T08:00:00+02:00", "recurring": true}
```

### Freeze a runtime e policy

```http
POST /api/v1/freezes

{"name": "incidente-881", "reason": "incidente 881", "applications": ["*"], "environments": ["prod"],
 "to": "2026-10-18T18:00:00Z", "hard": true}
```

`from` vale adesso per default. Risponde `201` con la finestra. Risponde `403` senza `freeze.write` su ogni applicazione e ambiente della finestra, e `422` per un nome non valido, un duplicato o una finestra nel passato. `DELETE /api/v1/freezes/{name}` risponde `204`, oppure `404` per una finestra non creata a runtime.

```http
POST /api/v1/policies

{"files": {"governance.rego": "package manent.governance\n…", "data.yaml": "…"}, "comment": "approvazione dei DBA"}
```

Risponde `201` con la revisione in bozza e l'esito dei test, oppure `422` con l'errore di compilazione o i test falliti. `{revision}` è un numero o `current` (la revisione in vigore). Vedi [Policy in Rego](policy.md#revisioni).

## Release composte { #release-composte }

`POST /api/v1/releases` invia una [release composta](catene.md#release-composte). Il corpo è la release in JSON con i descrittori inclusi (`descriptor`; `file` è solo per la CLI):

```json
{
  "apiVersion": "manent/v1", "kind": "Release", "id": "shop-2026-10", "onFailure": "rollback",
  "deploys": [
    {"name": "db",  "descriptor": {"apiVersion": "manent/v1", "application": "shop-db", "environment": "prod", "artifact": {"type": "migration", "name": "shop-db", "version": "42", "source": "https://…"}}},
    {"name": "api", "after": ["db"], "descriptor": {"apiVersion": "manent/v1", "application": "shop-api", "environment": "prod", "artifact": {"type": "application", "name": "shop-api", "version": "2.4.0", "source": "https://…"}}}
  ]
}
```

Risponde `202` con lo stato della release: `id`, `state` (`PENDING`, `SUCCESS`, `ERROR`, `CANCELLED`), `on_failure`, `requested_by`, `error` e `deploys`, in ordine topologico, con `name`, `deployment_id`, `application`, `environment`, `version`, `after`, `state`, `error`, `rollback_id` e `rollback_state`. Una release non valida (cicli, dipendenze sconosciute, `schedule` o `dryRun` in un deploy) è `400`; un deploy rifiutato da policy o governance è `422`. Nella risposta i deploy sono `ENQUEUED`: li accoda subito dopo il workflow della release, e le letture successive ne riportano lo stato effettivo. Inviare di nuovo lo stesso `id` restituisce la release esistente.

`GET /api/v1/releases[?limit=]` restituisce `{"releases": [...]}`, `GET /api/v1/releases/{id}` una singola release, `POST /api/v1/releases/{id}/cancel` la annulla (`204`).

## Log, timeline e metriche DORA

```sh
curl -H "Authorization: Bearer $TOKEN" https://manent.example.com/api/v1/deployments/7c0e…/steps
curl -H "Authorization: Bearer $TOKEN" "https://manent.example.com/api/v1/deployments/7c0e…/logs?offset=0"
```

```json
{
  "lines": [
    {"time": "2026-10-10T12:03:12Z", "level": "INFO", "step": "artifacts", "message": "artefatto recuperato",
     "attrs": {"sha256": "2d27…"}}
  ],
  "next": 1,
  "done": false
}
```

Per seguire un deploy si ripete la richiesta con `offset` uguale a `next` finché `done` è `true`. `GET /api/v1/metrics/dora?application=&environment=&since=` accetta per `since` una data RFC 3339, una durata (`720h`) o un numero di giorni (`30d`, default). Tutte richiedono `deployment.read`. Vedi [Osservabilità](../operazioni/osservabilita.md).

### Eventi in tempo reale { #eventi-in-tempo-reale }

`GET /api/v1/deployments/{id}/events` invia lo stato e le nuove righe di log di un deploy come Server-Sent Events, con gli stessi permessi di stato e log:

| Evento | Dati |
|---|---|
| `status` | Lo stato del deploy, come `GET /api/v1/deployments/{id}`, ogni volta che cambia. |
| `log` | Un array di nuove righe di log. L'`id` dell'evento è l'offset successivo: un client che si riconnette con `Last-Event-ID` (o `?offset=`) riprende da lì. |
| `end` | Il deploy è concluso e tutte le sue righe sono state inviate; il server chiude lo stream. |
| `error` | La lettura di stato o log è fallita. |

Un commento `: keep-alive` ogni 15 secondi tiene aperta la connessione attraverso i proxy. `EventSource` non può inviare l'intestazione `Authorization`: con l'autenticazione lo stream si legge con `fetch` (come fa l'interfaccia web) o con un client che supporta le intestazioni (`curl -N`).

### Identità e approvazioni

`GET /api/v1/me` restituisce l'identità della richiesta (`subject`, `name`, `method`, `groups`), se l'autenticazione è attiva e i ruoli che le si applicano, con applicazioni e ambienti (`grants`). `GET /api/v1/approvals` restituisce `{"approvals": [...]}`: i deploy in attesa di una decisione, prima dell'avvio o tra due ondate, per cui il chiamante ha `deployment.approve` e che non ha richiesto lui. La CLI li espone come `manent whoami` e `manent approvals`.

### Attività e worker

`GET /api/v1/activity[?application=&environment=]` restituisce i deploy non ancora conclusi, dal più recente, in cinque elenchi: `running` (con `executor`, `started_at`, `last_step`, l'ultimo step registrato, e `rollout_progress`), `awaiting_approval`, `waiting_dependency` (con `after`), `queued` (dal più vecchio: escono dalla coda in quest'ordine) e `delayed` (per orario di partenza). `truncated` è `true` se i workflow attivi erano più di quelli esaminati (1000). Contiene solo i deploy che il chiamante può leggere.

`GET /api/v1/workers` restituisce `{"workers": [...]}`: per ogni worker `id`, `state` (`alive`, `stale`, `stopped`, `unregistered`), `version` (versione dei workflow), `build`, `hostname`, `region`, `started_at`, `heartbeat_at`, `recovered_by`, `pending` (workflow in sospeso per versione) e `tasks`, i workflow in esecuzione (`kind`: `deploy`, `pass`, `gate`, `approval`, `schedule`, `after`, `release`, con `deployment_id`, applicazione, ambiente, region e `started_at`). Richiede `platform.read`; `hidden_tasks` conta i workflow delle applicazioni che il chiamante non può leggere. Ogni worker ha anche `queues`, le code da cui preleva, e `resources`, il campione che invia a ogni heartbeat: `cpus` e `cpu_limit` (CPU dell'host e utilizzabili dal processo, che segue la quota del container), `process_cpu_cores` (CPU usata dal campione precedente), `memory_total_bytes`, `memory_available_bytes` e `memory_limit_bytes` (host e container), `process_memory_bytes`, `load1`/`load5`/`load15`, `workdir_free_bytes`, `concurrency`, `goroutines` e `sampled_at`; i valori che il sistema operativo non fornisce mancano.

`GET /api/v1/queues` restituisce `{"queues": [...]}`: per ogni coda `name`, `kind` (`deployments`, `approvals`, `region`, `other`), `region`, i limiti `worker_concurrency`, `partition_concurrency` e `global_concurrency`, i conteggi `enqueued`, `running` e `delayed`, `oldest_enqueued` e `workers`, i worker attivi in ascolto. Richiede `platform.read`.

La CLI li espone come `manent activity`, `manent workers` e `manent queues`.

Le richieste propagano il contesto di tracing W3C (`traceparent`): la traccia di un client, ad esempio una pipeline di CI, prosegue nell'API e negli step del worker.

## Errori

Gli errori hanno sempre la forma:

```json
{"error": "messaggio"}
```

| Status | Significato |
|---|---|
| `400` | Richiesta o descrittore malformati. |
| `401` | Token mancante o non valido. |
| `403` | Permesso negato. |
| `404` | Deploy, schedule, revisione o finestra inesistenti. |
| `422` | Richiesta non accettabile (policy, governance, orario passato, ...). |
| `503` | Temporaneamente non disponibile: ad esempio nessuna policy di governance caricata. Riprovare. |

## Log degli accessi

Ogni richiesta viene registrata nel log applicativo e quindi inoltrata anche al SIEM, con metodo, percorso, status, durata, IP del client e identità (`actor`). Le azioni rilevanti producono anche una voce di [audit](../operazioni/accesso.md#audit).
