# Quick start

## Requirements

- Go 1.27 or later. `go.mod` pins the 1.27.x toolchain: with an older Go it is downloaded automatically (`GOTOOLCHAIN=auto`).
- No database to install to try manent: by default DBOS uses a SQLite file (`manent.db`). Production needs Postgres.

## Build

```sh
make build      # bin/manent (client) and bin/manent-server
make plugins    # optional: external plugins in bin/ (manent-plugin-remote-copy)
```

## First deploy

1. **Check the descriptor.** `validate` computes the step plan and shows the effective deploy, without starting anything:

    ```sh
    bin/manent-server -c configs/manent.yml validate -f examples/descriptor.yml --offline
    ```

    `--offline` skips reading the [environment configuration](guida/configurazione-ambiente.md) (for example from Consul).

2. **Start a worker.** Deploys are run only by a worker. `serve` starts the HTTP API on `:8080` and a worker in the same process:

    ```sh
    bin/manent-server -c configs/manent.yml serve
    ```

3. **Queue the deploy** from another terminal with the `manent` client, which by default talks to the local server (`http://127.0.0.1:8080`). The command waits for the deploy to finish and prints the result:

    ```sh
    bin/manent deploy -f examples/descriptor.yml
    ```

    The example descriptor copies the artifact over SSH to `192.168.1.10`: adapt `artifact.source`, `deploy.targets` and the secrets to your environment, or use `--dry-run` to see what would happen without touching the targets.

4. **Check the status** at any time:

    ```sh
    bin/manent list
    bin/manent status <id>
    ```

The same deploy can be queued through the API, with the descriptor in JSON:

```sh
curl -X POST localhost:8080/api/v1/deployments \
  -H 'Content-Type: application/json' \
  -d @examples/descriptor.json
```

## Next steps

- Understand what just happened, then build on it: the [tutorials](tutorial/index.md), starting from [First deploy to a Linux server](tutorial/primo-deploy.md).
- The ideas behind it: [Overview](concetti/panoramica.md) and [Deploy types](concetti/tipologie-deploy.md).
- The full descriptor format: [Deploy descriptor](guida/descrittore.md).
- All commands: [`manent` client](guida/cli.md), [`manent-server`](operazioni/server.md) and [HTTP API](guida/api.md).
- Follow a deploy: `bin/manent logs -f <id>` and [Observability](operazioni/osservabilita.md).
- Take manent to production: [Installation](operazioni/installazione.md), [Configuration](operazioni/configurazione.md), [Scalability](operazioni/scalabilita.md) and [Recovery, shutdown and retention](operazioni/affidabilita.md).
