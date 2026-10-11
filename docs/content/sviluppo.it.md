# Sviluppo

## Comandi

```sh
make build        # bin/manent (client) e bin/manent-server con l'interfaccia web (NOUI=1 la salta)
make web         # interfaccia web in internal/webui/dist (Node >= 20 o un container node:22)
make web-dev     # interfaccia web su http://127.0.0.1:5173 contro un manent locale
make web-test    # tipi, test dei componenti (Vitest) e allineamento con api/openapi.yaml
make web-api     # rigenera i tipi TypeScript da api/openapi.yaml
make plugins      # plugin esterni in bin/
make test         # go test ./... (SQLite)
make test-postgres  # gli stessi test su Postgres (avvia un container se serve)
make test-e2e     # plugin remote-copy contro un server SSH reale in Docker
make test-load    # carico e caos: più worker, uno cade a metà
make test-usecases  # casi d'uso end-to-end su Docker Compose (vedi Casi d'uso)
make image        # immagine container
make lint         # gofmt + go vet
make fmt          # gofmt -w
make proto        # rigenera il codice gRPC dei plugin esterni
make docs         # sito della documentazione in docs/site
make docs-serve   # anteprima della documentazione con ricarica automatica
make clean        # rimuove binari, directory di lavoro, database SQLite e sito generato
```

Un singolo test:

```sh
go test ./internal/engine -run TestWorkflowFallito -v
```

`make proto` richiede `protoc`, `protoc-gen-go` e `protoc-gen-go-grpc`:

```sh
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
```

## Test

- **Engine:** i test avviano un DBOS reale su SQLite in una directory temporanea. Con `MANENT_TEST_DATABASE_URL` (un server Postgres su cui l'utente può creare database) ogni test usa invece un database Postgres creato per lui ed eliminato alla fine (`internal/testdb`): `make test-postgres` lo fa con un container. `scaling_test.go` avvia più worker sullo stesso database per verificare la recovery per `executor_id`, la serializzazione per ambiente e l'assenza di doppi deploy dalle schedule. Usano un `SchedulePollInterval` basso: con il default di 30 secondi una nuova schedule partirebbe solo dopo il ricaricamento successivo.
- **Recovery e aggiornamenti:** `recovery_test.go` simula la caduta di un worker (arresto non registrato) e verifica la recovery automatica e manuale, il rifiuto di un worker attivo, la migrazione dei workflow a una nuova versione e il drain. `load_test.go` (`make test-load`, `MANENT_TEST_LOAD=1`) accoda molti deploy su tre worker e ne fa cadere uno mentre esegue: tutti i deploy devono concludersi, mai due insieme nello stesso ambiente.
- **Casi d'uso:** `test/usecases` avvia API, worker, client e target SSH su Docker Compose e percorre deploy, fallimenti, rollback e rilasci a ondate. Vedi [Casi d'uso](casi-uso.md).
- **Interfaccia web:** Vitest e Testing Library verificano le viste con un'API finta costruita sui tipi OpenAPI (`web/src/test`); `internal/webui` le intestazioni di sicurezza e il fallback per il routing lato client; `internal/api/openapi_test.go` che `api/openapi.yaml` descriva esattamente le rotte del server. Playwright (`web/e2e`) gira in UC8 contro il laboratorio, con un identity provider OIDC di prova e axe per l'accessibilità.
- **SSH reale:** `plugins/remotecopy/e2e_test.go` gira solo con `MANENT_TEST_SSH_ADDR`; `scripts/e2e-ssh.sh` avvia un sshd Debian in Docker (`test/e2e/sshd`) e verifica layout releases, pulizia, rollback e checksum.
- **Plugin esterni:** i test di `internal/extplugin` compilano `testdata/fakeplugin` e avviano processi reali. Verificano segreti, isolamento dell'ambiente, inoltro dei log, riavvio dopo un crash e verifica dello sha256.
- **Plugin:** `remotecopy` sostituisce SSH con le interfacce `Dialer` e `Session`.
- **Callback:** i test usano un server `httptest` e verificano le quattro modalità di autenticazione e la policy sugli URL.
- **Accesso:** `internal/auth` verifica i token OIDC contro un identity provider finto (discovery, JWKS e JWT firmati in memoria); `internal/authz` permessi e audit su un servizio finto; `internal/api` le risposte 401/403.
- **Vault:** i test usano un server `httptest` che simula KV v1/v2, il login AppRole e la revoca del token.
- **Rollback:** `internal/engine/rollback_test.go` verifica ordine di checkpoint e compensazioni, arresto alla prima compensazione fallita e dry-run; `internal/rollback` le policy per tipo di artefatto.

