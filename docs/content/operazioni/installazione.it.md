# Installazione

manent ha due binari:

| Binario | Dove | Contenuto |
|---|---|---|
| `manent-server` | nodi di API e worker | API, worker, operazioni di piattaforma ([manent-server](server.md)). Il plugin `remote-copy` è compilato dentro ed è disponibile anche come [binario esterno](../plugins/esterni.md) (`manent-plugin-remote-copy`). |
| `manent` | postazioni degli utenti e pipeline CI | Il [client](../guida/cli.md), che parla solo con l'API. |

## Binari

I rilasci su GitHub contengono, per amd64 e arm64:

- `manent_<versione>_<os>_<arch>`: il solo client, per Linux, macOS e Windows;
- `manent-server_<versione>_<os>_<arch>`: server, client e plugin `remote-copy`, con la configurazione d'esempio e le unità systemd, per Linux e macOS;

- `checksums.txt`, firmato con [Sigstore](https://www.sigstore.dev/) in modalità keyless;
- un SBOM SPDX per ogni archivio (`*.sbom.json`).

Verifica della firma e dell'archivio:

```sh
cosign verify-blob \
  --certificate checksums.txt.pem --signature checksums.txt.sig \
  --certificate-identity-regexp 'https://github.com/manent-deployer/manent/.github/workflows/release.yml@refs/tags/v.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
sha256sum --ignore-missing -c checksums.txt
```

Compilazione dai sorgenti: `make build` (`bin/manent` e `bin/manent-server`, versione da `git describe`) e `make plugins`.

## Container

| Immagine | Entrypoint | Contenuto |
|---|---|---|
| `ghcr.io/manent-deployer/manent-server` | `manent-server` (default `serve`) | Server, client e plugin `remote-copy`. Configurazione da `/etc/manent/manent.yml` (`MANENT_CONFIG`); porte `8080` (API) e `9090` (`observability.listen`). |
| `ghcr.io/manent-deployer/manent` | `manent` | Solo il client, per le pipeline di CI. |

```sh
docker run --rm ghcr.io/manent-deployer/manent-server:VERSION version
docker run --rm -e MANENT_SERVER=https://manent.example.com -e MANENT_TOKEN ghcr.io/manent-deployer/manent:VERSION list
```

Entrambe sono basate su `distroless/static` e girano come utente non root. Le immagini dei rilasci sono firmate con cosign:

```sh
cosign verify ghcr.io/manent-deployer/manent-server:VERSION \
  --certificate-identity-regexp 'https://github.com/manent-deployer/manent/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

Immagini locali: `make image` (server; il client con `--target client`).

## Kubernetes (Helm)

Il chart è in `deploy/helm/manent`:

```sh
kubectl create secret generic manent-db \
  --from-literal=url='postgres://manent:…@postgres:5432/manent?sslmode=verify-full'
helm install manent deploy/helm/manent -f values-prod.yaml
```

Il chart installa:

| Risorsa | Descrizione |
|---|---|
| Deployment `<release>-api` | `manent-server api`, dietro il Service `<release>-api` (porta 8080). |
| StatefulSet `<release>-worker` | Worker. Il nome del pod (`<release>-worker-0`, …) diventa l'`executor_id` (`DBOS__VMID`): è stabile tra i riavvii, quindi ogni worker riprende i propri deploy interrotti. |
| ConfigMap | `manent.yml` dalla chiave `config` dei values, con `observability.listen: ":9090"` e la `workdir`. |
| PodDisruptionBudget | Un pod alla volta per API e worker. |
| ServiceMonitor | Con `serviceMonitor.enabled`, scrape di `/metrics` di API e worker (Prometheus Operator). |

Punti da considerare:

- **Database.** L'URL del database (con la password) arriva dal Secret `database.existingSecret` tramite `DBOS_SYSTEM_DATABASE_URL`, mai dalla ConfigMap.
- **Arresto.** `worker.terminationGracePeriodSeconds` (default 360) deve superare `dbos.drain_timeout` (default del chart 5m).
- **Scale down.** Un pod rimosso non torna: i suoi workflow in sospeso vengono ripresi da un altro worker dopo `recovery.auto_recover_after` (default del chart 10m). Vedi [Recovery](affidabilita.md#worker-perso).
- **Segreti e file.** Token dell'API, chiavi SSH, `known_hosts`, CA e token Vault/Consul si montano con `extraVolumes` ed `extraVolumeMounts`; le variabili con `extraEnv`.
- **Più region.** Per i [rilasci a ondate](../guida/ondate.md#esecuzione-nelle-region) si installa una release per region nel cluster di quella region, con `api.enabled: false` (tranne dove sta l'API), `config.dbos.region: <region>` e `config.dbos.region_routing: true` su tutte, e lo stesso Secret del database.
- **Filesystem in sola lettura.** I container hanno il root filesystem in sola lettura; `workdir` e `/tmp` sono volumi `emptyDir`.

## systemd

Le unità sono in `deploy/systemd`:

```sh
sudo useradd --system --home /var/lib/manent manent
sudo install -m 0755 manent-server manent /usr/local/bin/
sudo install -d -o manent /etc/manent /var/log/manent
sudo install -m 0640 configs/manent.yml /etc/manent/manent.yml
sudo install -m 0600 -o manent deploy/systemd/manent.env.example /etc/manent/manent.env   # DBOS_SYSTEM_DATABASE_URL
sudo install -m 0644 deploy/systemd/manent-worker.service deploy/systemd/manent-api.service /etc/systemd/system/
sudo systemctl enable --now manent-worker manent-api
```

L'`executor_id` di default è l'hostname: deve restare lo stesso tra i riavvii. `TimeoutStopSec` (6 minuti) deve superare `dbos.drain_timeout`.

## Database

Per più nodi serve Postgres. All'avvio manent crea lo schema `dbos` (DBOS) e lo schema `manent` (registro dei worker): i worker sempre, l'API e `admin` solo se lo schema `dbos` manca. L'utente deve poterli creare, oppure vanno creati in anticipo. Per backup e ripristino vedi [Backup e ripristino](affidabilita.md#backup-e-ripristino).
