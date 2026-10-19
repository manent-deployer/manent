# Prometheus e Alertmanager

Con manent Prometheus ha due ruoli:

1. **Decide se una release è sana.** Gli health gate dei rilasci [a ondate](../guida/ondate.md) e [per percentuale](../guida/traffico.md) eseguono query PromQL e leggono gli allarmi attivi da Alertmanager dopo ogni stadio.
2. **Sorveglia manent stesso.** API e worker espongono metriche su code, deploy, step, worker, policy e target.

!!! abstract "Cosa serve"
    Prometheus (2.x o 3.x) raggiungibile dai worker che orchestrano i rollout · facoltativamente Alertmanager · la tua applicazione che esporta metriche delle richieste con etichette che identificano applicazione e AZ (o track)

## Parte 1: health gate

### Collega manent

```yaml title="manent.yml"
rollout:
  prometheus_url: http://prometheus.monitoring:9090
  alertmanager_url: http://alertmanager.monitoring:9093
  check_timeout: 30s          # tempo massimo di ogni lettura
```

Senza `prometheus_url`, un rollout che usa metriche Prometheus viene rifiutato all'invio. Metriche e allarmi li legge il worker che orchestra il deploy, anche con il [routing per region](../guida/ondate.md#esecuzione-nelle-region): nelle region girano solo le sonde HTTP.

### Etichetta le metriche dell'applicazione

Le query sono modelli: manent sostituisce `{application}`, `{environment}`, `{region}` e `{az}` per ogni stadio, e `{track}` (candidata o stabile) nei rilasci per percentuale. Le tue metriche devono avere etichette corrispondenti, per esempio dalla configurazione dello scrape o dalle etichette dei pod Kubernetes:

```yaml title="prometheus.yml (estratto)"
scrape_configs:
  - job_name: shop
    static_configs:
      - targets: ["web-1.prod.internal:9100"]
        labels: {app: shop, az: eu-1a}
      - targets: ["web-2.prod.internal:9100"]
        labels: {app: shop, az: eu-1b}
```

### Scrivi il gate

```yaml title="envconfig/shop/prod.yml"
rollout:
  waves:
    - {name: canary, select: "canary=true", bake: 10m}
    - {name: rest, select: "canary!=true", by: az, bake: 5m}
  health:
    interval: 30s
    failuresToStop: 2
    metrics:
      - name: error-rate
        query: |
          sum(rate(http_requests_total{app="{application}",az="{az}",code=~"5.."}[5m]))
          / sum(rate(http_requests_total{app="{application}",az="{az}"}[5m]))
        max: 0.01
      - name: p99-latency
        query: |
          histogram_quantile(0.99, sum by (le) (rate(http_request_duration_seconds_bucket{app="{application}",az="{az}"}[5m])))
        max: 0.5
    alerts:
      blockOn: ["severity=critical"]
```

- Una metrica è rossa quando il valore è sopra `max` o sotto `min`, **oppure quando la query non restituisce dati**. Uno stadio che non riceve traffico non può dimostrare di essere sano; se il silenzio è accettabile, dichiaralo con `or vector(0)`.
- Si usa la prima serie del risultato: aggrega (`sum`, `histogram_quantile`) in modo che la query restituisca un solo valore.
- `alerts.blockOn` rende rosso il gate finché un allarme attivo soddisfa **tutte** le condizioni e riguarda lo stadio: le sue etichette `application` (o `app`), `region` e `az` devono corrispondere, e un'etichetta assente dall'allarme corrisponde a tutto.

Prova le query nell'interfaccia di Prometheus con i segnaposto sostituiti, poi verifica il piano con `manent validate`.

### Confronta candidata e stabile

Nei rilasci per percentuale, `compare: stable` valuta la candidata rispetto alla stabile invece che con una soglia fissa, e `analysis: mann-whitney` lo fa con un test statistico sulle serie del gradino:

```yaml
rollout:
  strategy: traffic
  traffic: {steps: [5, 25, 50, 100], bake: 15m}
  health:
    analysis: {pass: 95, marginal: 75}
    metrics:
      - name: errors
        compare: stable
        analysis: mann-whitney
        critical: true
        query: 'sum(rate(http_requests_total{app="{application}",track="{track}",code=~"5.."}[1m])) / sum(rate(http_requests_total{app="{application}",track="{track}"}[1m]))'
```

L'etichetta `track` deve distinguere le due versioni: il plugin Kubernetes imposta `manent.io/track` sui pod, che una regola di relabeling può copiare in `track`. Vedi l'analisi canary in [Rilasci per percentuale](../guida/traffico.md).

## Parte 2: monitorare manent

### Raccogli le metriche di API e worker

| Processo | Endpoint |
|---|---|
| `serve`, `api` | `/metrics` sulla porta dell'API (`:8080`) e su `observability.listen` |
| `worker` | `/metrics` solo su `observability.listen` (per esempio `:9090`): i worker non hanno API |

Su Kubernetes, `serviceMonitor.enabled: true` nel chart Helm crea un ServiceMonitor per il Prometheus Operator. Sulle VM:

```yaml title="prometheus.yml (estratto)"
scrape_configs:
  - job_name: manent
    static_configs:
      - targets: ["manent-api-1:9090", "manent-worker-1:9090", "manent-worker-2:9090"]
```

Gli endpoint non richiedono autenticazione: tieni `observability.listen` sulla rete di monitoraggio, e non esporre `/metrics` dal punto di accesso pubblico dell'API.

### Allarmi

```yaml title="manent-rules.yml"
groups:
  - name: manent
    rules:
      - alert: ManentWorkerLost
        expr: max by (executor) (manent_executor_up) == 0
        for: 5m
        annotations:
          summary: "il worker manent {{ $labels.executor }} non invia heartbeat"
      - alert: ManentQueueStuck
        expr: max(manent_workflows{workflow="manent.deploy",status="ENQUEUED"}) > 20
        for: 15m
      - alert: ManentDeployFailed
        expr: sum by (application, environment) (increase(manent_deployments_total{outcome="failed",dry_run="false"}[30m])) > 0
      - alert: ManentTargetsOutOfSync
        expr: max by (application, environment) (manent_targets{state=~"drifted|missing"}) > 0
        for: 30m
      - alert: ManentPolicyStale
        expr: max(manent_policy_age_seconds) > 600
```

`manent_workflows`, `manent_executor_*` e `manent_targets` vengono letti dal database, quindi ogni istanza riporta lo stesso valore: aggregali con `max`. `manent_deployments_total` e gli istogrammi sono per processo: aggregali con `sum`.

### Dashboard

Pannelli utili: deploy per esito (`manent_deployments_total`), durata p95 dei deploy per ambiente (`manent_deployment_duration_seconds`), durata degli step (`manent_step_duration_seconds`), profondità della coda (`manent_workflows`), worker attivi (`manent_executor_up`), passi delle ondate per region e AZ (`manent_rollout_passes_total`). Le metriche DORA (frequenza dei deploy, lead time, change failure rate, tempo di ripristino) le calcola manent stesso: `manent dora` o la pagina Metriche dell'interfaccia web.

Riferimento: [Osservabilità](../operazioni/osservabilita.md), [Rilasci a ondate](../guida/ondate.md#health-gate).
