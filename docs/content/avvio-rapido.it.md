# Avvio rapido

## Requisiti

- Go 1.27 o successivo. Il `go.mod` fissa la toolchain 1.27.x: con una versione di Go più vecchia viene scaricata automaticamente (`GOTOOLCHAIN=auto`).
- Nessun database da installare per provare manent: di default DBOS usa un file SQLite (`manent.db`). In produzione serve Postgres.

## Compilare

```sh
make build      # bin/manent (client) e bin/manent-server
make plugins    # facoltativo: plugin esterni in bin/ (manent-plugin-remote-copy)
```

## Primo deploy

1. **Controlla il descrittore.** `validate` calcola il piano degli step e mostra il deploy effettivo, senza avviare nulla:

    ```sh
    bin/manent-server -c configs/manent.yml validate -f examples/descriptor.yml --offline
    ```

    `--offline` evita di leggere la [configurazione di ambiente](guida/configurazione-ambiente.md) (ad esempio da Consul).

2. **Avvia un worker.** I deploy vengono eseguiti solo da un worker. `serve` avvia l'API HTTP su `:8080` e un worker nello stesso processo:

    ```sh
    bin/manent-server -c configs/manent.yml serve
    ```

3. **Accoda il deploy** da un altro terminale con il client `manent`, che di default parla con il server locale (`http://127.0.0.1:8080`). Il comando attende la fine del deploy e ne stampa il risultato:

    ```sh
    bin/manent deploy -f examples/descriptor.yml
    ```

    Il descrittore d'esempio copia l'artefatto via SSH su `192.168.1.10`: adatta `artifact.source`, `deploy.targets` e i segreti al tuo ambiente, oppure usa `--dry-run` per vedere cosa succederebbe senza modificare i target.

4. **Controlla lo stato** in qualsiasi momento:

    ```sh
    bin/manent list
    bin/manent status <id>
    ```

Lo stesso deploy si può accodare via API, con il descrittore in JSON:

```sh
curl -X POST localhost:8080/api/v1/deployments \
  -H 'Content-Type: application/json' \
  -d @examples/descriptor.json
```

## Passi successivi

- Capisci cosa è appena successo e costruisci su questo: i [tutorial](tutorial/index.md), a partire da [Primo deploy su un server Linux](tutorial/primo-deploy.md).
- Le idee alla base: [Panoramica](concetti/panoramica.md) e [Tipologie di deploy](concetti/tipologie-deploy.md).
- Il formato completo del descrittore: [Descrittore di deploy](guida/descrittore.md).
- Tutti i comandi: [client `manent`](guida/cli.md), [`manent-server`](operazioni/server.md) e [API HTTP](guida/api.md).
- Seguire un deploy: `bin/manent logs -f <id>` e [Osservabilità](operazioni/osservabilita.md).
- Portare manent in produzione: [Installazione](operazioni/installazione.md), [Configurazione](operazioni/configurazione.md), [Scalabilità](operazioni/scalabilita.md) e [Recovery, arresto e retention](operazioni/affidabilita.md).
