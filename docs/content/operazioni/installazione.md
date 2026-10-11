# Installation

manent has two binaries:

| Binary | Where | Contents |
|---|---|---|
| `manent-server` | API and worker nodes | API, workers, platform operations ([manent-server](server.md)). The `remote-copy` plugin is compiled in and is also available as an [external binary](../plugins/esterni.md) (`manent-plugin-remote-copy`). |
| `manent` | user workstations and CI pipelines | The [client](../guida/cli.md), which talks only to the API. |

## Binaries

GitHub releases contain, for amd64 and arm64:

- `manent_<version>_<os>_<arch>`: the client only, for Linux, macOS and Windows;
- `manent-server_<version>_<os>_<arch>`: server, client and `remote-copy` plugin, with the example configuration and the systemd units, for Linux and macOS;

- `checksums.txt`, signed with [Sigstore](https://www.sigstore.dev/) in keyless mode;
- an SPDX SBOM for each archive (`*.sbom.json`).

Verifying the signature and the archive:

```sh
cosign verify-blob \
  --certificate checksums.txt.pem --signature checksums.txt.sig \
  --certificate-identity-regexp 'https://github.com/joeg-ita/manent/.github/workflows/release.yml@refs/tags/v.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
sha256sum --ignore-missing -c checksums.txt
```

Building from source: `make build` (`bin/manent` and `bin/manent-server`, version from `git describe`) and `make plugins`.

## Containers

| Image | Entrypoint | Contents |
|---|---|---|
| `ghcr.io/joeg-ita/manent-server` | `manent-server` (default `serve`) | Server, client and `remote-copy` plugin. Configuration from `/etc/manent/manent.yml` (`MANENT_CONFIG`); ports `8080` (API) and `9090` (`observability.listen`). |
| `ghcr.io/joeg-ita/manent` | `manent` | The client only, for CI pipelines. |

```sh
docker run --rm ghcr.io/joeg-ita/manent-server:VERSION version
docker run --rm -e MANENT_SERVER=https://manent.example.com -e MANENT_TOKEN ghcr.io/joeg-ita/manent:VERSION list
```

Both are based on `distroless/static` and run as a non-root user. Release images are signed with cosign:

```sh
cosign verify ghcr.io/joeg-ita/manent-server:VERSION \
  --certificate-identity-regexp 'https://github.com/joeg-ita/manent/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

Local images: `make image` (server; the client with `--target client`).

## Kubernetes (Helm) { #kubernetes-helm }

The chart is in `deploy/helm/manent`:

```sh
kubectl create secret generic manent-db \
  --from-literal=url='postgres://manent:…@postgres:5432/manent?sslmode=verify-full'
helm install manent deploy/helm/manent -f values-prod.yaml
```

The chart installs:

| Resource | Description |
|---|---|
| Deployment `<release>-api` | `manent-server api`, behind the `<release>-api` Service (port 8080). |
| StatefulSet `<release>-worker` | Workers. The pod name (`<release>-worker-0`, …) becomes the `executor_id` (`DBOS__VMID`): it is stable across restarts, so each worker resumes its own interrupted deploys. |
| ConfigMap | `manent.yml` from the `config` key of the values, with `observability.listen: ":9090"` and the `workdir`. |
| PodDisruptionBudget | One pod at a time for API and workers. |
| ServiceMonitor | With `serviceMonitor.enabled`, scrape of `/metrics` of API and workers (Prometheus Operator). |

Things to consider:

- **Database.** The database URL (with the password) comes from the `database.existingSecret` Secret through `DBOS_SYSTEM_DATABASE_URL`, never from the ConfigMap.
- **Shutdown.** `worker.terminationGracePeriodSeconds` (default 360) must exceed `dbos.drain_timeout` (chart default 5m).
- **Scale down.** A removed pod does not come back: its pending workflows are resumed by another worker after `recovery.auto_recover_after` (chart default 10m). See [Recovery](affidabilita.md#worker-perso).
- **Secrets and files.** API tokens, SSH keys, `known_hosts`, CAs and Vault/Consul tokens are mounted with `extraVolumes` and `extraVolumeMounts`; variables with `extraEnv`.
- **Several regions.** For [wave rollouts](../guida/ondate.md#esecuzione-nelle-region) install one release per region in that region's cluster, with `api.enabled: false` (except where the API lives), `config.dbos.region: <region>` and `config.dbos.region_routing: true` on all of them, and the same database Secret.
- **Read-only filesystem.** Containers have a read-only root filesystem; `workdir` and `/tmp` are `emptyDir` volumes.

## systemd

The units are in `deploy/systemd`:

```sh
sudo useradd --system --home /var/lib/manent manent
sudo install -m 0755 manent-server manent /usr/local/bin/
sudo install -d -o manent /etc/manent /var/log/manent
sudo install -m 0640 configs/manent.yml /etc/manent/manent.yml
sudo install -m 0600 -o manent deploy/systemd/manent.env.example /etc/manent/manent.env   # DBOS_SYSTEM_DATABASE_URL
sudo install -m 0644 deploy/systemd/manent-worker.service deploy/systemd/manent-api.service /etc/systemd/system/
sudo systemctl enable --now manent-worker manent-api
```

The default `executor_id` is the hostname: it must stay the same across restarts. `TimeoutStopSec` (6 minutes) must exceed `dbos.drain_timeout`.

## Database

Several nodes need Postgres. At startup manent creates the `dbos` schema (DBOS) and the `manent` schema (worker registry). The user must be able to create them, or they must be created in advance. For backup and restore see [Backup and restore](affidabilita.md#backup-e-ripristino).
