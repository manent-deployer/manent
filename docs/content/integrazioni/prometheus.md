# Prometheus and Alertmanager

Prometheus plays two roles with manent:

1. **It decides whether a release is healthy.** Health gates of [wave](../guida/ondate.md) and [percentage](../guida/traffico.md) rollouts run PromQL queries and read active alerts from Alertmanager after every stage.
2. **It watches manent itself.** API and workers expose metrics on queues, deploys, steps, workers, policies and targets.

!!! abstract "What you need"
    Prometheus (2.x or 3.x) reachable from the workers that orchestrate rollouts · optionally Alertmanager · your application exporting request metrics with labels that identify application and AZ (or track)

## Part 1: health gates

### Connect manent

```yaml title="manent.yml"
rollout:
  prometheus_url: http://prometheus.monitoring:9090
  alertmanager_url: http://alertmanager.monitoring:9093
  check_timeout: 30s          # maximum time of each reading
```

Without `prometheus_url`, a rollout that uses Prometheus metrics is rejected at submission. Metrics and alerts are read by the worker that orchestrates the deploy, even with [region routing](../guida/ondate.md#esecuzione-nelle-region): only HTTP probes run in the regions.

### Label your application's metrics

Queries are templates: manent substitutes `{application}`, `{environment}`, `{region}` and `{az}` for each stage, and `{track}` (candidate or stable) in percentage rollouts. Your metrics need matching labels, for example from the scrape configuration or the Kubernetes pod labels:

```yaml title="prometheus.yml (excerpt)"
scrape_configs:
  - job_name: shop
    static_configs:
      - targets: ["web-1.prod.internal:9100"]
        labels: {app: shop, az: eu-1a}
      - targets: ["web-2.prod.internal:9100"]
        labels: {app: shop, az: eu-1b}
```

### Write the gate

```yaml title="envconfig/shop/shop/prod.yml"
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

- A metric is red when its value is above `max` or below `min`, **or when the query returns no data**. A stage that receives no traffic cannot prove it is healthy; if silence is acceptable, say so with `or vector(0)`.
- The first series of the result is used: aggregate (`sum`, `histogram_quantile`) so that the query returns one value.
- `alerts.blockOn` turns the gate red while an active alert matches **all** the conditions and concerns the stage: its `application` (or `app`), `region` and `az` labels must match, and a label missing from the alert matches everything.

Test the queries in the Prometheus UI with the placeholders replaced, then check the plan with `manent validate`.

### Compare candidate and stable

In percentage rollouts, `compare: stable` evaluates the candidate against the stable version instead of a fixed threshold, and `analysis: mann-whitney` does it statistically over the series of the step:

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

The `track` label must distinguish the two versions: the Kubernetes plugin sets `manent.io/track` on the pods, which a relabelling rule can copy into `track`. See [Canary analysis](../guida/traffico.md).

## Part 2: monitoring manent

### Scrape API and workers

| Process | Endpoint |
|---|---|
| `serve`, `api` | `/metrics` on the API port (`:8080`) and on `observability.listen` |
| `worker` | `/metrics` on `observability.listen` only (for example `:9090`): workers have no API |

On Kubernetes, `serviceMonitor.enabled: true` in the Helm chart creates a ServiceMonitor for the Prometheus Operator. On VMs:

```yaml title="prometheus.yml (excerpt)"
scrape_configs:
  - job_name: manent
    static_configs:
      - targets: ["manent-api-1:9090", "manent-worker-1:9090", "manent-worker-2:9090"]
```

The endpoints need no authentication: keep `observability.listen` on the monitoring network, and do not expose `/metrics` through the public entry point of the API.

### Alerts

```yaml title="manent-rules.yml"
groups:
  - name: manent
    rules:
      - alert: ManentWorkerLost
        expr: max by (executor) (manent_executor_up) == 0
        for: 5m
        annotations:
          summary: "manent worker {{ $labels.executor }} is not sending heartbeats"
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

`manent_workflows`, `manent_executor_*` and `manent_targets` are read from the database, so every instance reports the same value: aggregate them with `max`. `manent_deployments_total` and the histograms are per process: aggregate them with `sum`.

### Dashboards

Useful panels: deploys per outcome (`manent_deployments_total`), p95 deploy duration per environment (`manent_deployment_duration_seconds`), step durations (`manent_step_duration_seconds`), queue depth (`manent_workflows`), workers up (`manent_executor_up`), wave passes per region and AZ (`manent_rollout_passes_total`). DORA metrics (deployment frequency, lead time, change failure rate, time to restore) are computed by manent itself: `manent dora` or the Metrics page of the web interface.

Reference: [Observability](../operazioni/osservabilita.md), [Wave rollouts](../guida/ondate.md#health-gate).
