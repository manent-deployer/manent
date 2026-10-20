# Osservabilità

manent espone metriche Prometheus, tracce OpenTelemetry, il log e la timeline di ogni deploy e le metriche DORA. I log applicativi continuano ad andare anche al [SIEM](log-siem.md).

```yaml title="manent.yml"
observability:
  listen: ":9090"     # /metrics, /healthz, /readyz su una porta dedicata
  metrics: true       # default
  tracing:
    enabled: true
    endpoint: otel-collector:4318
    insecure: true
    sample_ratio: 1
```

## Sonde e metriche

| Percorso | Dove | Descrizione |
|---|---|---|
| `/healthz` | API e `observability.listen` | Il processo è attivo (liveness). |
| `/readyz` | API e `observability.listen` | Il processo raggiunge il database e non è in arresto (readiness). Durante il [drain](affidabilita.md#arresto-ordinato-drain) risponde `503`. |
| `/metrics` | API e `observability.listen` | Metriche in formato Prometheus. |

`worker` non ha l'API: per sonde e metriche serve `observability.listen`. Con `serve` le stesse rotte sono anche sulla porta dell'API. Non richiedono autenticazione, quindi la porta va esposta solo alla rete di monitoraggio (vedi [Sicurezza](sicurezza.md)).

### Metriche

| Metrica | Tipo | Etichette | Descrizione |
|---|---|---|---|
| `manent_deployments_total` | counter | `application`, `environment`, `kind`, `outcome`, `dry_run` | Deploy conclusi dal processo. |
| `manent_deployment_duration_seconds` | histogram | `application`, `environment`, `outcome` | Durata dall'avvio sul worker alla conclusione, senza l'attesa in coda né le approvazioni. |
| `manent_step_duration_seconds` | histogram | `step`, `outcome` | Durata di ogni tentativo di uno step: anche i retry. |
| `manent_rollout_passes_total` | counter | `application`, `environment`, `region`, `az`, `outcome` | Passi dei [rilasci a ondate](../guida/ondate.md) conclusi dal processo. |
| `manent_http_request_duration_seconds` | histogram | `method`, `route`, `code` | Richieste all'API. |
| `manent_workflows` | gauge | `workflow`, `status`, `queue` | Workflow per stato: deploy in coda, in esecuzione, in attesa di approvazione e conclusi. |
| `manent_executor_up` | gauge | `executor`, `version` | 1 se il worker invia heartbeat, 0 se è perso o arrestato. |
| `manent_executor_heartbeat_timestamp_seconds` | gauge | `executor`, `version` | Ultimo heartbeat. |
| `manent_targets` | gauge | `application`, `environment`, `state` | Target per stato letto dalla [riconciliazione](../guida/governance.md#stato-reale): `in-sync`, `drifted`, `missing`, `standby`, `unknown`. |
| `manent_build_info` | gauge | `version`, `commit`, `workflow_version` | Versione del binario e dei workflow. |
| `manent_policy_revision_info` | gauge | `engine`, `source`, `revision` | [Policy di governance](../guida/policy.md) in vigore nel processo. |
| `manent_policy_age_seconds` | gauge | | Secondi dall'ultima lettura riuscita della policy e dei freeze a runtime. |
| `manent_policy_evaluations_total` | counter | `phase`, `outcome` | Valutazioni della policy (`allowed`, `denied`, `frozen`, `error`, `unavailable`). |
| `manent_policy_eval_duration_seconds` | histogram | `phase` | Durata delle valutazioni. |
| `manent_authz_grants_evaluations_total` | counter | `outcome` | Permessi di un'identità calcolati dalla [policy dei permessi](../guida/permessi.md) (`ok`, `cached`, `error`). |
| `manent_authz_deny_evaluations_total` | counter | `action`, `outcome` | Divieti valutati (`allowed`, `denied`, `error`). |
| `manent_authz_eval_duration_seconds` | histogram | `part` | Durata delle valutazioni dei permessi (`grants`, `deny`). |
| `manent_authz_invalid_grants_total` | counter | | Permessi scartati perché non validi o con azioni `policy.*`. |
| `manent_authz_cache_entries` | gauge | | Identità nella cache dei permessi. |
| `manent_policy_load_errors_total` | counter | | Letture fallite: revisione rifiutata o sorgente irraggiungibile. |

Più le metriche standard del runtime Go (`go_*`) e del processo (`process_*`).

!!! note "Metriche globali e di processo"
    `manent_deployments_total` e gli istogrammi sono del singolo processo e vanno sommati tra le istanze (`sum`). `manent_workflows`, `manent_executor_*` e `manent_targets` sono letti dal database, quindi ogni istanza riporta gli stessi valori: vanno aggregati con `max`. La lettura dal database avviene al più ogni 15 secondi per i workflow non conclusi (in coda, in esecuzione, pianificati) e al più ogni 5 minuti per i conclusi, il cui conteggio scorre tutto lo storico.

Esempi di query e allarmi:

```promql
# Deploy falliti nell'ultima ora per applicazione e ambiente
sum by (application, environment) (increase(manent_deployments_total{outcome="failed",dry_run="false"}[1h]))

# Deploy in coda da troppo: coda ferma o worker insufficienti
max(manent_workflows{workflow="manent.deploy",status="ENQUEUED"}) > 20

# Worker perso
max by (executor) (manent_executor_up) == 0

# Target cambiati fuori da manent
max by (application, environment) (manent_targets{state=~"drifted|missing"}) > 0

# Durata p95 dei deploy in produzione
histogram_quantile(0.95, sum by (le) (rate(manent_deployment_duration_seconds_bucket{environment="prod"}[1d])))
```

## Tracing

Con `tracing.enabled` manent esporta le tracce via OTLP/HTTP (`endpoint`, oppure `OTEL_EXPORTER_OTLP_ENDPOINT`). Una traccia segue il deploy dalla richiesta all'API fino agli step eseguiti dal worker:

```text
POST /api/v1/deployments          (API, figlia del traceparent del client, ad es. la CI)
└── deployment                    (worker: manent.deployment.id, application, environment, outcome)
    ├── step config
    ├── step secrets
    ├── step artifacts
    ├── step deploy
    └── step activation
```

La traccia della richiesta viene salvata nel deploy (`trace_parent`) e passata al worker, anche se il deploy parte dopo un'approvazione o all'orario di `schedule.at`. Le esecuzioni delle schedule ricorrenti iniziano tracce nuove. Ogni tentativo di uno step è una span: i retry sono visibili. Dopo una recovery il deploy produce una nuova span `deployment` nella stessa traccia.

## Log e timeline di un deploy

I log emessi durante gli step (da manent e dagli step che usano il logger con il context dello step) sono salvati con il deploy e consultabili da qualsiasi istanza, anche dopo che il worker che lo ha eseguito si è fermato:

```sh
manent logs 7c0e…            # tutto il log
manent logs -f 7c0e…         # segue il deploy fino alla conclusione
manent logs --json 7c0e…     # una riga JSON per voce
```

```text
2026-10-10 14:03:12 INFO  [artifacts] artefatto recuperato path=work/7c0e…/app.tar.gz sha256=2d27…
2026-10-10 14:03:15 INFO  [deploy] deploy completato su target host=10.0.0.5
```

La timeline riporta ogni step registrato con orari, durata, output ed errore:

```sh
manent steps 7c0e…
```

Via API: `GET /api/v1/deployments/{id}/logs?offset=N` (con `next` e `done` per proseguire) e `GET /api/v1/deployments/{id}/steps`. Richiedono `deployment.read`.

!!! note
    - Sono salvate al più 500 righe per step, di livello `INFO` o superiore, anche se `logging.level` è più alto. Il log applicativo e il SIEM restano completi.
    - Come gli output, i log non devono contenere segreti.
    - I log dei [plugin esterni](../plugins/esterni.md) arrivano al log applicativo e al SIEM, ma non al log del deploy.
    - Log e timeline vengono eliminati con il deploy dalla [retention](affidabilita.md#retention-dello-storico).

## Metriche DORA

```sh
manent dora --since 30d [-a myapp] [-e prod]
```

```json
{
  "since": "2026-09-10T00:00:00Z",
  "until": "2026-10-10T00:00:00Z",
  "total": {"deployments": 42, "failures": 3, "deployments_per_day": 1.3, "change_failure_rate": 0.07,
            "mttr_seconds": 2520, "lead_time_seconds": 172800},
  "environments": [{"application": "myapp", "environment": "prod", "deployments": 12, "failures": 1, "…": "…"}]
}
```

| Metrica | Calcolo |
|---|---|
| `deployments_per_day` | Deploy riusciti nel periodo diviso i giorni. |
| `change_failure_rate` | Deploy che hanno causato un guasto diviso i deploy conclusi. Un guasto è un deploy fallito oppure un [rollback manuale](../guida/governance.md#rollback-manuale), che segnala una release difettosa. |
| `mttr_seconds` | Media del tempo da un guasto al successivo deploy riuscito nello stesso ambiente. |
| `lead_time_seconds` | Mediana del tempo tra il primo rilascio riuscito di una versione, in qualsiasi ambiente, e il suo rilascio nell'ambiente. Misura la velocità di promozione: manent non conosce l'orario del commit. |

I dry-run e i deploy annullati sono esclusi. Il calcolo esamina al più gli ultimi 10.000 deploy e vede solo lo storico conservato dalla retention. Via API: `GET /api/v1/metrics/dora?application=&environment=&since=`, con i soli ambienti che l'identità può leggere.
