# Repository degli artefatti

Il worker scarica ogni artefatto dalla sua `artifact.source` e ne verifica lo sha256. `artifacts.sources` in `manent.yml` stabilisce quali sorgenti sono ammesse e quali credenziali vanno a ciascuna: il descrittore porta solo l'URL, mai una password.

!!! abstract "Cosa serve"
    Un account in sola lettura su ogni repository · le sue credenziali nel gestore dei segreti · la CA aziendale, se i repository la usano

## 1. Ammetti solo i tuoi repository

```yaml title="manent.yml"
artifacts:
  timeout: 30m
  allow_unlisted: false          # tutto il resto viene rifiutato all'invio (422) e al download
  sources:
    - prefix: https://nexus.example.com/repository/releases/
      auth: {type: basic, username: manent, secret_ref: "vault:secret/data/manent/nexus#password"}
```

- Vince il **prefisso più lungo** che corrisponde, quindi un prefisso più specifico può avere credenziali diverse.
- Le credenziali vengono lette a ogni download e non vengono mai inoltrate su un redirect verso un altro host.
- Un prefisso copre solo ciò che sta sotto: `https://repo.example.com` non copre `https://repo.example.com.evil.example/`, e un URL con credenziali prima dell'host o con segmenti `.`/`..` nel percorso (anche codificati) è rifiutato, così non può uscire dal prefisso sul server.
- Con `allow_unlisted: true` (il default, solo per lo sviluppo) un descrittore potrebbe far scaricare al worker qualunque URL raggiungibile dalla sua rete, o copiare sui target un file locale del worker.

## 2. Repository per repository

=== "Sonatype Nexus"

    Crea un utente con il privilegio `nx-repository-view-*-*-read` sui repository delle release (o un ruolo dedicato), poi:

    ```yaml
    - prefix: https://nexus.example.com/repository/releases/
      auth: {type: basic, username: manent, secret_ref: "vault:secret/data/manent/nexus#password"}
    ```

=== "JFrog Artifactory"

    Crea un access token con permesso di lettura sui repository. Invialo come bearer token, o come header di API key nelle installazioni più vecchie:

    ```yaml
    - prefix: https://acme.jfrog.io/artifactory/releases-local/
      auth: {type: bearer, secret_ref: "vault:secret/data/manent/artifactory#token"}
    # - prefix: https://artifactory.example.com/artifactory/
    #   auth: {type: header, header: X-JFrog-Art-Api, secret_ref: "vault:secret/data/manent/artifactory#api_key"}
    ```

=== "Pacchetti GitLab o GitHub"

    Un deploy token (GitLab) o un token fine-grained con lettura dei pacchetti (GitHub):

    ```yaml
    - prefix: https://gitlab.example.com/api/v4/projects/42/packages/generic/
      auth: {type: header, header: DEPLOY-TOKEN, secret_ref: "vault:secret/data/manent/gitlab#deploy_token"}
    ```

=== "Registry di container (OCI)"

    Per gli artefatti `oci://` manent legge solo il manifest dell'immagine e ne usa il digest come sha256; l'immagine la scarica il target (Docker, Kubernetes, ECS…). Le credenziali `basic` servono anche a ottenere un token del registry quando il registry lo richiede (Harbor, GitLab, GHCR, Docker Hub).

    ```yaml
    - prefix: oci://registry.example.com/shop/
      auth: {type: basic, username: robot$manent, secret_ref: "vault:secret/data/manent/harbor#token"}
    # - prefix: oci+http://registry.lab:5000/     # registry in HTTP in chiaro, solo per i laboratori
    ```

=== "Directory locali o condivise"

    ```yaml
    - prefix: file:///srv/artifacts/
    ```

    I percorsi vengono normalizzati in `file://` più il percorso assoluto ripulito, quindi `../` non può uscire dal prefisso.

## 3. CA private

```yaml
- prefix: https://nexus.internal/repository/releases/
  ca_file: /etc/ssl/certs/corp-ca.pem
  auth: {type: basic, username: manent, secret_ref: "vault:secret/data/manent/nexus#password"}
```

## 4. Fissa ciò che rilasci

Pubblica lo sha256 dalla CI e passalo a manent, così lo step `qa` rifiuta qualunque altro file, anche con lo stesso nome e la stessa versione:

```sh
manent deploy -f deploy/test.yml --version "$TAG" \
  --source "https://nexus.example.com/repository/releases/shop/shop-$TAG.tgz" \
  --sha256 "$(sha256sum dist/shop-$TAG.tgz | cut -d' ' -f1)"
```

Le promozioni lo fissano in automatico: la produzione riceve esattamente i byte riusciti nell'ambiente precedente. Per le immagini, riferiscile per digest (`oci://registry.example.com/shop/api@sha256:…`) o lascia che manent fissi il tag al digest che ha letto.

## Verifica

```sh
manent validate -f deploy/test.yml     # una sorgente fuori da artifacts.sources viene rifiutata qui
manent validate -f deploy/test.yml --check-artifact   # l'artefatto esiste e le credenziali funzionano
manent deploy -f deploy/test.yml --dry-run
```

`--check-artifact` chiede al server API di trovare l'artefatto senza scaricarlo (`HEAD`, o il manifest per un'immagine, il cui digest viene confrontato con `artifact.sha256`). Lo step `artifacts` del dry-run scarica l'artefatto con le credenziali configurate e riporta `sha256` e `size`.

Riferimento: [Segreti e sorgenti degli artefatti](../operazioni/segreti-artefatti.md).
