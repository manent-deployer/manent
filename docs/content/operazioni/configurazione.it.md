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
| `siem.enabled` | `false` | Inoltra ogni log al SIEM in RFC 5424. |
| `siem.network` | `udp` | `udp` oppure `tcp`. |
| `siem.address` | | `host:porta` del collettore. |

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
| `artifact_types.<tipo>.allowed` | `application`: `true`, `migration`: `false` | Il rollback è possibile per il tipo. I tipi non elencati non lo ammettono. |
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
| `rbac.bindings` | `[]` | `subjects` o `groups`, `role`, `applications`, `environments` (pattern con `*`). |
| `audit.file` | | File JSON Lines delle voci di audit, con catena di hash, oltre al log e al SIEM. |
| `audit.max_size_mb` | `0` | Dimensione oltre cui il file viene ruotato (`0` = nessuna rotazione). |
| `audit.hmac_key_ref` | | Riferimento alla chiave (almeno 16 byte) con cui la catena diventa un HMAC. |

Vedi [Autenticazione, permessi e audit](accesso.md).

### `governance`

| Chiave | Default | Descrizione |
|---|---|---|
| `governance.rules` | `[]` | Regole per applicazione e ambiente (vale la prima che corrisponde): `name`, `applications`, `environments`, `requires` (ambienti da cui promuovere), `approvals` (`required`, `allow_self`, `timeout`, default `72h`). |
| `governance.freezes` | `[]` | Finestre di freeze: `name`, `reason`, `applications`, `environments` e `from`/`to` oppure `cron` (5 campi), `duration`, `timezone`. |

Vedi [Governance dei rilasci](../guida/governance.md).

### `descriptor_policy`

| Chiave | Default | Descrizione |
|---|---|---|
| `params` | `{"*": ["*"]}` | Parametri impostabili dal descrittore, per plugin. |
| `secret_refs` | `["*"]` | Riferimenti ai segreti dichiarabili dal descrittore. |
| `targets` | `true` | Target indicati dal descrittore. |

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

Vedi [remote-copy](../plugins/remote-copy.md) e [Plugin esterni](../plugins/esterni.md).

### `rollout`

Health gate dei [rilasci a ondate](../guida/ondate.md#health-gate).

| Chiave | Default | Descrizione |
|---|---|---|
| `prometheus_url` | | API di Prometheus per `rollout.health.metrics` (es. `http://prometheus:9090`). Senza, un rollout con metriche è rifiutato. |
| `alertmanager_url` | | API di Alertmanager per `rollout.health.alerts`. |
| `check_timeout` | `30s` | Tempo massimo di ogni lettura del gate. |

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
