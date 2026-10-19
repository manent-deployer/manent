# Sistemi di metriche

Health gate e analisi canary possono leggere le metriche dal sistema che i tuoi team usano già. Ogni metrica di un gate sceglie il proprio sistema con `provider`, quindi un gate può combinarli: errori da Prometheus, latenza da Datadog, throughput del load balancer da CloudWatch.

| `provider` | Sistema | Linguaggio delle query |
|---|---|---|
| `prometheus` (default) | Prometheus, Thanos, Mimir, VictoriaMetrics | PromQL. Vedi [Prometheus e Alertmanager](prometheus.md). |
| `datadog` | Datadog | Metric query |
| `newrelic` | New Relic | NRQL tramite NerdGraph |
| `elastic` | Elasticsearch, Elastic Cloud | ES\|QL |
| `cloudwatch` | Amazon CloudWatch | Metric math o Metrics Insights |
| `azure-monitor` | Azure Monitor Logs, Application Insights (basato su workspace) | KQL |

!!! abstract "Regole che valgono per tutti i sistemi"
    - Le credenziali sono riferimenti a segreti nella sezione `rollout` di `manent.yml`, letti a ogni lettura.
    - Le metriche le legge il worker che orchestra il deploy, che deve raggiungere l'API del sistema.
    - **Nessun dato è rosso**, come con Prometheus: un gate che non vede nulla non può dire che la release è sana.
    - I segnaposto `{application}`, `{environment}`, `{region}`, `{az}` e, nei rilasci per percentuale, `{track}` vengono sostituiti in ogni query.

## Datadog

1. In *Organization Settings* crea una **API key** e una **Application key** per un service account. Limita l'application key a `timeseries_query` se la tua organizzazione usa chiavi con scope.
2. Salva entrambe le chiavi e configura il sito della tua organizzazione:

```yaml title="manent.yml"
rollout:
  datadog:
    url: https://api.datadoghq.eu          # default https://api.datadoghq.com; usa il tuo sito
    api_key_ref: "vault:secret/data/manent/datadog#api_key"
    app_key_ref: "vault:secret/data/manent/datadog#app_key"
```

```yaml title="health.metrics"
- name: error-rate
  provider: datadog
  query: 'sum:trace.http.request.errors{service:{application},env:{environment},availability-zone:{az}}.as_rate() / sum:trace.http.request.hits{service:{application},env:{environment},availability-zone:{az}}.as_rate()'
  max: 0.01
```

Datadog sceglie la risoluzione in base alla finestra letta; `.rollup(avg, 15)` la fissa.

## New Relic

1. Crea una **User key** (*API keys → Create a key → User*) per un utente con accesso in lettura ai dati dell'account, e annota l'**account ID**.

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

Scrivi l'NRQL **senza** `SINCE`, `UNTIL` e `TIMESERIES`: li aggiunge manent per la finestra che legge. Il valore è il primo campo numerico del risultato.

## Elastic

1. Crea una **API key** con privilegi di lettura sugli indici o sui data stream che contengono le metriche (`read` e `view_index_metadata`), oppure usa un utente con gli stessi privilegi.

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

Le query ES|QL ricevono anche `{start}`, `{end}` (RFC 3339) e `{step}` (per esempio `15 seconds`). Il valore è l'ultima colonna numerica; per le serie (analisi canary), una riga per intervallo di tempo, ordinate per tempo.

## Amazon CloudWatch

1. Concedi all'identità del worker `cloudwatch:GetMetricData`: tramite la catena dell'SDK (IRSA, ruolo del task o dell'istanza), chiavi statiche, o un ruolo da assumere.

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

Le query vengono lette con `GetMetricData`, in Metrics Insights (`SELECT …`) o in metric math; il periodo viene arrotondato a un valore accettato da CloudWatch (1, 5, 10, 30 secondi o multipli di 60).

## Azure Monitor

1. Assegna a un'identità il ruolo **Log Analytics Reader** sul workspace: un service principal (tenant, client ID e segreto) oppure la workload o managed identity del worker.

```yaml title="manent.yml"
rollout:
  azure_monitor:
    workspace_id: 00000000-0000-0000-0000-000000000000
    tenant_id: <tenant-id>
    client_id: <client-id>
    client_secret_ref: "vault:secret/data/manent/azure#client_secret"
    # senza tenant_id/client_id: workload identity o managed identity del worker
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

La finestra viene passata come `timespan` della query; per le serie usa `bin(TimeGenerated, {step})`. Il valore è l'ultima colonna numerica. `endpoint` e `authority` coprono i cloud sovrani.

## Verifica i gate

```sh
manent validate -f deploy/prod.yml        # un provider senza configurazione dà 422
manent deploy -f deploy/prod.yml --dry-run
```

Durante un rollout reale lo stato mostra l'ultima lettura rossa di ogni stadio, per esempio `metric error-rate (eu-1/eu-1a): valore 0.034 fuori soglia`, e con l'analisi canary il punteggio e le metriche fallite con medie, rapporto e p-value.

Riferimento: [Configurazione di manent](../operazioni/configurazione.md#rollout), [Rilasci per percentuale](../guida/traffico.md).
