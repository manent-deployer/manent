# CLI

manent ha due binari:

| Binario | Per chi | Cosa fa |
|---|---|---|
| `manent` | Sviluppatori, release manager, pipeline CI | Client: legge descrittori YAML e parla **solo con l'API** del server. Non contiene il motore, i plugin né i driver di database. |
| `manent-server` | Piattaforma | API, worker e operazioni sul database. Vedi [manent-server](../operazioni/server.md). |

Questa pagina descrive il client.

```sh
manent [-c <client.yml>] [--server <url>] [--token-file <file>] [--ca-file <pem>] <comando> [opzioni]
```

## Configurazione del client

Ogni comando passa dall'API: valgono sempre l'[autenticazione, i permessi e l'audit](../operazioni/accesso.md) del server.

```yaml title="~/.config/manent/client.yml"
server: https://manent.example.com
token_file: ~/.config/manent/token    # token OIDC o statico
ca_file: /etc/ssl/certs/ca-aziendale.pem   # solo se la CA del server non è tra quelle di sistema
timeout: 60s                          # di ogni richiesta
```

| Impostazione | Opzione | Variabile | File | Default |
|---|---|---|---|---|
| File di configurazione | `-c`, `--config` | `MANENT_CLIENT_CONFIG` | | `$XDG_CONFIG_HOME/manent/client.yml` o `~/.config/manent/client.yml`, se esiste |
| Server | `--server` | `MANENT_SERVER` | `server` | `http://127.0.0.1:8080` (`manent-server serve` in locale) |
| Token | | `MANENT_TOKEN` | | |
| File del token | `--token-file` | `MANENT_TOKEN_FILE` | `token_file` | |
| CA del server | `--ca-file` | `MANENT_CA_FILE` | `ca_file` | CA di sistema |

L'opzione prevale sulla variabile, che prevale sul file. Per il token: `--token-file` (o `MANENT_TOKEN_FILE`), poi `MANENT_TOKEN`, poi `token_file` del file. Il token non si passa mai come opzione, perché resterebbe nella cronologia della shell.

```sh
export MANENT_SERVER=https://manent.example.com
export MANENT_TOKEN=…
manent deploy -f release.yml
```

In una pipeline di CI si può usare l'immagine `ghcr.io/joeg-ita/manent`, che contiene solo il client.

## Deploy

### `deploy`

Accoda un deploy da descrittore YAML e, di default, ne attende la conclusione.

```sh
manent deploy -f examples/descriptor.yml [--no-wait] [--timeout 30m] [--dry-run]
```

| Opzione | Descrizione |
|---|---|
| `-f`, `--file` | Descrittore YAML (obbligatorio). |
| `--no-wait` | Stampa l'ID e termina subito, senza attendere. |
| `--timeout` | Tempo massimo di attesa (es. `30m`). Scaduto il tempo il comando termina, ma il deploy prosegue. |
| `--dry-run` | Esegue il deploy in [simulazione](dry-run.md). |

I deploy pianificati (`schedule` nel descrittore) e quelli in [attesa di approvazione](governance.md#approvazioni) non vengono attesi: il comando stampa lo stato e termina.

### `validate`

Verifica il descrittore sul server con gli stessi controlli dell'accodamento (policy sui descrittori, configurazione di ambiente, callback, sorgente dell'artefatto, rollback) e mostra il piano e il deploy effettivo, cioè il descrittore fuso con la configurazione di ambiente. Non accoda nulla e non contatta segreti, artefatto o target. Richiede il permesso `deployment.read`.

```sh
manent validate -f examples/descriptor.yml
```

L'output riporta anche i nomi dei segreti dichiarati, la regola di [governance](governance.md) che si applica e l'eventuale freeze attivo. Se la configurazione di ambiente non è raggiungibile, il server lo segnala in `warnings` e la verifica resta al worker. Per verificare una configurazione di manent prima di installarla c'è [`manent-server validate`](../operazioni/server.md#validate).

### `status`

```sh
manent status <id>
```

Stampa stato, date e, per i deploy conclusi, il risultato di ogni step o l'errore.

### `list`

```sh
manent list [-s <schedule>] [-a <applicazione>] [-e <ambiente>] [--state SUCCESS] [-n 20]
```

Elenca i deploy dal più recente, compresi quelli in attesa di approvazione o respinti. Con `--schedule` mostra solo le esecuzioni di una schedule ricorrente. I filtri (e i permessi) non riducono il numero di risultati: l'elenco prosegue sui deploy più vecchi fino a `-n`.

### `cancel`

```sh
manent cancel <id>
```

Un deploy in coda, pianificato o in attesa di approvazione viene rimosso. Un deploy in esecuzione si ferma all'inizio dello step successivo: lo step in corso termina comunque.

## Governance

Vedi [Governance dei rilasci](governance.md).

```sh
manent inventory [-a <applicazione>] [-e <ambiente>]
manent promote -a <applicazione> --from <ambiente> --to <ambiente> [--version <v>] [--deploy-file <yaml>]
manent rollback -a <applicazione> -e <ambiente> [--to <versione>]
manent approve <id> [-m <commento>]
manent reject <id> -m <motivo>
manent freezes [-a <applicazione>] [-e <ambiente>]
```

`promote` e `rollback` accettano anche `--id`, `--dry-run`, `--freeze-override <motivo>`, `--no-wait` e `--timeout`, con lo stesso significato di `deploy`.

## Log, timeline e metriche

```sh
manent logs [-f] [--json] <id>         # log del deploy; -f lo segue fino alla conclusione
manent steps <id>                      # step registrati con orari, durata, output ed errori
manent dora [--since 30d] [-a <applicazione>] [-e <ambiente>]
```

Vedi [Osservabilità](../operazioni/osservabilita.md).

## Schedule

```sh
manent schedule list
manent schedule show <nome>
manent schedule pause <nome>
manent schedule resume <nome>
manent schedule delete <nome>
```

Le schedule si creano con un descrittore che contiene `schedule.cron`. Vedi [Deploy pianificati](pianificazione.md).

## Versione

```sh
manent version     # versione, commit e data di build del client
```

## Codici di uscita

Il comando termina con codice `0` se l'operazione riesce e con un codice diverso da `0` in caso di errore, incluso un deploy concluso con errore quando `deploy` ne attende l'esito.
