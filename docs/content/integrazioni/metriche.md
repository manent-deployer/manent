# Metrics systems

Health gates and canary analysis can read metrics from the system your teams already use. Each metric of a gate chooses its system with `provider`, so one gate can mix them: errors from Prometheus, latency from Datadog, load balancer throughput from CloudWatch.

| `provider` | System | Query language |
|---|---|---|
| `prometheus` (default) | Prometheus, Thanos, Mimir, VictoriaMetrics | PromQL. See [Prometheus and Alertmanager](prometheus.md). |
| `datadog` | Datadog | Metric query |
| `newrelic` | New Relic | NRQL through NerdGraph |
| `elastic` | Elasticsearch, Elastic Cloud | ES\|QL |
| `cloudwatch` | Amazon CloudWatch | Metric math or Metrics Insights |
| `azure-monitor` | Azure Monitor Logs, Application Insights (workspace-based) | KQL |

!!! abstract "Rules that apply to every system"
    - Credentials are secret references in the `rollout` section of `manent.yml`, read at every reading.
    - Metrics are read by the worker that orchestrates the deploy, which must reach the system's API.
    - **No data is red**, as with Prometheus: a gate that sees nothing cannot say the release is healthy.
    - Placeholders `{application}`, `{environment}`, `{region}`, `{az}` and, in percentage rollouts, `{track}` are substituted in every query.

## Datadog

1. In *Organization Settings*, create an **API key** and an **Application key** for a service account. Scope the application key to `timeseries_query` if your organization uses scoped keys.
2. Store both keys and configure the site of your organization:

```yaml title="manent.yml"
rollout:
  datadog:
    url: https://api.datadoghq.eu          # default https://api.datadoghq.com; use your site
    api_key_ref: "vault:secret/data/manent/datadog#api_key"
    app_key_ref: "vault:secret/data/manent/datadog#app_key"
```

```yaml title="health.metrics"
- name: error-rate
  provider: datadog
  query: 'sum:trace.http.request.errors{service:{application},env:{environment},availability-zone:{az}}.as_rate() / sum:trace.http.request.hits{service:{application},env:{environment},availability-zone:{az}}.as_rate()'
  max: 0.01
```

Datadog chooses the resolution from the window read; add `.rollup(avg, 15)` to set it.

## New Relic

1. Create a **User key** (*API keys → Create a key → User*) for a user with read access to the account's data, and note the **account ID**.

```yaml title="manent.yml"
rollout:
  newrelic:
    url: https://api.eu.newrelic.com/graphql    # default https://api.newrelic.com/graphql
    account_id: "1234567"
    api_key_ref: "vault:secret/data/manent/newrelic#user_key"
```

```yaml title="health.metrics"
- name: error-rate
  provider: newrelic
  query: "SELECT percentage(count(*), WHERE error IS true) / 100 FROM Transaction WHERE appName = '{application}'"
  max: 0.01
```

Write NRQL **without** `SINCE`, `UNTIL` and `TIMESERIES`: manent adds them for the window it reads. The first numeric field of the result is the value.

## Elastic

1. Create an **API key** with read privileges on the indices or data streams holding your metrics (`read` and `view_index_metadata`), or use a user with the same privileges.

```yaml title="manent.yml"
rollout:
  elastic:
    url: https://es.example.com:9200
    api_key_ref: "vault:secret/data/manent/elastic#api_key"
    # username: manent
    # password_ref: "vault:secret/data/manent/elastic#password"
    # ca_file: /etc/ssl/certs/corp-ca.pem
```

```yaml title="health.metrics"
- name: error-rate
  provider: elastic
  query: |
    FROM metrics-shop*
    | WHERE service.name == "{application}" AND @timestamp >= "{start}" AND @timestamp < "{end}"
    | STATS rate = AVG(http.error_rate)
  max: 0.01
```

ES|QL queries also receive `{start}`, `{end}` (RFC 3339) and `{step}` (for example `15 seconds`). The last numeric column is the value; for series (canary analysis), one row per time bucket sorted by time.

## Amazon CloudWatch

1. Give the worker's identity `cloudwatch:GetMetricData`: through the SDK chain (IRSA, task or instance role), static keys, or a role to assume.

```yaml title="manent.yml"
rollout:
  cloudwatch:
    region: eu-west-1
    # access_key_id_ref: "vault:secret/data/manent/aws#access_key_id"
    # secret_access_key_ref: "vault:secret/data/manent/aws#secret_access_key"
    # role_arn: arn:aws:iam::123456789012:role/manent-metrics-reader
    # external_id: manent
```

```yaml title="health.metrics"
- name: target-5xx
  provider: cloudwatch
  query: "SELECT SUM(HTTPCode_Target_5XX_Count) FROM \"AWS/ApplicationELB\" WHERE TargetGroup = 'targetgroup/{application}-{environment}/0123456789abcdef'"
  max: 5
```

Queries are read with `GetMetricData`, either Metrics Insights (`SELECT …`) or metric math; the period is rounded to a value CloudWatch accepts (1, 5, 10, 30 seconds or multiples of 60).

## Azure Monitor

1. Give an identity the **Log Analytics Reader** role on the workspace: a service principal (tenant, client ID and secret) or the worker's own workload or managed identity.

```yaml title="manent.yml"
rollout:
  azure_monitor:
    workspace_id: 00000000-0000-0000-0000-000000000000
    tenant_id: <tenant-id>
    client_id: <client-id>
    client_secret_ref: "vault:secret/data/manent/azure#client_secret"
    # without tenant_id/client_id: workload identity or managed identity of the worker
```

```yaml title="health.metrics"
- name: error-rate
  provider: azure-monitor
  query: |
    AppRequests
    | where AppRoleName == "{application}"
    | summarize rate = todouble(countif(Success == false)) / count()
  max: 0.01
```

The window is passed as the query's `timespan`; for series use `bin(TimeGenerated, {step})`. The last numeric column is the value. `endpoint` and `authority` cover sovereign clouds.

## Check the gates

```sh
manent validate -f deploy/prod.yml        # a provider without configuration is a 422
manent deploy -f deploy/prod.yml --dry-run
```

During a real rollout the status shows the last red reading of each stage, for example `metric error-rate (eu-1/eu-1a): valore 0.034 fuori soglia`, and with canary analysis the score and the failed metrics with means, ratio and p-value.

Reference: [Configuring manent](../operazioni/configurazione.md#rollout), [Percentage rollouts](../guida/traffico.md).
