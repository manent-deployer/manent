# Configurazione di manent

manent legge un file YAML indicato con `-c` (o con la variabile `MANENT_CONFIG`). Ogni valore non indicato prende il default riportato sotto; le chiavi sconosciute sono un errore. Il file d'esempio completo è `configs/manent.yml`.

## Riferimento

### `app`

| Chiave | Default | Descrizione |
|---|---|---|
| `name` | `manent` | Nome dell'applicazione DBOS e `APP-NAME` dei messaggi inviati al SIEM. |
| `version` | `0.1.0` | Versione dei workflow registrata da DBOS: un worker riprende solo i workflow della propria versione. Non è la versione del binario e va cambiata solo quando le note di rilascio lo indicano. Vedi [Versioni e aggiornamenti](aggiornamenti.md). |

### `dbos`

| Chiave | Default | Descrizione |
|---|---|---|
| `database_url` | `sqlite:manent.db` | Database di sistema DBOS. In produzione Postgres: `postgres://user:pass@host:5432/manent`. |
| `executor_id` | hostname | ID del worker: deve essere univoco tra i nodi e stabile tra i riavvii. Vedi [Scalabilità](scalabilita.md). |
| `queue` | `deployments` | Nome della coda dei deploy. |
| `concurrency` | `4` | Deploy in esecuzione contemporanea su ogni worker. |
| `global_concurrency` | `0` | Deploy in esecuzione su tutti i worker (`0` = nessun limite). |
| `environment_concurrency` | `1` | Deploy contemporanei della stessa application/environment su tutti i worker. Non può superare `global_concurrency`. |
| `schedule_poll_interval` | `30s` | Ogni quanto i worker rileggono le schedule ricorrenti. |
| `region` | | Region del worker: con `region_routing` esegue i passi dei [rilasci a ondate](../guida/ondate.md#esecuzione-nelle-region) su quella region. |
| `region_routing` | `false` | Esegue i passi dei rollout sui worker della region dei target, come workflow figli. In multi-region va attivato su tutti i worker. |
| `drain_timeout` | `2m` | All'arresto il worker smette di prelevare deploy e attende quelli in corso fino a questo tempo (`0` = non attende). Vedi [drain](affidabilita.md#arresto-ordinato-drain). |

### `http`

| Chiave | Default | Descrizione |
|---|---|---|
| `listen` | `:8080` | Indirizzo dell'API (sovrascrivibile con `serve --listen`). |
| `ui.enabled` | `true` | Serve l'[interfaccia web](../guida/interfaccia-web.md) su `/`. |

### `logging` e `siem`

| Chiave | Default | Descrizione |
|---|---|---|
| `logging.level` | `info` | `debug`, `info`, `warn`, `error`. |
| `logging.format` | `text` | `text` oppure `json`. |
| `siem.enabled` | `false` | Inoltra ogni log alle destinazioni dei log strutturati. |
| `siem.network` | `udp` | `udp`, `tcp`, `unixgram` (socket syslog locale) o `journald` (protocollo nativo). |
| `siem.address` | | `host:porta` del collettore; per `unixgram` e `journald` il percorso del socket (default `/dev/log` e `/run/systemd/journal/socket`). |
| `siem.destinations` | `[]` | Destinazioni aggiuntive (`network`, `address`), ad esempio il journal locale insieme al SIEM remoto. |

Vedi [Log e SIEM](log-siem.md).

### `workdir`

Default `./work`. Directory locale in cui ogni deploy scarica l'artefatto (`workdir/<deployment-id>`). Non serve che sia condivisa tra i worker.

### `envconfig`

| Chiave | Default | Descrizione |
|---|---|---|
| `provider` | `none` | `none`, `consul` oppure `file`. |
| `required` | `false` | Fa fallire il deploy se per l'ambiente non esiste configurazione. |
| `max_retries` | `3` | Tentativi aggiuntivi di lettura. |
| `consul.address` | `CONSUL_HTTP_ADDR` o `http://127.0.0.1:8500` | Indirizzo dell'agent Consul. |
| `consul.datacenter` | | Datacenter Consul. |
| `consul.key` | `manent/{application}/{environment}` | Modello della chiave KV. |
| `consul.token_file` | | File con il token ACL (in alternativa a `CONSUL_HTTP_TOKEN`). |
| `file.path` | `./envconfig/{application}/{environment}.yml` | Modello del percorso per il provider `file`. |
| `file.tenants` | `./envconfig/tenants/{tenant}.yml` | Modello del percorso dei documenti dei [tenant](../guida/tenant.md). |
| `consul.tenants_key` | `manent/tenants/{tenant}` | Modello della chiave KV dei documenti dei tenant. |
| `tenants_cache` | `30s` | Per quanto resta in cache l'elenco dei tenant (selettori, permessi). |

Vedi [Configurazione di ambiente](../guida/configurazione-ambiente.md).

### `callbacks`

| Chiave | Default | Descrizione |
|---|---|---|
| `timeout` | `10s` | Timeout di ogni chiamata. |
| `max_retries` | `3` | Tentativi aggiuntivi di ogni invio. |
| `allow_http` | `false` | Ammette callback `http`. |
| `allowed_hosts` | `[]` | Host ammessi, anche `*.dominio`; vuoto = tutti. |

Vedi [Callback](../guida/callback.md).

### `rollback`

| Chiave | Default | Descrizione |
|---|---|---|
| `locator` | `plugin` | Strategia di default per individuare la release a cui tornare. |
| `artifact_types.<tipo>.allowed` | `application`, `container`, `kubernetes`, `ecs`, `lambda`, `cloudrun`: `true`; `migration`: `false` | Il rollback è possibile per il tipo. I tipi non elencati non lo ammettono. |
| `artifact_types.<tipo>.enabled` | `false` | Default per i deploy del tipo. Richiede `allowed`. |
| `artifact_types.<tipo>.on` | `[deploy, post-test, activation]` | Step il cui fallimento avvia il rollback. |
| `artifact_types.<tipo>.locator` | | Locator specifico del tipo. |

Vedi [Rollback](../guida/rollback.md).

### `auth`, `rbac`, `audit`

| Chiave | Default | Descrizione |
|---|---|---|
| `auth.mode` | `none` | `none`: nessuna autenticazione né permessi (solo sviluppo). `enabled`: token bearer obbligatorio e RBAC. |
| `auth.oidc` | | `issuer`, `audience`, `username_claim`, `groups_claim`, `ca_file`. |
| `auth.tokens` | `[]` | Token statici: `name`, `sha256` (mai il token), `groups`, `expires_at`. |
| `auth.tokens_file` | | File YAML con altri token (stesso formato, chiave `tokens`), riletto quando cambia: revoca senza riavvio. |
| `rbac.roles` | `{}` | Ruoli aggiuntivi: nome → elenco di azioni. |
| `rbac.bindings` | `[]` | `subjects` o `groups`, `role`, `applications`, `environments` (pattern con `*`), `tenants` (selettore: il binding vale solo per i deploy di quei [tenant](../guida/tenant.md#permessi)). |
| `audit.file` | | File JSON Lines delle voci di audit, con catena di hash, oltre al log e al SIEM. |
| `audit.max_size_mb` | `0` | Dimensione oltre cui il file viene ruotato (`0` = nessuna rotazione). |
| `audit.hmac_key_ref` | | Riferimento alla chiave (almeno 16 byte) con cui la catena diventa un HMAC. |

Vedi [Autenticazione, permessi e audit](accesso.md).

### `governance`

| Chiave | Default | Descrizione |
|---|---|---|
| `governance.engine` | `builtin` | `builtin`: le regole e le finestre di questo file. `opa`: policy Rego valutate da OPA, modificabili a runtime. |
| `governance.rules` | `[]` | Regole per applicazione e ambiente (vale la prima che corrisponde): `name`, `applications`, `environments`, `requires` (ambienti da cui promuovere), `approvals` (`required`, `allow_self`, `timeout`, default `72h`, `when`, `approvers` con `groups` e `subjects`), `change` (`approval`, `timeout`, default `72h`: ticket di change, vedi [Chat e change management](../guida/integrazioni.md#change-management)). Con `opa` sono i dati della policy predefinita. |
| `governance.freezes` | `[]` | Finestre di freeze: `name`, `reason`, `applications`, `environments` e `from`/`to` oppure `cron` (5 campi), `duration`, `timezone`, `hard`. Le finestre si possono creare anche a runtime (`manent freeze add`). |
| `governance.refresh` | `10s` | Ogni quanto ogni processo rilegge la policy e i freeze a runtime. |
| `governance.max_staleness` | `10m` | Oltre questo tempo senza una lettura riuscita, i deploy reali sono rifiutati (`503`). |
| `governance.start` | `deny` | Cosa ricontrolla l'avvio del deploy: `deny` (divieti e freeze) o `strict` (anche approvazioni e change aumentati dopo l'accodamento). |
| `governance.opa.source` | `store` | Revisioni: `store` (gestite con `manent policy`), `file`, `bundle`, `consul`. |
| `governance.opa.store.review` | `false` | Una bozza viene approvata da un'identità diversa dal suo autore. |
| `governance.opa.store.tests` | `run` | `run`: i test della revisione girano se presenti; `required`: le revisioni senza test sono rifiutate. |
| `governance.opa.file.dir` | | Directory della policy con `source: file`. |
| `governance.opa.bundle.*` | | `url`, `ca_file`, `credentials_ref`, `signing` (`public_key_ref`, `algorithm`, `key_id`, `scope`), `insecure_skip_verify`. |
| `governance.opa.consul.prefix` | `manent/policies` | Prefisso del KV con `source: consul` (indirizzo e token di `envconfig.consul`). |
| `governance.opa.query` | `data.manent.governance.decision` | Query della decisione. |
| `governance.opa.eval_timeout` | `200ms` | Limite di una valutazione. |

Vedi [Governance dei rilasci](../guida/governance.md) e [Policy in Rego](../guida/policy.md).

### `descriptor_policy`

| Chiave | Default | Descrizione |
|---|---|---|
| `params` | `{"*": ["*"]}` | Parametri impostabili dal descrittore, per plugin. |
| `secret_refs` | `["*"]` | Riferimenti ai segreti dichiarabili dal descrittore. |
| `targets` | `true` | Target indicati dal descrittore. |
| `step_params` | `{}` | Parametri degli [step personalizzati](../guida/step-personalizzati.md) impostabili dal descrittore, per step (`"*"` = tutti). Default: nessuno. |

Vedi [Sicurezza](sicurezza.md#cosa-puo-chiedere-un-descrittore).

### `secrets` e `artifacts`

| Chiave | Default | Descrizione |
|---|---|---|
| `secrets.vault.enabled` | `false` | Abilita lo schema `vault:`. |
| `secrets.vault.address`, `namespace`, `auth`, `token_file`, `approle`, `kubernetes`, `ca_file`, `timeout` | | Connessione e login a Vault. |
| `artifacts.timeout` | `30m` | Durata massima di un download. |
| `artifacts.allow_unlisted` | `true` | Ammette sorgenti non elencate in `artifacts.sources`. |
| `artifacts.sources` | `[]` | `prefix`, `auth` (`type`, `username`, `header`, `secret_ref`), `ca_file`. |

Vedi [Segreti e sorgenti degli artefatti](segreti-artefatti.md).

### `pipeline`

Elenco ordinato degli step, con `name`, `mode` (`mandatory` oppure `optional`), `enabled` (default degli step opzionali) e `max_retries`. Vedi [Pipeline e step](../concetti/pipeline.md).

Uno step con `type` (`command` o `http`) è uno [step personalizzato](../guida/step-personalizzati.md), definito da `timeout`, `secrets`, `params`, `outputs`, `ssh`, `run` e `dry_run`. Si configurano in `pipeline.custom`:

| Chiave | Default | Descrizione |
|---|---|---|
| `pipeline.custom.spec_key_refs` | `[]` | Chiavi HMAC (almeno 32 byte) che firmano le specifiche registrate nei piani; la prima firma, tutte verificano. |
| `pipeline.custom.revoked_specs` | `[]` | SHA-256 delle specifiche da non eseguire più. |
| `pipeline.custom.known_hosts` | `~/.ssh/known_hosts` | Host key dei target per gli step command. |
| `pipeline.custom.insecure_ignore_host_key` | `false` | Salta la verifica della host key: solo per lo sviluppo. |
| `pipeline.custom.secrets_dir` | `/dev/shm/manent` | Directory dei segreti sui target. |
| `pipeline.custom.allow_http` | `false` | Ammette URL `http` negli step http. |

### `plugins`

Plugin da attivare, ciascuno con la propria configurazione:

```yaml
plugins:
  remote-copy:                       # plugin compilato dentro manent
    known_hosts: ~/.ssh/known_hosts
    artifact_types: [application]
  k8s-deploy:                        # plugin esterno
    external:
      command: /opt/manent/plugins/k8s-deploy
      sha256: "3f1c…"
    artifact_types: [container]
```

Plugin compilati in manent: [remote-copy](../plugins/remote-copy.md), [container](../plugins/container.md), [kubernetes](../plugins/kubernetes.md), [ecs](../plugins/ecs.md), [lambda](../plugins/lambda.md), [cloudrun](../plugins/cloudrun.md), [jboss](../plugins/jboss.md), [windows](../plugins/windows.md). Vedi anche [Plugin esterni](../plugins/esterni.md).

### `rollout`

Health gate dei [rilasci a ondate](../guida/ondate.md#health-gate).

| Chiave | Default | Descrizione |
|---|---|---|
| `prometheus_url` | | API di Prometheus per `rollout.health.metrics` (es. `http://prometheus:9090`). Senza, un rollout con metriche Prometheus è rifiutato; lo stesso vale per gli altri sistemi. |
| `alertmanager_url` | | API di Alertmanager per `rollout.health.alerts`. |
| `datadog` | | Datadog per le metriche con `provider: datadog`: `url` (sito dell'API, default `https://api.datadoghq.com`), `api_key_ref`, `app_key_ref`. |
| `newrelic` | | New Relic NerdGraph: `url` (default `https://api.newrelic.com/graphql`), `account_id`, `api_key_ref` (chiave utente). |
| `elastic` | | Elasticsearch ES\|QL: `url`, `api_key_ref` oppure `username` e `password_ref`, `ca_file`. |
| `cloudwatch` | | Amazon CloudWatch: `region`, `endpoint` (facoltativo), credenziali dalla catena dell'SDK o da `access_key_id_ref` e `secret_access_key_ref`, `role_arn` ed `external_id` per assumere un ruolo. |
| `azure_monitor` | | Azure Monitor Logs (`provider: azure-monitor`): `workspace_id`, un service principal con `tenant_id`, `client_id` e `client_secret_ref` oppure, senza, l'identità del worker (workload o managed identity); `endpoint` e `authority` per i cloud sovrani. |
| `check_timeout` | `30s` | Tempo massimo di ogni lettura del gate. |

### `integrations`

Notifiche in Slack e Teams, approvazioni da Slack e ticket di change. Vedi [Chat e change management](../guida/integrazioni.md).

| Chiave | Default | Descrizione |
|---|---|---|
| `integrations.public_url` | | Indirizzo dell'interfaccia web, per i link nei messaggi e nei ticket. |
| `integrations.language` | `en` | Lingua dei messaggi: `en` o `it`. |
| `integrations.timeout` | `15s` | Timeout delle chiamate a Slack, Teams e ITSM. |
| `integrations.slack.bot_token_ref`, `signing_secret_ref` | | Segreti dell'app Slack: messaggi aggiornabili e pulsanti di approvazione. |
| `integrations.slack.identities` | | ID utente Slack → `subject` (`oidc:alice`) e `groups` dell'identità di manent. |
| `integrations.slack.email_identity` | | Identità degli utenti non elencati dalla loro email, es. `oidc:{email}`. |
| `integrations.notifications` | `[]` | Destinazioni: `name`, `applications`, `environments`, `events`, `dry_run` e `slack` (`channel` o `webhook_ref`) oppure `teams` (`webhook_ref`). |
| `integrations.change.provider` | | `servicenow` o `jira`. |
| `integrations.change.poll_interval` | `1m` | Intervallo iniziale di lettura dell'approvazione del change (fino a 5 minuti). |
| `integrations.change.servicenow`, `jira` | | Connessione e campi del sistema ITSM. |

### `chains`

[Catene di deploy](../guida/catene.md): dipendenze tra deploy e release composte.

| Chiave | Default | Descrizione |
|---|---|---|
| `poll_interval` | `5s` | Primo intervallo con cui un deploy in attesa o una release rileggono lo stato dei deploy attesi; raddoppia a ogni lettura fino a 5 minuti. Di norma la lettura avviene prima, perché chi si conclude avvisa chi lo attende. |

### `observability`

| Chiave | Default | Descrizione |
|---|---|---|
| `listen` | | Indirizzo di un server con `/metrics`, `/healthz` e `/readyz` (es. `:9090`). Serve ai worker, che non hanno l'API. |
| `metrics` | `true` | Metriche Prometheus. |
| `tracing.enabled` | `false` | Esporta le tracce OpenTelemetry via OTLP/HTTP. |
| `tracing.endpoint` | | Collector, es. `otel-collector:4318`; vuoto: `OTEL_EXPORTER_OTLP_ENDPOINT`. |
| `tracing.insecure` | `false` | Usa `http` invece di `https` (endpoint senza schema). |
| `tracing.sample_ratio` | `1` | Frazione delle tracce campionate, se la richiesta non porta già una decisione. |

Vedi [Osservabilità](osservabilita.md).

### `recovery`

| Chiave | Default | Descrizione |
|---|---|---|
| `heartbeat_interval` | `15s` | Ogni quanto un worker aggiorna il registro. |
| `stale_after` | `1m` | Ritardo di heartbeat oltre il quale un worker è considerato perso (almeno il doppio di `heartbeat_interval`). |
| `auto_recover_after` | `0s` | Se maggiore di zero, i workflow in sospeso di un worker della stessa versione arrestato o senza heartbeat da questo tempo vengono ripresi da un altro worker. Almeno `stale_after`. |

Vedi [Recovery, arresto e retention](affidabilita.md).

### `retention`

Come tutte le durate della configurazione, i valori si scrivono con l'unità (`0s`, `10m`, `2160h`): un numero senza unità non è valido.

| Chiave | Default | Descrizione |
|---|---|---|
| `max_age` | `0s` | I workflow conclusi da più di questo tempo vengono eliminati (`0s` = mai; altrimenti almeno `24h`). |
| `keep_releases` | `10` | Deploy riusciti conservati comunque per applicazione e ambiente. |
| `interval` | `1h` | Ogni quanto un worker esegue la pulizia. |

### `reconcile` { #reconcile }

Lettura periodica della release in esercizio sui target, per segnalare nell'inventario quelli cambiati fuori da manent (vedi [stato reale dei target](../guida/governance.md#stato-reale)).

| Chiave | Default | Descrizione |
|---|---|---|
| `interval` | `0s` | Ogni quanto i worker leggono i target di ogni ambiente (`0s` = disattivata; altrimenti almeno `10s`). |
| `timeout` | `30s` | Tempo massimo della lettura di un target. |
| `concurrency` | `8` | Target di un ambiente letti insieme. |

## Variabili d'ambiente

| Variabile | Effetto |
|---|---|
| `MANENT_CONFIG` | File di configurazione, in alternativa a `-c`. |
| `DBOS_SYSTEM_DATABASE_URL` | Sovrascrive `dbos.database_url`. |
| `DBOS__VMID` | Sovrascrive `dbos.executor_id`. |
| `CONSUL_HTTP_ADDR` | Indirizzo Consul, se `envconfig.consul.address` è vuoto. |
| `CONSUL_HTTP_TOKEN` | Token ACL Consul. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Collector delle tracce, se `observability.tracing.endpoint` è vuoto. Valgono anche le altre variabili `OTEL_EXPORTER_OTLP_*` (intestazioni, certificati). |

I segreti con riferimento `env:NOME` sono letti dalle variabili d'ambiente del worker.
