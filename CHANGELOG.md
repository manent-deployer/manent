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

- Plugin `windows` per host Windows: applicazioni IIS (`physicalPath` dell'applicazione,
  application pool fermato o riciclato) e servizi Windows (`binPath` con `sc.exe config`, attesa
  di `Running`), via WinRM (NTLM o basic, HTTPS) o OpenSSH, con script PowerShell compatibili con
  Windows PowerShell 5.1 e PowerShell 7. Una directory per versione sotto `releases_dir`,
  sha256 verificato sul target, rollback e `CurrentRelease` (riconciliazione), tipo di artefatto
  `windows` con rollback ammesso nei default. Caso d'uso UC20 su un host Windows emulato con
  PowerShell vero, WinRM e OpenSSH.
- Integrazioni con CI e IaC: `manent deploy --ci` (esiti in `$GITHUB_OUTPUT` o nel report dotenv
  di GitLab, riepilogo in `$GITHUB_STEP_SUMMARY`), `--outputs`, `--summary`, `--id`, `--version`,
  `--source`, `--sha256`; verifica di una configurazione di ambiente candidata (`manent env
  validate`, `POST /api/v1/environments/validate`); GitHub Action (`integrations/github`),
  template GitLab (`integrations/gitlab/manent.yml`) e provider Terraform (`integrations/terraform`:
  `manent_deployment` con drift dall'inventario e dalla riconciliazione, `manent_inventory`,
  `manent_environment_check`). Casi d'uso UC18 (CI) e UC19 (Terraform vero).
- Stato reale dei target (`reconcile` in `manent.yml`): i worker leggono a intervalli, un target
  alla volta, la release in esercizio con `CurrentRelease` del plugin e la confrontano con
  l'inventario. `targets` e `out_of_sync` nell'inventario (API, `manent inventory
  --out-of-sync`, interfaccia web), stati `in-sync`, `drifted`, `missing`, `standby`,
  `unknown`, metrica `manent_targets`, log e SIEM quando un target cambia fuori da manent. Caso
  d'uso UC17 su target SSH veri.
- `maxUnavailable` per ondata (`waves[].maxUnavailable`): sostituisce quello del rollout per i
  passi dell'ondata.