## CI e rilasci

`.github/workflows/ci.yml` esegue su ogni push e pull request: lint e `go mod tidy`, test su SQLite, test su Postgres con carico e caos, test SSH end-to-end, build del sito, `helm lint` e build dell'immagine.

Un rilascio parte da un tag `vX.Y.Z` (`.github/workflows/release.yml`):

1. spostare la sezione `Unreleased` di `CHANGELOG.md` in `## [X.Y.Z] - AAAA-MM-GG` e aggiornare `appVersion` in `deploy/helm/manent/Chart.yaml`;
2. `git tag vX.Y.Z && git push --tags`;
3. GoReleaser (`.goreleaser.yaml`) compila i binari, crea archivi con SBOM, firma i checksum e l'immagine con cosign (keyless) e pubblica il rilascio con le note del CHANGELOG (`scripts/release-notes.sh`).

Cosa è considerato incompatibile è descritto in [Versioni e aggiornamenti](operazioni/aggiornamenti.md).

## Convenzioni

- Ogni servizio è definito tramite interfacce, per facilitare mock e test.
- Ogni componente riceve il logger per iniezione. I log arrivano sia al log applicativo sia al SIEM come `rfc5424.Message`.
- I commenti nel codice sono in italiano, descrittivi ma sintetici.
- `internal/app` è l'unico punto di composizione: i nuovi plugin interni si registrano in `app.NewPlugins`, i nuovi step in `app.NewSteps`.
- Un nuovo step che deve funzionare in dry-run implementa `pipeline.DryRunner`. Altrimenti viene saltato.
- Un nuovo step che modifica gli ambienti implementa `pipeline.Compensator` (e, se deve registrare lo stato precedente, `pipeline.Checkpointer`), così il rollback può annullarlo.
- Un errore che un nuovo tentativo non può risolvere (policy, validazione) va marcato con `pipeline.Permanent`: lo step fallisce subito senza i retry.
- Un nuovo provider di segreti implementa `secrets.Provider` e si registra per schema in `app.NewSecretProvider`.
- Un nuovo locator di rollback implementa `plugin.ReleaseLocator` e si registra in `app.NewLocators`.
- I nomi dei workflow DBOS (`manent.deploy`, `manent.scheduled-deploy`, `manent.approval`) non vanno cambiati. Una modifica incompatibile alla struttura di un workflow (step in ordine diverso, input diverso) richiede una nuova versione dei workflow (`app.version`), da segnalare nel CHANGELOG.
- Nel codice degli step i log vanno emessi con il context dello step (`InfoContext(ctx, ...)`): solo così arrivano al log del deploy.
- La documentazione in `docs/content` va aggiornata insieme alle funzionalità. Vedi `docs/README.md` per la struttura del sito.

## Struttura del repository

```
cmd/manent/                      client (solo API)
cmd/manent-server/               server: API, worker, admin
cmd/manent-plugin-remote-copy/   remote-copy come plugin esterno
deploy/helm/manent/              chart Helm
deploy/systemd/                  unità systemd
scripts/                         test end-to-end e note di rilascio
test/e2e/                        immagini per i test end-to-end
internal/                        motore (non importabile da fuori)
pkg/plugin/                      contratto pubblico dei plugin
pkg/plugin/external/             SDK e protocollo gRPC dei plugin esterni
plugins/remotecopy/              plugin di riferimento
api/openapi.yaml                 contratto dell'API HTTP
web/                             interfaccia web (React, TypeScript, Vite)
internal/webui/                  incorpora e serve la build di web/
configs/                         configurazione d'esempio
examples/                        descrittori e configurazioni di ambiente d'esempio
docs/                            questo sito
```
