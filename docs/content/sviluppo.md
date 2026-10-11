# Development

## Commands

```sh
make build        # bin/manent (client) and bin/manent-server (version from git describe)
make plugins      # external plugins in bin/
make test         # go test ./... (SQLite)
make test-postgres  # the same tests on Postgres (starts a container if needed)
make test-e2e     # remote-copy plugin against a real SSH server in Docker
make test-load    # load and chaos: several workers, one goes down halfway
make test-usecases  # end-to-end use cases on Docker Compose (see Use cases)
make image        # container image
make lint         # gofmt + go vet
make fmt          # gofmt -w
make proto        # regenerates the gRPC code of external plugins
make docs         # documentation site in docs/site
make docs-serve   # documentation preview with live reload
make clean        # removes binaries, working directory, SQLite database and generated site
```

A single test:

```sh
go test ./internal/engine -run TestWorkflowFallito -v
```

`make proto` requires `protoc`, `protoc-gen-go` and `protoc-gen-go-grpc`:

```sh
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
```

## Tests

- **Engine:** tests start a real DBOS on SQLite in a temporary directory. With `MANENT_TEST_DATABASE_URL` (a Postgres server on which the user can create databases) each test instead uses a Postgres database created for it and dropped at the end (`internal/testdb`): `make test-postgres` does this with a container. `scaling_test.go` starts several workers on the same database to check recovery by `executor_id`, serialization per environment and the absence of double deploys from schedules. They use a low `SchedulePollInterval`: with the 30-second default a new schedule would start only after the next reload.
- **Recovery and upgrades:** `recovery_test.go` simulates a worker going down (unrecorded shutdown) and checks automatic and manual recovery, the refusal of an alive worker, the migration of workflows to a new version and drain. `load_test.go` (`make test-load`, `MANENT_TEST_LOAD=1`) enqueues many deploys on three workers and takes one down while it runs: all deploys must complete, never two at once in the same environment.
- **Use cases:** `test/usecases` runs API, workers, client and SSH targets on Docker Compose and goes through deploys, failures, rollbacks and wave rollouts. See [Use cases](casi-uso.md).
- **Real SSH:** `plugins/remotecopy/e2e_test.go` runs only with `MANENT_TEST_SSH_ADDR`; `scripts/e2e-ssh.sh` starts a Debian sshd in Docker (`test/e2e/sshd`) and checks the releases layout, cleanup, rollback and checksum.
- **External plugins:** the `internal/extplugin` tests compile `testdata/fakeplugin` and start real processes. They check secrets, environment isolation, log forwarding, restart after a crash and sha256 verification.
- **Plugins:** `remotecopy` replaces SSH with the `Dialer` and `Session` interfaces.
- **Callbacks:** tests use an `httptest` server and check the four authentication modes and the URL policy.
- **Access:** `internal/auth` checks OIDC tokens against a fake identity provider (discovery, JWKS and JWTs signed in memory); `internal/authz` permissions and audit on a fake service; `internal/api` the 401/403 responses.
- **Vault:** tests use an `httptest` server that simulates KV v1/v2, AppRole login and token revocation.
- **Rollback:** `internal/engine/rollback_test.go` checks the order of checkpoints and compensations, stopping at the first failed compensation and dry-run; `internal/rollback` the per-artifact-type policies.

## CI and releases

`.github/workflows/ci.yml` runs on every push and pull request: lint and `go mod tidy`, tests on SQLite, tests on Postgres with load and chaos, end-to-end SSH tests, site build, `helm lint` and image build.

A release starts from a `vX.Y.Z` tag (`.github/workflows/release.yml`):

1. move the `Unreleased` section of `CHANGELOG.md` to `## [X.Y.Z] - YYYY-MM-DD` and update `appVersion` in `deploy/helm/manent/Chart.yaml`;
2. `git tag vX.Y.Z && git push --tags`;
3. GoReleaser (`.goreleaser.yaml`) builds the binaries, creates archives with SBOMs, signs the checksums and the image with cosign (keyless) and publishes the release with the CHANGELOG notes (`scripts/release-notes.sh`).

What counts as incompatible is described in [Versions and upgrades](operazioni/aggiornamenti.md).

## Conventions

- Every service is defined through interfaces, to ease mocks and tests.
- Every component receives the logger by injection. Logs reach both the application log and the SIEM as `rfc5424.Message`.
- Code comments are in Italian, descriptive but concise.
- `internal/app` is the only composition point: new internal plugins are registered in `app.NewPlugins`, new steps in `app.NewSteps`.
- A new step that must work in a dry-run implements `pipeline.DryRunner`. Otherwise it is skipped.
- A new step that changes environments implements `pipeline.Compensator` (and, if it must record the previous state, `pipeline.Checkpointer`), so rollback can undo it.
- An error that a new attempt cannot fix (policy, validation) is marked with `pipeline.Permanent`: the step fails immediately without retries.
- A new secret provider implements `secrets.Provider` and is registered by scheme in `app.NewSecretProvider`.
- A new rollback locator implements `plugin.ReleaseLocator` and is registered in `app.NewLocators`.
- DBOS workflow names (`manent.deploy`, `manent.scheduled-deploy`, `manent.approval`, `manent.deploy-pass`) must not change. An incompatible change to a workflow's structure (steps in a different order, different input) requires a new workflow version (`app.version`), to be noted in the CHANGELOG.
- In step code, logs are emitted with the step's context (`InfoContext(ctx, ...)`): only then do they reach the deploy log.
- The documentation in `docs/content` is updated together with features, in English (`page.md`, the default language) and Italian (`page.it.md`). See `docs/README.md` for the site structure.

## Repository layout

```
cmd/manent/                      client (API only)
cmd/manent-server/               server: API, worker, admin
cmd/manent-plugin-remote-copy/   remote-copy as an external plugin
deploy/helm/manent/              Helm chart
deploy/systemd/                  systemd units
scripts/                         end-to-end tests and release notes
test/e2e/                        images for end-to-end tests
internal/                        engine (not importable from outside)
pkg/plugin/                      public plugin contract
pkg/plugin/external/             SDK and gRPC protocol of external plugins
plugins/remotecopy/              reference plugin
configs/                         example configuration
examples/                        example descriptors and environment configurations
docs/                            this site
```
