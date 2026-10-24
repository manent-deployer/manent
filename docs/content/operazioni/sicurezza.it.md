# Sicurezza

## Checklist per la produzione

La configurazione di default è pensata per lo sviluppo. All'avvio, `serve` e `worker` registrano un avviso (`configuration not fit for production`) per ogni impostazione ancora permissiva. In produzione:

- [ ] `auth.mode: enabled`, con OIDC per gli utenti e token statici per la CI, questi in `auth.tokens_file` e con `expires_at`. Vedi [Autenticazione, permessi e audit](accesso.md).
- [ ] Utenti e CI usano l'API o il client `manent`; `manent-server` e le credenziali del database solo ai processi del server e agli operatori della piattaforma.
- [ ] `rbac.bindings` con il minimo necessario: ad esempio `deployer` sugli ambienti non di produzione, `tester` (solo dry-run) in produzione, `operator` ai release manager.
- [ ] `audit.file` con `hmac_key_ref` e `max_size_mb`, verificato periodicamente con `manent-server audit verify`, oltre alla conservazione delle voci nel SIEM.
- [ ] `governance` con promozione obbligatoria e approvazioni per la produzione, e le finestre di freeze aziendali. Il ruolo `approver` a un gruppo diverso da chi rilascia; `deployment.freeze-override` solo a chi gestisce le emergenze. Vedi [Governance dei rilasci](../guida/governance.md).
- [ ] `descriptor_policy` impostata: nessun comando dal descrittore, riferimenti ai segreti limitati, target dalla configurazione di ambiente.
- [ ] Segreti in Vault (`secrets.vault`), non in variabili d'ambiente del worker. Vedi [Segreti e sorgenti degli artefatti](segreti-artefatti.md).
- [ ] `artifacts.allow_unlisted: false`, con le sorgenti e le relative credenziali in `artifacts.sources`.
- [ ] `callbacks.allowed_hosts` impostato.
- [ ] Con gli [step personalizzati](../guida/step-personalizzati.md): `pipeline.custom.spec_key_refs` impostato, un utente SSH dedicato (`ssh.user`) con privilegi minimi, segreti di sola lettura per `dry_run`, i controlli dichiarati `mandatory` e nessun `insecure_ignore_host_key`.
- [ ] `sha256` per ogni plugin esterno, binari in una directory non scrivibile dal worker.
- [ ] TLS terminato davanti a `serve` (reverse proxy o load balancer).
- [ ] Accesso al database DBOS solo per worker, `serve` e operatori della piattaforma.
- [ ] `/metrics`, `/healthz` e `/readyz` non richiedono autenticazione: `observability.listen` raggiungibile solo dalla rete di monitoraggio e, davanti all'API, `/metrics` non esposto all'esterno.
- [ ] `retention.max_age` coerente con i requisiti di conservazione, e audit conservato a parte (file e SIEM).

## Segreti

Il principio è che **un valore segreto non viene mai persistito**. DBOS salva nel database il descrittore, il piano, la configurazione di ambiente letta e l'output di ogni step, quindi nessuno di questi deve contenere segreti.

- **Descrittore e configurazione di ambiente:** contengono solo riferimenti (`env:`, `file:`, `vault:`) associati a un nome logico.
- **Step `secrets`:** verifica che i riferimenti siano risolvibili ma non restituisce i valori.
- **Plugin:** leggono un segreto per nome logico, tramite `req.Secrets`, solo nel momento in cui serve.
- **Callback e sorgenti degli artefatti:** l'autenticazione riferisce un segreto, letto solo al momento dell'invio o del download.
- **Consul:** il token si legge da `CONSUL_HTTP_TOKEN` o da `token_file`, mai dal file di configurazione.
- **Token dell'API:** in configurazione c'è solo lo sha256.

## Cosa può chiedere un descrittore

Chi può inviare un descrittore non è necessariamente chi amministra gli ambienti. La configurazione di ambiente è autorevole, ma ciò che non definisce può essere aggiunto dal descrittore. Senza limiti, un descrittore potrebbe:

