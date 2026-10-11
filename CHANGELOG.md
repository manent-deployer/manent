# Changelog

Tutte le modifiche rilevanti di manent sono registrate in questo file.

Il formato segue [Keep a Changelog](https://keepachangelog.com/it-IT/1.1.0/) e il progetto
adotta il [Semantic Versioning](https://semver.org/lang/it/): vedi
[Versioni e aggiornamenti](docs/content/operazioni/aggiornamenti.md) per cosa è considerato
incompatibile (API, descrittore, configurazione, protocollo dei plugin, workflow DBOS).

Al rilascio la sezione Unreleased diventa `## [X.Y.Z] - AAAA-MM-GG`: il workflow di rilascio
ne usa il contenuto come note.

## [Unreleased]

### Aggiunto

- Casi d'uso end-to-end su Docker Compose (`make test-usecases`, `test/usecases`): API, worker,
  client e target SSH in due region, da un deploy pianificato ai rilasci a ondate multi-region,
  con fallimenti, rollback, recovery e governance.
- Documentazione in inglese (lingua di default) e in italiano, sotto `/it/` del sito, con
  selettore della lingua (plugin mkdocs-static-i18n).
- Rilasci a ondate multi-region: etichette `region`/`az` sui target, strategia `rollout` (ondate
  con selettori, passi per AZ o region, `parallelRegions`, `maxUnavailable`, `bake`), health gate
  su sonde HTTP, Prometheus e Alertmanager, `onFailure` (`stop`, `rollback-wave`, `rollback-all`),
  approvazioni tra le ondate, freeze per region, esecuzione dei passi sui worker della region
  (`dbos.region`, `dbos.region_routing`, workflow `manent.deploy-pass`), stato del rollout in
  `manent status`, `drift` nell'inventario, metrica `manent_rollout_passes_total`.
- Operatività:
  - registro dei worker con heartbeat (`manent admin executors`), recovery dei workflow di un
    worker perso, manuale (`manent admin recover --executor`) o automatica
    (`recovery.auto_recover_after`), con rivendicazione esclusiva rispetto al riavvio del nodo;
  - migrazione dei workflow a una nuova versione dei workflow dopo un aggiornamento
    (`manent admin recover --from-version`);
  - drain all'arresto del worker (`dbos.drain_timeout`) e sonde `/healthz` e `/readyz`;
  - retention dello storico (`retention`, `manent admin prune`) che conserva le ultime release
    di ogni ambiente;
  - metriche Prometheus (`/metrics`, `observability.listen` per i worker), tracing
    OpenTelemetry via OTLP dalla richiesta all'API fino agli step del worker;
  - log di ogni deploy consultabili da API e CLI (`manent logs -f`), timeline degli step
    (`manent steps`) e metriche DORA (`manent dora`, `GET /api/v1/metrics/dora`);
  - `manent version`, immagine container, chart Helm, unità systemd, rilasci con GoReleaser
    (checksum firmati, SBOM, immagine firmata), CI con test su SQLite e Postgres, test
    end-to-end del plugin SSH e test di carico e caos.
- Governance: inventario delle release, promozione obbligatoria tra ambienti, approvazioni
  con divieto di auto-approvazione, finestre di freeze con deroga, rollback manuale.
- Sicurezza: autenticazione OIDC e token statici (anche da file, con scadenza), RBAC per
  applicazione e ambiente, audit con catena di hash, policy sui descrittori, Vault, credenziali
  e allowlist delle sorgenti degli artefatti, CLI remota tramite API (`--server`).
- Rollback automatico per tipo di artefatto con checkpoint e compensazioni.
- Dry-run, callback firmati, plugin esterni su gRPC, documentazione MkDocs.

### Modificato

- **Incompatibile:** la CLI è divisa in due binari. `manent` è il client: parla solo con l'API
  (`--server`, `MANENT_SERVER` o `~/.config/manent/client.yml`, default `http://127.0.0.1:8080`)
  e non contiene il motore, i plugin né i driver di database. `manent-server` contiene `serve`,
  `api` (prima `serve --no-worker`), `worker`, `admin`, `audit verify` e `validate` locale. La
  CLI diretta sul database non esiste più per gli utenti; `manent validate` verifica il
  descrittore sul server (`POST /api/v1/deployments/validate`). Immagini `manent-server` e
  `manent` (solo client).
- La nuova struttura del workflow per i rilasci a ondate non cambia i deploy senza `rollout`
  (stessi nomi degli step DBOS): la versione dei workflow resta compatibile.
- I test dell'engine si eseguono anche su Postgres (`MANENT_TEST_DATABASE_URL`, `make test-postgres`).

### Corretto

- `manent status` (e l'API) di un deploy fallito ora riporta l'errore: in modalità client lo
  stato DBOS non lo includeva.
- Il log di un deploy raccoglie le righe INFO anche con `logging.level` più alto.
- Rollout multi-region con `dbos.region_routing`: le sonde HTTP dell'health gate sui target di
  una region sono eseguite da un worker di quella region (workflow `manent.gate-probe`), non
  dall'orchestratore, che con reti separate per region non raggiungeva i target.
- Il reinvio di un descrittore con `id` e `schedule.at` dopo l'orario restituisce il deploy
  esistente invece di essere rifiutato come pianificato nel passato.
- Dopo un deploy fallito che ha lasciato i target modificati (senza rollback automatico),
  `manent rollback` ripristina l'ultima versione riuscita, anche indicata con `--to`, invece di
  rifiutarla come già in esercizio.
