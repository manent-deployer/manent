BIN := bin/manent
SERVER_BIN := bin/manent-server
PROTO := pkg/plugin/external/proto/deployer.proto
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null)
LDFLAGS := -s -w -X github.com/joeg-ita/manent/internal/version.Version=$(VERSION) \
	-X github.com/joeg-ita/manent/internal/version.Commit=$(COMMIT)
# Postgres for tests (make test-postgres starts a container if none is given).
PG_TEST_URL ?= postgres://postgres:postgres@localhost:55432/postgres?sslmode=disable
IMAGE ?= manent:$(VERSION)

.PHONY: build plugins test test-postgres test-e2e test-load test-usecases lint run fmt clean proto docs docs-serve image web web-dev web-test web-api

# npm with a local Node >= 20, otherwise in a container (scripts/web.sh).
WEB := scripts/web.sh

# Two binaries: the client (API only) and the server (API, workers, platform operations), with
# the embedded web interface. NOUI=1 skips the UI build (the server shows a page explaining
# how to build it).
build: $(if $(NOUI),,web)
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/manent
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(SERVER_BIN) ./cmd/manent-server

# Web interface (web/) built into internal/webui/dist, embedded by go:embed.
web: web/node_modules/.package-lock.json
	$(WEB) run build

web/node_modules/.package-lock.json: web/package.json web/package-lock.json
	$(WEB) ci --no-audit --no-fund

# Development server with reload on http://127.0.0.1:5173; the API is MANENT_DEV_API (default :8080).
web-dev: web/node_modules/.package-lock.json
	WEB_PORT=5173 $(WEB) run dev -- --host 0.0.0.0

# Types, component tests and alignment of the types with api/openapi.yaml.
web-test: web/node_modules/.package-lock.json
	$(WEB) run lint
	$(WEB) test
	$(WEB) run check:api

# Regenerates the TypeScript API types from api/openapi.yaml.
web-api: web/node_modules/.package-lock.json
	$(WEB) run gen:api

# Plugins shipped as external binaries (see docs/content/plugins/esterni.md)
plugins:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/manent-plugin-remote-copy ./cmd/manent-plugin-remote-copy

test:
	go test ./...

# Same tests on Postgres: every test creates and drops its own database.
test-postgres:
	@docker inspect manent-pg >/dev/null 2>&1 || docker run -d --name manent-pg -e POSTGRES_PASSWORD=postgres -p 55432:5432 postgres:16-alpine
	@until docker exec manent-pg pg_isready -U postgres >/dev/null 2>&1; do sleep 1; done
	MANENT_TEST_DATABASE_URL='$(PG_TEST_URL)' go test -count=1 ./...

# remote-copy plugin against a real SSH server in Docker.
test-e2e:
	scripts/e2e-ssh.sh

# End-to-end use cases on Docker Compose: API, workers, client and SSH targets in two regions.
# A subset: make test-usecases CASES="uc1 uc2"; KEEP=1 leaves the lab running.
test-usecases:
	test/usecases/run.sh $(if $(KEEP),--keep) $(CASES)

# Load and chaos: several workers, one dies halfway (add MANENT_TEST_DATABASE_URL for Postgres).
test-load:
	MANENT_TEST_LOAD=1 go test -count=1 -run TestCaricoECaos -v ./internal/engine/

# Image of the server (default) and of the client only.
image:
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(IMAGE) .
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --target client -t manent-client:$(VERSION) .

lint:
	gofmt -l . | (! grep .) && go vet ./...

fmt:
	gofmt -w .

run: build
	$(SERVER_BIN) -c configs/manent.yml serve

# Regenerates the gRPC code of the external plugin protocol.
# Requires protoc, protoc-gen-go and protoc-gen-go-grpc in PATH.
proto:
	protoc --go_out=. --go_opt=paths=source_relative \
		--go-grpc_out=. --go-grpc_opt=paths=source_relative $(PROTO)

# Static documentation site in docs/site (MkDocs Material, see docs/README.md)
docs:
	cd docs && mkdocs build --strict

# Preview with live reload, reachable from the other machines on the network
# (http://<host>:8000/manent/). DOCS_ADDR=127.0.0.1:8000 limits it to this machine.
DOCS_ADDR ?= 0.0.0.0:8000
docs-serve:
	cd docs && mkdocs serve -a $(DOCS_ADDR)

clean:
	rm -rf bin work manent.db* docs/site