- impostare `post_command` e far eseguire comandi arbitrari sui target, con le credenziali dell'ambiente;
- dichiarare un riferimento `env:` a una variabile del worker (ad esempio `env:CONSUL_HTTP_TOKEN`) e farla usare a un plugin;
- indicare target propri e ricevere l'artefatto con le credenziali dell'ambiente.

`descriptor_policy` stabilisce cosa il descrittore può aggiungere:

```yaml title="manent.yml"
descriptor_policy:
  params:                       # per plugin; "*" vale per tutti i plugin
    remote-copy: [dest_dir, verify_checksum, keep_releases]
  secret_refs:                  # pattern con {application} e {environment}
    - "vault:secret/data/{application}/*"
  targets: false
```

| Chiave | Default | Descrizione |
|---|---|---|
| `params` | `{"*": ["*"]}` | Pattern dei parametri di `deploy.params` impostabili dal descrittore, per plugin. Si applicano le voci del plugin indicato (`deploy.plugin`, dall'ambiente o dal descrittore) e quelle di `"*"`. |
| `secret_refs` | `["*"]` | Pattern dei riferimenti che il descrittore può dichiarare in `secrets`. |
| `targets` | `true` | Ammette `deploy.targets` nel descrittore quando l'ambiente non li definisce. |

Ciò che arriva dalla configurazione di ambiente non è soggetto alla policy: comandi, segreti e target restano sotto il controllo di chi la amministra. Se la sezione è indicata, i campi omessi mantengono il default; in produzione vanno impostati tutti e tre.

La policy viene verificata all'accodamento (`422`), di nuovo nello step `config` insieme alla fusione con la configurazione di ambiente, e da `manent validate`. Nello step `config` una violazione fa fallire il deploy subito, senza i retry dello step.

## Callback

Gli URL dei callback li sceglie chi richiede il deploy. Per evitare che un descrittore faccia chiamare al worker servizi interni (SSRF):

- di default sono ammessi solo URL `https` (`callbacks.allow_http`);
- `callbacks.allowed_hosts` limita gli host raggiungibili;
- i redirect non vengono seguiti;
- un URL non può contenere credenziali.

Vedi [Callback](../guida/callback.md#limiti-sugli-url).

## Plugin esterni

Un plugin esterno è un binario eseguito dal worker con i suoi permessi. manent applica queste protezioni:

| Protezione | Come |
|---|---|
| Integrità del binario | Con `external.sha256`, il binario viene verificato all'attivazione e a ogni avvio del processo. |
| Ambiente isolato | Il processo non eredita le variabili del worker, tranne `PATH`, `HOME`, `TMPDIR`, `TZ` e `LANG`. Le altre vanno passate esplicitamente con `external.env`. |
| Segreti su richiesta | I segreti arrivano solo tramite il canale gRPC dei segreti, attivo per la durata di una singola chiamata `Deploy`, `DryRun`, `CurrentRelease` o `Rollback`. |
| Canale cifrato | La comunicazione con il processo usa mTLS con certificati generati a ogni avvio. |

!!! warning
    Senza `sha256` manent registra un avviso: chi può sostituire il binario può eseguire codice con i permessi del worker. In produzione indicare sempre l'hash e installare i binari in una directory non scrivibile dall'utente del worker.

## SSH

Il plugin `remote-copy` verifica la host key dei target con `known_hosts`. `insecure_ignore_host_key: true` disattiva la verifica ed è pensato solo per lo sviluppo.

## API

Con `auth.mode: enabled` l'API richiede un token bearer (OIDC o statico) e applica i permessi. manent non termina il TLS: va esposto dietro un reverse proxy o un load balancer che lo faccia. Con `auth.mode: none` l'API non ha autenticazione: usarla solo in locale.

## Database

Il database di sistema DBOS contiene lo storico dei deploy, la coda, i log dei deploy e il registro dei worker. Chi vi accede con permessi di scrittura può accodare, annullare o modificare workflow senza passare dai permessi di manent. Va quindi trattato come un componente amministrativo: credenziali solo a worker, `serve` e operatori della piattaforma, connessione cifrata (`sslmode=verify-full`).
