BIN := bin/manent
SERVER_BIN := bin/manent-server
PROTO := pkg/plugin/external/proto/deployer.proto
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null)
LDFLAGS := -s -w -X github.com/joeg-ita/manent/internal/version.Version=$(VERSION) \
	-X github.com/joeg-ita/manent/internal/version.Commit=$(COMMIT)
# Postgres per i test (make test-postgres avvia un container se non è indicato).
PG_TEST_URL ?= postgres://postgres:postgres@localhost:55432/postgres?sslmode=disable
IMAGE ?= manent:$(VERSION)

.PHONY: build plugins test test-postgres test-e2e test-load test-usecases lint run fmt clean proto docs docs-serve image web web-dev web-test web-api

# npm con Node >= 20 locale, altrimenti in un container (scripts/web.sh).
WEB := scripts/web.sh

# Due binari: il client (solo API) e il server (API, worker, operazioni di piattaforma), con
# l'interfaccia web incorporata. NOUI=1 salta la build della UI (il server mostra una pagina
# che spiega come costruirla).
build: $(if $(NOUI),,web)
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/manent
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(SERVER_BIN) ./cmd/manent-server

# Interfaccia web (web/) compilata in internal/webui/dist, incorporata da go:embed.
web: web/node_modules/.package-lock.json
	$(WEB) run build

web/node_modules/.package-lock.json: web/package.json web/package-lock.json
	$(WEB) ci --no-audit --no-fund

# Server di sviluppo con ricarica su http://127.0.0.1:5173; l'API è MANENT_DEV_API (default :8080).
web-dev: web/node_modules/.package-lock.json
	WEB_PORT=5173 $(WEB) run dev -- --host 0.0.0.0

# Tipi, test dei componenti e allineamento dei tipi con api/openapi.yaml.
web-test: web/node_modules/.package-lock.json
	$(WEB) run lint
	$(WEB) test
	$(WEB) run check:api

# Rigenera i tipi TypeScript dell'API da api/openapi.yaml.
web-api: web/node_modules/.package-lock.json
	$(WEB) run gen:api

# Plugin distribuiti come binari esterni (vedi docs/content/plugins/esterni.md)
plugins:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/manent-plugin-remote-copy ./cmd/manent-plugin-remote-copy

test:
	go test ./...

# Stessi test su Postgres: ogni test crea ed elimina un proprio database.
test-postgres:
	@docker inspect manent-pg >/dev/null 2>&1 || docker run -d --name manent-pg -e POSTGRES_PASSWORD=postgres -p 55432:5432 postgres:16-alpine
	@until docker exec manent-pg pg_isready -U postgres >/dev/null 2>&1; do sleep 1; done
	MANENT_TEST_DATABASE_URL='$(PG_TEST_URL)' go test -count=1 ./...

# Plugin remote-copy contro un server SSH reale in Docker.
test-e2e:
	scripts/e2e-ssh.sh

# Casi d'uso end-to-end su Docker Compose: API, worker, client e target SSH in due region.
# Un sottoinsieme: make test-usecases CASES="uc1 uc2"; KEEP=1 lascia il laboratorio attivo.
test-usecases:
	test/usecases/run.sh $(if $(KEEP),--keep) $(CASES)

# Carico e caos: più worker, uno cade a metà (aggiungere MANENT_TEST_DATABASE_URL per Postgres).
test-load:
	MANENT_TEST_LOAD=1 go test -count=1 -run TestCaricoECaos -v ./internal/engine/

# Immagine del server (default) e del solo client.
image:
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(IMAGE) .
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --target client -t manent-client:$(VERSION) .

lint:
	gofmt -l . | (! grep .) && go vet ./...

fmt:
	gofmt -w .

run: build
	$(SERVER_BIN) -c configs/manent.yml serve

# Rigenera il codice gRPC del protocollo dei plugin esterni.
# Richiede protoc, protoc-gen-go e protoc-gen-go-grpc nel PATH.
proto:
	protoc --go_out=. --go_opt=paths=source_relative \
		--go-grpc_out=. --go-grpc_opt=paths=source_relative $(PROTO)

# Sito statico della documentazione in docs/site (MkDocs Material, vedi docs/README.md)
docs:
	cd docs && mkdocs build --strict

# Anteprima con ricarica automatica, raggiungibile dalle altre macchine della rete
# (http://<host>:8000/manent/). DOCS_ADDR=127.0.0.1:8000 la limita a questa macchina.
DOCS_ADDR ?= 0.0.0.0:8000
docs-serve:
	cd docs && mkdocs serve -a $(DOCS_ADDR)

clean:
	rm -rf bin work manent.db* docs/site
