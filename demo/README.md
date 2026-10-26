# manent demo

One container with everything needed to watch manent release software: the server with its web interface, a worker, SQLite and five hosts reached over SSH.

```sh
docker build -f demo/Dockerfile -t manent-demo .     # from the repository root
docker run --rm -p 8080:8080 -p 9000:9000 --name manent-demo manent-demo
```

Open http://localhost:8080: the demo identity provider on port 9000 signs you in as John Doe, with no password. At startup the container replays a short release history (`seed.sh`): three applications in staging and prod, with promotions. Then try:

```sh
docker exec -it manent-demo manent deploy -f /demo/deploys/shop-1.3.0.yml   # canary, then one region at a time
docker exec -it manent-demo manent deploy -f /demo/deploys/shop-1.4.0.yml   # no /healthz: the canary gate fails, rollback
docker exec -it manent-demo manent logs <id> --level warn
docker exec -it manent-demo manent inventory
docker exec -it manent-demo manent rollback -a shop -e prod
```

`MANENT_DEMO_SEED=false` starts with an empty history.

## How it is put together

| Path | What it is |
|---|---|
| `manent.yml` | Server configuration: SQLite, one user (John Doe: a static token for the CLI, OIDC for the web interface), environment configuration from files, remote-copy plugin. |
| `envconfig/<app>/<env>.yml` | Targets, plugin parameters and, for `shop` in prod, the wave rollout with its health gate. |
| `deploys/` | The descriptors you submit: just application, environment and artifact. |
| `entrypoint.sh` | Starts the identity provider (`test/usecases/oidc` with `-user`) and creates the hosts (`staging-1`, `web-eu-1a`, `web-eu-1b`, `web-us-1a`, `web-us-1b`): one loopback address, one SSH user and one HTTP server on port 8081 each. |
| `artifacts.sh` | Builds the artifacts: a page and `/healthz` per version; `shop` 1.4.0 has no `/healthz`. |
| `record/` | Regenerates the README GIF: `make-gif.sh` (needs agg, ffmpeg, Node with `web/node_modules`, Google Chrome). |

The demo signs anyone in as an administrator, its token is public and its data lives in the container: it is not a starting point for a real installation. See the [installation guide](https://manent-deployer.github.io/manent/operazioni/installazione/) for that.
