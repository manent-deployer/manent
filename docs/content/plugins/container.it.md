# container

Il plugin `container` rilascia un'immagine di container su server e VM via SSH, con Docker o Podman. Su ogni target scarica l'immagine, sostituisce il container dell'applicazione e attende che sia pronto. Usa la stessa connessione SSH di [remote-copy](remote-copy.md), quindi non serve un agente sui target.

## L'artefatto è un'immagine

L'artefatto è un'immagine in un registry, indicata con una sorgente `oci://`:

```yaml
artifact:
  type: container
  name: shop
  version: "1.4.0"
  source: oci://registry.example.com/team/shop:1.4.0
```

Lo step `artifacts` non scarica i layer: legge il manifest dal registry e ne calcola il digest, che diventa lo **sha256 dell'artefatto**. Da lì in avanti il plugin avvia sempre `registry.example.com/team/shop@sha256:<digest>`, mai il tag. Così:

- un deploy ripreso dopo un crash, una promozione o un rollback manuale rilasciano esattamente l'immagine verificata;
- se nel frattempo qualcuno sposta il tag, il controllo `qa` blocca la promozione: `sha256 atteso <digest>, calcolato <altro digest>`;
- nel descrittore si può fissare `artifact.sha256` al digest (senza `sha256:`) o usare direttamente `oci://registry/team/shop@sha256:<digest>`.

Sono accettati i manifest OCI e Docker v2, compresi gli indici multi-architettura. I registry che chiedono un token (Docker Hub, GHCR, Harbor, GitLab) sono gestiti in automatico. Le credenziali si configurano come per le altre [sorgenti degli artefatti](../operazioni/segreti-artefatti.md):

```yaml title="manent.yml"
artifacts:
  allow_unlisted: false
  sources:
    - prefix: oci://registry.example.com/team/
      auth: {type: basic, username: manent, secret_ref: "vault:secret/data/manent/registry#password"}
```

`oci+http://` indica un registry in chiaro, solo per sviluppo e laboratori.

## Configurazione

```yaml title="manent.yml"
plugins:
  container:
    known_hosts: ~/.ssh/known_hosts      # default
    insecure_ignore_host_key: false      # true solo in sviluppo
    artifact_types: [container]          # default
```

## Parametri del deploy

| Parametro | Obbligatorio | Descrizione |
|---|---|---|
| `ssh_key_secret` | uno dei due | Nome del segreto con la chiave privata PEM. |
| `password_secret` | uno dei due | Nome del segreto con la password SSH. |
| `runtime` | no | `docker` (default) o `podman`. |
| `sudo` | no | `true` per eseguire il runtime con `sudo -n`. Di norma l'utente è nel gruppo `docker`. |
| `container_name` | no | Nome del container. Default: il nome dell'applicazione. |
| `run_args` | no | Argomenti aggiuntivi di `run`: porte, volumi, rete, `--env-file`, limiti. Ad esempio `-p 8080:8080 --env-file /etc/shop.env`. |
| `command` | no | Argomenti passati al container dopo l'immagine. |
| `registry_username` | no | Utente per il login al registry sul target. |
| `registry_password_secret` | con `registry_username` | Segreto con la password del registry. Viene passata sullo stdin di `login`, mai sulla riga di comando. |
| `wait_timeout` | no | Attesa dell'avvio (default `60s`). |
| `stable_for` | no | Senza `HEALTHCHECK` nell'immagine: tempo in esecuzione senza riavvii per considerarlo pronto (default `5s`). |

`artifact.version` è obbligatoria e diventa l'etichetta `manent.version` del container. Ogni target deve avere `user`.

!!! warning "`run_args` e `command` sono comandi"
    Arrivano alla shell del target così come sono scritti. Vanno definiti nella [configurazione di ambiente](../guida/configurazione-ambiente.md) ed esclusi dalla `descriptor_policy`, come i comandi di remote-copy.

```yaml title="envconfig/shop/shop/prod.yml"
plugin: container
targets:
  - {host: vm1.example.com, user: deploy, labels: {region: eu-1, az: eu-1a}}
  - {host: vm2.example.com, user: deploy, labels: {region: eu-1, az: eu-1b}}
params:
  ssh_key_secret: deploy-key
  run_args: "-p 8080:8080 --env-file /etc/shop/prod.env"
  wait_timeout: 90s
secrets:
  - {name: deploy-key, ref: "vault:secret/data/shop/prod#ssh_key"}
rollback:
  enabled: true
```

## Funzionamento

Per ogni target, in sequenza:

1. legge il container in esercizio (`container inspect`): versione, immagine e stato;
2. se il container ha già la stessa immagine e versione ed è pronto, non lo tocca. Retry e recovery non riavviano l'applicazione;
3. esegue il login al registry, se configurato;
4. scarica l'immagine per digest (`pull`);
5. rimuove il container precedente e avvia il nuovo con `--restart unless-stopped`, le etichette `manent.application`, `manent.environment`, `manent.version` e `manent.deployment`, `run_args` e `command`;
6. attende che sia pronto: con un `HEALTHCHECK` nell'immagine finché è `healthy`; senza, finché resta in esecuzione per `stable_for` senza riavvii. Un container uscito o `unhealthy` fa fallire il deploy, e le ultime righe del suo log finiscono nel log del deploy.

Il deploy si ferma al primo target che fallisce. Tra la rimozione del vecchio container e l'avvio del nuovo l'applicazione su quel target non risponde: per evitarlo si usa un [rilascio a ondate](../guida/ondate.md) con `maxUnavailable` e un bilanciatore che escluda i target non pronti.

## Rollback

- **Release in esercizio** (locator `plugin`): versione e immagine lette dalle etichette del container. I target senza container vengono ignorati; release diverse tra i target fermano il deploy nel checkpoint.
- **Rollback:** su ogni target riavvia il container con l'immagine precedente, per digest, con la stessa attesa del deploy. Prosegue anche se un target fallisce. Le immagini precedenti restano in cache sul target, quindi il rollback non dipende dal registry; se mancano vengono riscaricate.

Il tipo di artefatto `container` ammette il rollback (`rollback.artifact_types.container.allowed: true` nei default).

## Dry-run

In [dry-run](../guida/dry-run.md), per ogni target: connessione SSH, `version` del runtime con i permessi dell'utente e container in esercizio. Non esegue `pull` né modifica i container.

## Risultato

```json
{
  "plugin": "container",
  "targets": [
    {
      "host": "vm1.example.com",
      "status": "deployed",
      "details": {
        "container": "shop",
        "image": "registry.example.com/team/shop@sha256:4f1c…",
        "previous_version": "1.3.0",
        "previous_image": "registry.example.com/team/shop@sha256:9ab2…",
        "state": "running healthy"
      }
    }
  ]
}
```