- Gate per lotto nei rilasci a ondate: con `waves[].batchBake` le sonde leggono i target di
  ogni lotto (tranne l'ultimo, seguito dal gate dello stadio) prima del lotto successivo, con
  `interval` e `failuresToStop` del gate; anche nei passi eseguiti nella region.
- Analisi canary alla Kayenta nei gate di salute: `analysis: mann-whitney` confronta le serie di
  candidata e stabile con il test U di Mann-Whitney (`direction`, `alpha`, `maxRatio`/`minRatio`,
  `step`, `lookback`, `minPoints`), `health.analysis` calcola un punteggio pesato 0–100 con
  soglie `marginal` (durante il bake) e `pass` (alla fine), metriche `critical` e inconcludenti.
  Punteggio in `rollout.stages[].score` e nell'interfaccia web.
- Sorgenti di metriche oltre a Prometheus (`provider` della metrica, configurazione in
  `rollout`): Datadog, New Relic (NerdGraph/NRQL), Elastic (ES|QL) e Amazon CloudWatch
  (`GetMetricData`, `plugins/awsmetrics`). Casi d'uso UC15 (Prometheus e HAProxy veri) e UC16
  (sorgenti emulate da `test/usecases/fakes`).

- Casi d'uso UC10–UC14: svuotamento e router su un HAProxy vero con la Data Plane API, rilascio
  per percentuale di un chart Helm su k3s con la Gateway API, step personalizzati (SSH e HTTP),
  integrazioni Slack, Teams e ServiceNow (servizio finto `test/usecases/fakes`) e JBoss su un
  WildFly vero. I casi sono eseguiti in ordine numerico.

- Plugin `jboss`: rilascia `war` ed `ear` su JBoss EAP e WildFly (standalone o domain) con
  l'API HTTP di management, un deployment per versione e cambio atomico; sospensione, reload e
  restart del server con i parametri booleani `suspend`, `reload` e `restart`; rollback alla
  versione conservata nel repository dei contenuti.
- Interfaccia web servita da `manent-server` su `/` (`web/`, React + TypeScript, incorporata con
  `go:embed` da `internal/webui`): deploy con filtri e paginazione, dettaglio con ondate, step e log
  in tempo reale, approvazioni, inventario con promozione e rollback dopo un dry-run, freeze,
  schedule e metriche DORA. Login OIDC con PKCE (`auth.oidc.ui`), token solo in memoria, CSP
  rigida, tema scuro e chiaro, inglese e italiano, accessibilità WCAG 2.1 AA.
- Specifica OpenAPI dell'API (`api/openapi.yaml`), verificata contro le rotte del server.
- Endpoint `GET /api/v1/me`, `GET /api/v1/approvals`, `GET /api/v1/deployments/{id}/events`
  (Server-Sent Events), `GET /ui/config.json` e paginazione con `cursor`/`next_cursor`;
  comandi `manent whoami` e `manent approvals`.
- Caso d'uso UC8: interfaccia web con Playwright e axe contro il laboratorio, con un identity
  provider OIDC di prova.
- Plugin `container`: rilascia immagini su server e VM via SSH con Docker o Podman (pull per
  digest, attesa di HEALTHCHECK o avvio stabile, release e rollback dalle etichette del container,
  login al registry con la password sullo stdin).
- Plugin `kubernetes`: applica manifest (YAML, JSON o `.tar.gz`) su uno o più cluster con
  server-side apply, senza kubectl né client-go; attende il rollout dei workload, conserva lo
  storico delle release in ConfigMap per il rollback, `prune`, `require_image_digest`, variabili
  `${...}` dalla configurazione di ambiente e dry-run lato server.
- Sorgenti `oci://` e `oci+http://` per le immagini dei container: manent risolve il digest del
  manifest (anche con token alla Docker Hub) e lo usa come sha256 dell'artefatto.
- Casi d'uso UC6 (container su VM) e UC7 (Kubernetes su k3s) con registry e VM con Docker nel
  laboratorio.
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

- I messaggi di errore sono in inglese: errori restituiti da API (`{"error": ...}`), client e
  server, step, plugin (compresi gli script PowerShell del plugin `windows`), plugin esterni
  (gRPC), provider Terraform e motivi dei gate di salute. Log, output dei comandi, notifiche in
  chat e interfaccia web restano in italiano. Chi confronta il testo degli errori (script, alert)
  va aggiornato.
- Nella strategy `traffic` il gradino del 100% e il primo rilascio non valutano più le metriche
  `compare: stable`: la stabile non riceve traffico e il confronto leggerebbe una serie vuota.
- Governance: `approvals.when: rollout` sposta le approvazioni di una regola nei punti
  `approvalsBetween` del rollout, senza l'approvazione prima del deploy; un deploy senza punti di
  approvazione viene rifiutato.
- Licenza cambiata da GPL-3.0 ad Apache-2.0.
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

- Plugin `jboss`: il cambio di versione usa `replace-deployment`; con `undeploy` e `deploy` nella
  stessa composite WildFly non avviava il nuovo deployment con lo stesso runtime-name.
- `approvals.when: rollout`: il primo rilascio per percentuale, che salta i gradini intermedi,
  chiede l'approvazione prima del traffico; un punto di approvazione dopo l'ultima ondata non
  conta più come punto valido.
- Un webhook di chat non https è un errore permanente, senza tentativi ripetuti.

- L'API (`api`, `serve --no-worker`) e `admin` su un database nuovo creano lo schema DBOS invece
  di fallire se partono prima dei worker.
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
