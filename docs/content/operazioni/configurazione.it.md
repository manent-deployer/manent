# Configurazione di manent

manent legge un file YAML indicato con `-c` (o con la variabile `MANENT_CONFIG`). Ogni valore non indicato prende il default riportato sotto; le chiavi sconosciute sono un errore. Il file d'esempio completo è `configs/manent.yml`.

## Cosa usa ogni processo { #processes }

Ogni [processo di `manent-server`](server.md) carica e verifica l'intero file e costruisce all'avvio tutti i suoi componenti. La scelta più semplice è un solo file condiviso da API, worker e operatori, in cui cambiano solo le chiavi del singolo processo (vedi sotto). I processi differiscono per le sezioni che usano davvero:

- `api` accetta e verifica le richieste, calcola il piano e lo accoda; non esegue mai un deploy.
- `worker` esegue i deploy che preleva; non ha API, quindi non autentica nessuno.
- `serve` è entrambi in un solo processo e usa l'unione delle due colonne.
- `admin` lavora sul database senza passare dall'API (vedi [Operazioni di piattaforma](server.md#operazioni-di-piattaforma)).

| Sezione | `api` | `worker` | `admin` |
|---|---|---|---|
| [`app`](#app) | `name` | ✓ | ✓ (`version` è il default di `recover --to-version`) |
| [`dbos`](#dbos) | `database_url`, `queue` e i limiti di concorrenza (mostrati da `manent queues`) | ✓ | `database_url` |
| [`http`](#http) | ✓ | — | — |
| [`logging` e `siem`](#logging-e-siem) | ✓ | ✓ | ✓ |
| [`workdir`](#workdir) | solo come `cache_dir` di default del provider `git` | ✓ | — |
| [`tenants`](#tenants) | ✓ | ✓ | — |
| [`envconfig`](#envconfig) | ✓ (verifica all'accodamento, `env validate`, import) | ✓ (step `config`) | — |
| [`callbacks`](#callbacks) | `allow_http`, `allowed_hosts` | ✓ | — |
| [`rollback`](#rollback) | ✓ | ✓ | — |
| [`auth`, `rbac`](#auth-rbac-audit) | ✓ | — (i permessi sono verificati all'accodamento) | — (identità `cli:<utente>`, nessun permesso) |
| [`audit`](#auth-rbac-audit) | ✓ (azioni e divieti) | ✓ (esito dei deploy, esecuzioni degli step personalizzati) | ✓ (`audit verify`, azioni break-glass) |
| [`governance`](#governance) | ✓ | ✓ (controlli all'avvio, freeze durante il rollout, tick delle schedule) | `policy-activate`, `freeze-delete` |
| [`descriptor_policy`](#descriptor_policy) | ✓ | ✓ | — |
| [`secrets`](#secrets-e-artifacts) | ✓ | ✓ | ✓ (riferimenti risolti all'avvio) |
| [`artifacts`](#secrets-e-artifacts) | ✓ (allowlist, `--check-artifact`) | ✓ (download) | — |
| [`pipeline`](#pipeline) | `steps`, `custom.spec_key_refs` (piano e firma degli step personalizzati) | ✓ | `custom.revoked_specs` (`admin specs`) |
| [`plugins`](#plugins) | `artifact_types`, `queue`: scelta del plugin e instradamento; i plugin senza coda propria vengono anche attivati con la loro configurazione | ✓ (un plugin con coda propria solo se elencato in `dbos.plugins`, vedi [code dei plugin](scalabilita.md#plugin-queues)) | — |
| [`queues`](#queues) | ✓ (instradamento) | le code elencate in `dbos.queues` | — |
| [`rollout`](#rollout) | ✓ (un rollout che legge un sistema non configurato viene rifiutato) | ✓ (letture del gate) | — |
| [`integrations`](#integrations) | `notifications` (destinazioni scelte all'accodamento), `slack.signing_secret_ref`, `identities`, `email_identity` (approvazioni da Slack) | ✓ (invio dei messaggi, ticket di change) | — |
| [`chains`](#chains) | — | ✓ | — |
| [`observability`](#observability) | ✓ (`/metrics` anche sulla porta dell'API) | ✓ (`listen` è l'unico modo di esporre sonde e metriche) | — |
| [`recovery`](#recovery) | `stale_after` (`manent workers`) | ✓ | `stale_after` (`admin executors`) |
| [`retention`](#retention) | — | ✓ | `max_age`, `keep_releases` (`admin prune`) |
| [`reconcile`](#reconcile) | — | ✓ | — |

**Valori che devono essere uguali ovunque.** L'API verifica una richiesta con la propria configurazione e il worker la verifica di nuovo con la sua: valori diversi danno deploy accettati e poi rifiutati, o accodati dove nessuno li preleva.

- `app.name` e `dbos.database_url`: l'applicazione DBOS e il suo database.
- `dbos.queue`, `queues`, `plugins.<nome>.queue` e `artifact_types`: l'API sceglie la coda, il worker deve servirla.
- `pipeline.steps` e `pipeline.custom.spec_key_refs`: l'API firma gli step personalizzati del piano, il worker verifica la firma. Per ruotare una chiave, prima la si aggiunge a tutti i processi, poi la si mette per prima.
- `tenants`, `envconfig`, `descriptor_policy`, `governance`, `rollback`, `callbacks.allow_http`/`allowed_hosts`, i sistemi di `rollout`.

**Valori del singolo processo.**

- Worker: `dbos.executor_id` (univoco e stabile), `dbos.region`, `dbos.tenants`, `dbos.plugins`, `dbos.queues`, `dbos.exclusive`, `dbos.concurrency`, `dbos.drain_timeout`, `workdir`.
- API: `http.listen` (o `serve --listen`).
- Tutti: `observability.listen` e `audit.file`, un file per processo: due processi sullo stesso disco richiedono percorsi diversi.

I segreti riferiti da `pipeline.custom.spec_key_refs` e `audit.hmac_key_ref` vengono letti all'avvio: ogni processo che ha la chiave nella configurazione, `admin` compreso, deve poterli risolvere.

## Riferimento

### `app`

| Chiave | Default | Descrizione |
|---|---|---|
| `name` | `manent` | Nome dell'applicazione DBOS e `APP-NAME` dei messaggi inviati al SIEM. |
| `version` | `0.2.0` | Versione dei workflow registrata da DBOS: un worker riprende solo i workflow della propria versione. Non è la versione del binario e va cambiata solo quando le note di rilascio lo indicano. Vedi [Versioni e aggiornamenti](aggiornamenti.md). |

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
| `tenants` | `[]` | Limita il worker alle code dei deploy di questi [tenant](../guida/tenant.md#code-e-worker-per-tenant) (la coda di default per quelli che non ne hanno). Vuoto: tutte le code del registro. |
| `plugins` | `[]` | Plugin con una coda propria (`plugins.<nome>.queue`) serviti da questo worker: ne registra e preleva le code oltre alla propria. Vedi [code dei plugin](scalabilita.md#plugin-queues). |
| `queues` | `[]` | Code della sezione [`queues`](#queues) servite da questo worker: le registra e le preleva oltre alla propria. Vedi [code instradate](scalabilita.md#routed-queues). |
| `exclusive` | `false` | Il worker preleva solo le code di `queues` e `plugins` (e le approvazioni), non quella di default né quelle dei tenant: un gruppo riservato a quei deploy. Richiede `queues` o `plugins`; non con `tenants`. |
| `drain_timeout` | `2m` | All'arresto il worker smette di prelevare deploy e attende quelli in corso fino a questo tempo (`0` = non attende). Vedi [drain](affidabilita.md#arresto-ordinato-drain). |

### `http`

| Chiave | Default | Descrizione |
|---|---|---|
| `listen` | `:8080` | Indirizzo dell'API (sovrascrivibile con `serve --listen`). |
| `ui.enabled` | `true` | Serve l'[interfaccia web](../guida/interfaccia-web.md) su `/`. |
| `trusted_proxies` | `[]` | Indirizzi o CIDR dei reverse proxy il cui `X-Forwarded-For` dà l'indirizzo del client in log e audit. Vuoto: l'intestazione è ignorata, così nessuno può falsificare l'indirizzo. |

### `logging` e `siem`

| Chiave | Default | Descrizione |
|---|---|---|
| `logging.level` | `info` | `debug`, `info`, `warn`, `error`. |
| `logging.format` | `text` | `text` oppure `json`. |
| `siem.enabled` | `false` | Inoltra ogni log alle destinazioni dei log strutturati. |
| `siem.destinations` | `[]` | Le destinazioni, almeno una con `enabled`; ognuna riceve ogni messaggio. |
| `siem.destinations[].network` | | `udp`, `tcp`, `unixgram` (socket syslog locale) o `journald` (protocollo nativo). |
| `siem.destinations[].address` | | `host:porta` del collettore; per `unixgram` e `journald` il percorso del socket (default `/dev/log` e `/run/systemd/journal/socket`). |

Vedi [Log e SIEM](log-siem.md).

### `workdir`

Default `./work`. Directory locale in cui ogni deploy scarica l'artefatto (`workdir/<deployment-id>`). Non serve che sia condivisa tra i worker.

### `tenants`

| Chiave | Default | Descrizione |
|---|---|---|
| `file` | | Il [registro dei tenant](../guida/tenant.md) (`tenants.yml`). Vuoto: nessun registro, ogni applicazione è `untenanted`. |
| `cache` | `30s` | Per quanto si tiene il registro prima di rileggerlo. |
| `untenanted` | `allow` | Le applicazioni che nessun tenant elenca: `allow` le rilascia come tenant `untenanted`, `reject` le rifiuta ([dettagli](../guida/tenant.md#il-tenant-e-necessario-per-rilasciare)). |

### `envconfig`

| Chiave | Default | Descrizione |
|---|---|---|
| `providers` | `[]` | I provider con nome, nessuno = nessuna configurazione di ambiente. Un deploy legge prima il provider che il suo [tenant](../guida/tenant.md) indica nel registro, poi quelli il cui `match` corrisponde, in ordine ([Più provider](../guida/configurazione-ambiente.md#piu-provider)). |
| `providers[].name` | | Obbligatorio e univoco: lettere, cifre, `.`, `_`, `:`, `-`, al più 128 caratteri, il primo una lettera o una cifra (le regole di `artifact.name`). Compare come `config_provider` nello stato del deploy e i tenant lo indicano nel registro (`envconfig`). |
| `providers[].type` | | Obbligatorio: `consul`, `file` o `git`, configurato dalla sezione omonima qui sotto. |
| `providers[].match.applications` | `[]` | Pattern delle applicazioni qualificati con il tenant (`payments/*`, `*/*`; senza `/` solo untenanted). Vuoto: tutte. |
| `providers[].match.artifacts` | `[]` | Pattern su `artifact.name`. Vuoto: tutti. |
| `providers[].match.environments` | `[]` | Pattern degli ambienti. Vuoto: tutti. |
| `providers[].tenant_only` | `false` | Il provider serve solo i tenant che lo indicano nel registro, mai tramite `match`. Alternativo a `match`. |
| `providers[].on_missing` | `fail` | Quando il provider non ha la configurazione della release: `fail` (il deploy non ha configurazione di ambiente) o `next` (si legge il provider successivo che corrisponde). Ogni altro errore ferma la lettura. |
| `required` | `false` | Fa fallire il deploy se per l'ambiente non esiste configurazione. |
| `max_retries` | `3` | Tentativi aggiuntivi di lettura. |
| `providers[].consul.address` | `CONSUL_HTTP_ADDR` o `http://127.0.0.1:8500` | Indirizzo dell'agent Consul. |
| `providers[].consul.datacenter` | | Datacenter Consul. |
| `providers[].consul.key` | `manent/{tenant}/{application}/{artifact}/{environment}` | Modello della chiave KV. |
| `providers[].consul.token_file` | | File con il token ACL (in alternativa a `CONSUL_HTTP_TOKEN`). |
| `providers[].file.path` | `./envconfig/{tenant}/{application}/{artifact}/{environment}.yml` | Modello del percorso per il provider `file`. |
| `providers[].git.repository` | | Repository Git delle configurazioni (`https://`, `ssh://`, `git://`, `file://` o un percorso). Vedi [Repository Git](../integrazioni/git.md). |
| `providers[].git.ref` | `refs/heads/main` | Ref di default. |
| `providers[].git.path` | `{tenant}/{application}/{artifact}/{environment}.yml` | Modello del file nel repository. |
| `providers[].git.allowed_refs` | `[]` | Altri ref (pattern con `*`) che `config.ref` può indicare; uno SHA deve essere raggiungibile da essi o da `providers[].git.ref`. |
| `providers[].git.username`, `providers[].git.password_ref` | | Credenziali HTTPS (la password è un riferimento a un segreto). |
| `providers[].git.ssh_key_ref`, `providers[].git.known_hosts` | | Chiave SSH (riferimento a un segreto) e file dei known hosts, obbligatori insieme. |
| `providers[].git.ca_file` | | CA aggiuntive per HTTPS. |
| `providers[].git.require_signed` | `false` | Rifiuta i commit senza una firma valida di `providers[].git.signers`. |
| `providers[].git.signers.openpgp_keyring` | | Keyring pubblico OpenPGP in formato armored. |
| `providers[].git.signers.ssh_allowed_signers` | | File `allowed_signers` nel formato di `ssh-keygen`, per le firme SSH. |
| `providers[].git.max_age` | `0` | Età massima di un commit (`0` = nessuna); i rollback manuali ne sono esenti. |
| `providers[].git.cache_dir` | `<workdir>/.git-cache` | Copie locali del repository. |
| `providers[].git.ref_ttl` | `30s` | Per quanto si riusa la risoluzione di un ref prima di un nuovo fetch. |

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
| `artifact_types.<tipo>.allowed` | `application`, `container`, `kubernetes`, `ecs`, `lambda`, `cloudrun`, `war`, `ear`, `windows`, `appservice`, `containerapps`: `true`; `migration`: `false` | Il rollback è possibile per il tipo. I tipi non elencati non lo ammettono. |
| `artifact_types.<tipo>.enabled` | `false` | Default per i deploy del tipo. Richiede `allowed`. |
| `artifact_types.<tipo>.on` | `[deploy, post-test, activation]` | Step il cui fallimento avvia il rollback. |
| `artifact_types.<tipo>.locator` | | Locator specifico del tipo. |

I tipi scritti nel file si aggiungono a quelli di default, ma ogni voce sostituisce per intero il default del suo tipo: per abilitare il rollback di `application` si scrive `application: {allowed: true, enabled: true}`, non solo `enabled`.

Vedi [Rollback](../guida/rollback.md).

### `auth`, `rbac`, `audit`

| Chiave | Default | Descrizione |
|---|---|---|
| `auth.mode` | `none` | `none`: nessuna autenticazione né permessi (solo sviluppo). `enabled`: token bearer obbligatorio e RBAC. |
| `auth.oidc.issuer` | | Issuer OIDC: la discovery avviene all'avvio dell'API. |
| `auth.oidc.audience` | | Audience che i token di accesso devono avere. |
| `auth.oidc.username_claim` | | Obbligatorio: il claim che dà il nome all'identità (`oidc:<valore>`). Un token che non lo ha viene rifiutato, `sub` non lo sostituisce mai. |
| `auth.oidc.groups_claim` | | Claim con i gruppi usati dai binding. |
| `auth.oidc.ca_file` | | Bundle di CA aggiuntive per raggiungere l'issuer. |
| `auth.oidc.policy_claims` | `[email, name]` | Claim esposti alla [policy dei permessi](../guida/permessi.md); gli altri restano nascosti. |
| `auth.oidc.ui.client_id` | | Client OIDC pubblico (Authorization Code + PKCE) dell'[interfaccia web](../guida/interfaccia-web.md). Senza `ui` l'interfaccia non può fare il login. I suoi token devono avere l'audience e il claim dei gruppi indicati sopra. |
| `auth.oidc.ui.scopes` | `[openid, profile]` | Scope richiesti dall'interfaccia. |
| `auth.oidc.ui.redirect_path` | `/auth/callback` | Percorso di ritorno dopo il login. |
| `auth.tokens` | `[]` | Token statici per i sistemi automatici. |
| `auth.tokens[].name` | | Nome dell'identità (`token:<nome>`). |
| `auth.tokens[].sha256` | | SHA-256 del token, mai il token. |
| `auth.tokens[].groups` | `[]` | Gruppi usati dai binding. |
| `auth.tokens[].expires_at` | | Scadenza (RFC 3339); vuoto: nessuna scadenza. |
| `auth.tokens_file` | | File YAML con altri token (stesso formato, chiave `tokens`), riletto quando cambia: revoca senza riavvio. |
| `rbac.engine` | `builtin` | `builtin` (i binding qui sotto) o `opa` (la [policy dei permessi](../guida/permessi.md) delle revisioni; richiede `governance.engine: opa`). Le `policy.*` arrivano sempre da `rbac.bindings`. |
| `rbac.opa.cache` | `30s` | Per quanto si riusano i permessi di un'identità con `opa`. |
| `rbac.opa.cache_size` | `10000` | Identità nella cache dei permessi. |
| `rbac.roles` | `{}` | Ruoli aggiuntivi: nome → elenco di azioni. Quelli predefiniti sono `viewer`, `tester`, `deployer`, `operator`, `admin`. |
| `rbac.bindings` | `[]` | Ruoli assegnati alle identità su applicazioni e ambienti. |
| `rbac.bindings[].subjects`, `groups` | `[]` | Chi riceve il ruolo: identità (`oidc:alice`, `token:ci`) o gruppi. |
| `rbac.bindings[].role` | | Ruolo predefinito o uno di `rbac.roles`. |
| `rbac.bindings[].applications` | `[]` | Pattern qualificati: `shop` solo untenanted, `payments/*`, `*/*` ogni [tenant](../guida/tenant.md#pattern-delle-applicazioni-qualificati). |
| `rbac.bindings[].environments` | `[]` | Pattern degli ambienti con `*`. |
| `rbac.bindings[].artifacts` | `[]` | Limita il binding ad alcuni artefatti (pattern su `artifact.name`). Con questo filtro il binding non vale per le azioni sull'intero ambiente. |
| `rbac.bindings[].tenants` | | Selettore sul tenant proprietario (i suoi tag e `name`). |
| `rbac.bindings[].selector` | | Selettore sui tag dell'applicazione (chiavi limitate da `rbac.selector_keys`). |
| `rbac.tenant_roles` | `{members: deployer, admins: admin}` | Ruoli impliciti di `members` e `admins` di un [tenant](../guida/tenant.md) sulle sue applicazioni. Mai `policy.*`. |
| `rbac.selector_keys` | `[]` | Chiavi che il `selector` di binding e grant può leggere (vuoto: nessun selettore nei permessi). |
| `rbac.cross_tenant_inventory` | `true` | Chi appartiene a un tenant legge l'inventario di tutti i tenant. |
| `audit.file` | | File JSON Lines delle voci di audit, con catena di hash, oltre al log e al SIEM. |
| `audit.max_size_mb` | `0` | Dimensione oltre cui il file viene ruotato (`0` = nessuna rotazione). |
| `audit.hmac_key_ref` | | Riferimento alla chiave (almeno 16 byte) con cui la catena diventa un HMAC. |

Vedi [Autenticazione, permessi e audit](accesso.md).

### `governance`

| Chiave | Default | Descrizione |
|---|---|---|
| `governance.engine` | `builtin` | `builtin`: le regole e le finestre di questo file. `opa`: policy Rego valutate da OPA, modificabili a runtime. |
| `governance.rules` | `[]` | Regole per applicazione e ambiente: vale la prima che corrisponde. Con `opa` sono i dati della policy predefinita. |
| `governance.rules[].name` | | Nome della regola, riportato nelle decisioni e nell'audit. |
| `governance.rules[].applications`, `environments` | `[]` | Pattern qualificati delle applicazioni e pattern degli ambienti. |
| `governance.rules[].artifacts` | `[]` | Limita la regola ad alcuni artefatti (pattern su `artifact.name`). |
| `governance.rules[].tenants` | | Selettore sul tenant proprietario. |
| `governance.rules[].selector` | | Selettore sui tag dell'applicazione. |
| `governance.rules[].requires` | `[]` | Ambienti in cui lo stesso artefatto deve essere già stato rilasciato (promozione). |
| `governance.rules[].approvals.required` | `0` | Approvazioni necessarie prima del deploy. |
| `governance.rules[].approvals.allow_self` | `false` | Chi richiede il deploy può approvarlo. |
| `governance.rules[].approvals.timeout` | `72h` | Oltre questo tempo la richiesta scade. |
| `governance.rules[].approvals.when` | `before` | `before`, prima del deploy, oppure `rollout`: nei punti di approvazione del rollout. |
| `governance.rules[].approvals.approvers.groups`, `subjects` | `[]` | Chi può approvare (pattern con `*`); vuoto: chiunque abbia il permesso. |
| `governance.rules[].change.approval` | `false` | Apre un ticket di change per ogni deploy e attende che venga approvato nell'ITSM (vedi [Chat e change management](../guida/integrazioni.md#change-management)). |
| `governance.rules[].change.timeout` | `72h` | Limite dell'attesa dell'approvazione del change. |
| `governance.freezes` | `[]` | Finestre di freeze. Le finestre si possono creare anche a runtime (`manent freeze add`). |
| `governance.freezes[].name`, `reason` | | Nome e motivo mostrati a chi viene bloccato. |
| `governance.freezes[].applications`, `environments` | `[]` | Pattern qualificati delle applicazioni e pattern degli ambienti. |
| `governance.freezes[].artifacts` | `[]` | Limita la finestra ad alcuni artefatti. |
| `governance.freezes[].tenants`, `selector` | | Selettori sul tenant proprietario e sui tag dell'applicazione. |
| `governance.freezes[].regions` | `[]` | Limita la finestra ai passi dei [rilasci a ondate](../guida/ondate.md#approvazioni-e-freeze) in queste region: non blocca l'accodamento, il rollout si ferma prima della region. |
| `governance.freezes[].from`, `to` | | Finestra assoluta (RFC 3339). |
| `governance.freezes[].cron`, `duration`, `timezone` | | Finestra ricorrente: inizio come cron a 5 campi, durata e fuso orario. Alternativa a `from`/`to`. |
| `governance.freezes[].hard` | `false` | La finestra non può essere esentata dalla policy né superata con `freezeOverride`. |
| `governance.refresh` | `10s` | Ogni quanto ogni processo rilegge la policy e i freeze a runtime. |
| `governance.max_staleness` | `10m` | Oltre questo tempo senza una lettura riuscita, i deploy reali sono rifiutati (`503`). |
| `governance.start` | `deny` | Cosa ricontrolla l'avvio del deploy: `deny` (divieti e freeze) o `strict` (anche approvazioni e change aumentati dopo l'accodamento). |
| `governance.opa.source` | `store` | Revisioni: `store` (gestite con `manent policy`), `file`, `bundle`, `consul`. |
| `governance.opa.store.review` | `false` | Una bozza viene approvata da un'identità diversa dal suo autore. |
| `governance.opa.store.tests` | `run` | `run`: i test della revisione girano se presenti; `required`: le revisioni senza test sono rifiutate. |
| `governance.opa.file.dir` | | Directory della policy con `source: file`. |
| `governance.opa.bundle.*` | | `url`, `ca_file`, `credentials_ref`, `signing` (`public_key_ref`, `algorithm`, `key_id`, `scope`), `insecure_skip_verify`. |
| `governance.opa.consul.prefix` | `manent/policies` | Prefisso del KV con `source: consul` (indirizzo e token di `governance.opa.consul.address`/`token_file`, altrimenti del primo provider `consul` di `envconfig.providers`). |
| `governance.opa.consul.address`, `datacenter`, `token_file` | | Agent Consul delle policy; vuoti: quello del primo provider `consul` di `envconfig.providers`. |
| `governance.opa.query` | `data.manent.governance.decision` | Query della decisione. |
| `governance.opa.eval_timeout` | `200ms` | Limite di una valutazione. |

Vedi [Governance dei rilasci](../guida/governance.md) e [Policy in Rego](../guida/policy.md).

### `descriptor_policy`

| Chiave | Default | Descrizione |
|---|---|---|
| `params` | `{"*": ["*"]}` | Parametri impostabili dal descrittore, per plugin. |
| `secret_refs` | `["*"]` | Riferimenti ai segreti dichiarabili dal descrittore. |
| `targets` | `true` | Target indicati dal descrittore. |
| `post_test_hosts` | `["*"]` | Host raggiungibili dai controlli `postTest.http` del descrittore (pattern). |
| `step_params` | `{}` | Parametri degli [step personalizzati](../guida/step-personalizzati.md) impostabili dal descrittore, per step (`"*"` = tutti). Default: nessuno. |
| `config_ref` | `none` | Revisione della configurazione di ambiente che il descrittore può scegliere con `config.ref` ([provider git](../integrazioni/git.md)): `none`, `ref` (solo ref ammessi) o `sha` (anche SHA di commit). |
| `max_parallel` | `0` (nessun limite) | [Parallelismo](../guida/ondate.md#target-in-parallelo) massimo che un descrittore può chiedere (`deploy.parallel`, `rollout.waves[].parallel`); una percentuale si calcola su tutti i target. La configurazione di ambiente non vi è soggetta. |

Vedi [Sicurezza](sicurezza.md#cosa-puo-chiedere-un-descrittore).

### `secrets` e `artifacts`

| Chiave | Default | Descrizione |
|---|---|---|
| `secrets.vault.enabled` | `false` | Abilita lo schema `vault:`. |
| `secrets.vault.address` | `VAULT_ADDR` | Indirizzo di Vault. |
| `secrets.vault.namespace` | | Namespace di Vault Enterprise. |
| `secrets.vault.auth` | `token` | Login: `token`, `approle` o `kubernetes`. |
| `secrets.vault.token_file` | | File con il token (`auth: token`); vuoto: `VAULT_TOKEN`. |
| `secrets.vault.approle.role_id`, `secret_id_file`, `mount` | mount `approle` | Login AppRole: role ID, file con il secret ID, percorso di mount. |
| `secrets.vault.kubernetes.role`, `jwt_file`, `mount` | `jwt_file` `/var/run/secrets/kubernetes.io/serviceaccount/token`, mount `kubernetes` | Login con il service account di Kubernetes. |
| `secrets.vault.ca_file` | | Bundle di CA aggiuntive. |
| `secrets.vault.timeout` | `10s` | Timeout di ogni chiamata. |
| `artifacts.timeout` | `30m` | Durata massima di un download. |
| `artifacts.allow_unlisted` | `true` | Ammette sorgenti non elencate in `artifacts.sources`. |
| `artifacts.sources` | `[]` | Sorgenti note; per un URL vince il prefisso corrispondente più lungo. |
| `artifacts.sources[].prefix` | | Prefisso dell'URL (`https://`, `http://`, `file:///`, `oci://`, `oci+http://`). |
| `artifacts.sources[].auth.type` | | `basic` (`username` + segreto), `bearer` oppure `header` (segreto nell'header `header`, es. `X-JFrog-Art-Api`). |
| `artifacts.sources[].auth.username`, `header` | | Utente per `basic`, nome dell'header per `header`. |
| `artifacts.sources[].auth.secret_ref` | | Riferimento al segreto (`vault:...`), risolto a ogni download; mai il valore. |
| `artifacts.sources[].ca_file` | | Bundle di CA dedicato. |

Vedi [Segreti e sorgenti degli artefatti](segreti-artefatti.md).

### `pipeline`

`pipeline.steps` è l'elenco ordinato degli step: il piano segue sempre questo ordine. Un elenco nel file sostituisce per intero quello di default. Vedi [Pipeline e step](../concetti/pipeline.md).

| Chiave | Default | Descrizione |
|---|---|---|
| `steps[].name` | | Nome dello step: uno degli step standard o uno step personalizzato. `drain` e `restore` sono riservati ai [pool di traffico](../guida/svuotamento.md). |
| `steps[].mode` | | `mandatory` (non disattivabile) oppure `optional`. |
| `steps[].enabled` | `false` | Default di uno step opzionale; il descrittore può cambiarlo con `steps:`. |
| `steps[].max_retries` | `0` | Tentativi aggiuntivi dello step dopo un errore transitorio. |
| `steps[].type` | | `command` o `http`: lo step è uno [step personalizzato](../guida/step-personalizzati.md#riferimento), definito da `timeout`, `secrets`, `params`, `outputs`, `ssh`, `run` e `dry_run`. |

Pipeline di default:

| Step | `mode` | `enabled` | `max_retries` |
|---|---|---|---|
| `secrets` | `mandatory` | | `0` |
| `artifacts` | `mandatory` | | `3` |
| `qa` | `optional` | `true` | `0` |
| `deploy` | `mandatory` | | `2` |
| `post-test` | `optional` | `false` | `5` |
| `activation` | `optional` | `true` | `0` |

Gli step personalizzati si configurano in `pipeline.custom`:

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

`remote-copy` è sempre attivo, anche quando il file elenca solo altri plugin: scrivere `remote-copy:` serve solo a dargli una configurazione.

Due chiavi sono lette da manent e non passate al plugin: `queue` (la [coda propria](scalabilita.md#plugin-queues) del plugin) e `max_parallel`, il limite ai target di un lotto che il plugin aggiorna nello stesso momento, qualunque cosa chieda il deploy (default `16`): protegge worker e destinazioni, come le connessioni SSH o i rate limit di un'API cloud. Vedi [target in parallelo](../guida/ondate.md#target-in-parallelo).

Plugin compilati in manent: [remote-copy](../plugins/remote-copy.md), [container](../plugins/container.md), [kubernetes](../plugins/kubernetes.md), [ecs](../plugins/ecs.md), [lambda](../plugins/lambda.md), [cloudrun](../plugins/cloudrun.md), [jboss](../plugins/jboss.md), [windows](../plugins/windows.md), [appservice](../plugins/appservice.md), [containerapps](../plugins/containerapps.md), [script](../plugins/script.md) (script sul worker, da un catalogo). Vedi anche [Plugin esterni](../plugins/esterni.md).

### `queues` { #queues }

Code dedicate ai deploy di alcune applicazioni, artefatti e ambienti, decise dagli operatori: né il descrittore né la configurazione di ambiente possono scegliere una coda. Vince la prima voce che corrisponde; solo i worker che elencano la coda in `dbos.queues` ne eseguono i deploy.

```yaml
queues:
  - name: gpu
    match:
      applications: ["ml/*"]         # pattern tenant/applicazione, come nei permessi
      artifacts: [trainer]           # pattern su artifact.name
      environments: [prod]
    concurrency: 2
```

| Chiave | Default | Descrizione |
|---|---|---|
| `name` | | Nome della coda: lettere, cifre, `.`, `_`, `-`. Non `dbos.queue`, la sua coda delle approvazioni o la coda di un plugin. |
| `match` | | `applications`, `artifacts`, `environments`: almeno un pattern; una lista vuota corrisponde a tutto. |
| `concurrency` | `0` | Limite globale della coda (`0`: `dbos.global_concurrency`). Limita anche i deploy della coda su ogni worker. |

La coda di un plugin prevale su una coda instradata, che prevale su quella del tenant. Non ancora supportato con `dbos.region_routing`. Vedi [code instradate](scalabilita.md#routed-queues).

### `rollout`

Health gate dei [rilasci a ondate](../guida/ondate.md#health-gate).

| Chiave | Default | Descrizione |
|---|---|---|
| `prometheus_url` | | API di Prometheus per `rollout.health.metrics` (es. `http://prometheus:9090`). Senza, un rollout con metriche Prometheus è rifiutato; lo stesso vale per gli altri sistemi. |
| `alertmanager_url` | | API di Alertmanager per `rollout.health.alerts`. |
| `datadog.url` | `https://api.datadoghq.com` | Sito dell'API di Datadog, per le metriche con `provider: datadog` (es. `https://api.datadoghq.eu`). |
| `datadog.api_key_ref`, `app_key_ref` | | Riferimenti ai segreti delle chiavi API e applicazione, letti a ogni chiamata. |
| `newrelic.url` | `https://api.newrelic.com/graphql` | Endpoint NerdGraph di New Relic (`provider: newrelic`). |
| `newrelic.account_id` | | Account di cui si leggono i dati. |
| `newrelic.api_key_ref` | | Riferimento al segreto della chiave utente. |
| `elastic.url` | | API ES\|QL di Elasticsearch (`provider: elastic`). |
| `elastic.api_key_ref` | | Riferimento al segreto di una API key; alternativo a `username` e `password_ref`. |
| `elastic.username`, `password_ref` | | Credenziali basic. |
| `elastic.ca_file` | | Bundle di CA aggiuntive. |
| `cloudwatch.region` | | Region di Amazon CloudWatch (`provider: cloudwatch`). |
| `cloudwatch.endpoint` | | Endpoint alternativo (facoltativo). |
| `cloudwatch.access_key_id_ref`, `secret_access_key_ref` | | Riferimenti ai segreti di credenziali statiche; vuoti: la catena di default dell'SDK. |
| `cloudwatch.role_arn`, `external_id` | | Ruolo da assumere. |
| `azure_monitor.workspace_id` | | Workspace Log Analytics di Azure Monitor Logs (`provider: azure-monitor`). |
| `azure_monitor.tenant_id`, `client_id`, `client_secret_ref` | | Service principal; senza, l'identità del worker (workload o managed identity). |
| `azure_monitor.endpoint`, `authority` | | Endpoint delle query e del login per i cloud sovrani. |
| `check_timeout` | `30s` | Tempo massimo di ogni lettura del gate. |

### `integrations`

Notifiche in Slack e Teams, approvazioni da Slack e ticket di change. Vedi [Chat e change management](../guida/integrazioni.md).

| Chiave | Default | Descrizione |
|---|---|---|
| `integrations.public_url` | | Indirizzo dell'interfaccia web, per i link nei messaggi e nei ticket. |
| `integrations.language` | `en` | Lingua dei messaggi: `en` o `it`. |
| `integrations.timeout` | `15s` | Timeout delle chiamate a Slack, Teams e ITSM. |
| `integrations.slack.bot_token_ref`, `signing_secret_ref` | | Segreti dell'app Slack: messaggi aggiornabili e pulsanti di approvazione. |
| `integrations.slack.identities.<ID utente>.subject`, `groups` | | Identità di manent (`oidc:alice`) e gruppi di un utente Slack (ID come `U024BE7LH`): le sue approvazioni valgono come quelle di quell'identità. |
| `integrations.slack.email_identity` | | Identità degli utenti non elencati dalla loro email di Slack, es. `oidc:{email}` (scope `users:read.email`). |
| `integrations.slack.api_url` | `https://slack.com/api` | Indirizzo dell'API di Slack; lo cambiano solo i test. |
| `integrations.notifications` | `[]` | Destinazioni dei messaggi. |
| `integrations.notifications[].name` | | Nome della destinazione: lettere minuscole, cifre e `-`, al più 40 caratteri. Compare nei nomi degli step (`notify:<nome>:<evento>`). |
| `integrations.notifications[].applications`, `environments` | `[]` | Pattern qualificati delle applicazioni e pattern degli ambienti. |
| `integrations.notifications[].events` | `[deployment, approval]` | `deployment` (avvio ed esito) e `approval` (richieste ed esiti). |
| `integrations.notifications[].dry_run` | `false` | Notifica anche i dry-run. |
| `integrations.notifications[].slack.channel` | | Canale, con il bot token: messaggi aggiornati e pulsanti di approvazione. |
| `integrations.notifications[].slack.webhook_ref` | | Riferimento al segreto di un incoming webhook (solo notifiche); alternativo a `channel`. |
| `integrations.notifications[].teams.webhook_ref` | | Riferimento al segreto di un webhook di Teams (flusso Workflows o canale). |
| `integrations.change.provider` | | `servicenow` o `jira`. |
| `integrations.change.poll_interval` | `1m` | Intervallo iniziale di lettura dell'approvazione del change (fino a 5 minuti). |
| `integrations.change.servicenow.*` | | `url`, `username` con `password_ref` oppure `token_ref`, `table`, `fields`, `start_state`, `close_states`, `close_codes`: vedi [ServiceNow](../guida/integrazioni.md#servicenow). |
| `integrations.change.jira.*` | | `url`, `username`, `token_ref`, `project`, `issue_type`, `fields`, `approved_statuses`, `rejected_statuses`, `transitions`: vedi [Jira](../guida/integrazioni.md#jira). |

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
| `CONSUL_HTTP_ADDR` | Indirizzo Consul, se l'`address` di un provider Consul è vuoto. |
| `CONSUL_HTTP_TOKEN` | Token ACL Consul. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Collector delle tracce, se `observability.tracing.endpoint` è vuoto. Valgono anche le altre variabili `OTEL_EXPORTER_OTLP_*` (intestazioni, certificati). |

I segreti con riferimento `env:NOME` sono letti dalle variabili d'ambiente del worker.
