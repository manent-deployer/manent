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
| `--after <id>` | Avvia il deploy solo dopo il successo del deploy `<id>` (ripetibile): vedi [Catene di deploy](catene.md). |
| `--import <nome>=<id>` | Legge gli export del deploy `<id>` come import `<nome>` del descrittore e aggiunge `<id>` ad `after` (ripetibile): vedi [Export e import](catene.md#export-import). |
| `--id`, `--version`, `--source`, `--sha256` | Sostituiscono ID e campi dell'artefatto del descrittore (la pipeline dà la versione). |
| `--outputs <file>`, `--summary <file>`, `--ci` | Esiti `chiave=valore` e riepilogo Markdown per la CI: vedi [CI e infrastruttura come codice](ci-iac.md). |

I deploy pianificati (`schedule` nel descrittore) e quelli in [attesa di approvazione](governance.md#approvazioni) non vengono attesi: il comando stampa lo stato e termina.

### `validate`

Verifica il descrittore sul server con gli stessi controlli dell'accodamento (policy sui descrittori, configurazione di ambiente, callback, sorgente dell'artefatto, rollback) e mostra il piano e il deploy effettivo, cioè il descrittore fuso con la configurazione di ambiente. Non accoda nulla e non contatta segreti, artefatto o target. Richiede il permesso `deployment.read`.

```sh
manent validate -f examples/descriptor.yml
```

L'output riporta anche i nomi dei segreti dichiarati, la regola di [governance](governance.md) che si applica e l'eventuale freeze attivo. Se la configurazione di ambiente non è raggiungibile, il server lo segnala in `warnings` e la verifica resta al worker. Per verificare una configurazione di manent prima di installarla c'è [`manent-server validate`](../operazioni/server.md#validate).

### `env validate`

Verifica una configurazione di ambiente candidata prima che arrivi nel sistema di configurazione, fondendola con un descrittore di esempio (default: la richiesta del deploy in esercizio). Vedi [CI e infrastruttura come codice](ci-iac.md#env-validate).

```sh
manent env validate -a <applicazione> -e <ambiente> -f prod.yml [-d descrittore.yml]
```

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
manent inventory [-a <applicazione>] [-e <ambiente>] [--out-of-sync]
manent promote -a <applicazione> --from <ambiente> --to <ambiente> [--version <v>] [--deploy-file <yaml>]
manent rollback -a <applicazione> -e <ambiente> [--to <versione>]
manent approve <id> [-m <commento>]
manent reject <id> -m <motivo>
manent freezes [-a <applicazione>] [-e <ambiente>]
manent approvals                # deploy in attesa di una decisione che puoi prendere
manent whoami                   # la tua identità sul server e i tuoi ruoli
```

`promote` e `rollback` accettano anche `--id`, `--dry-run`, `--freeze-override <motivo>`, `--no-wait` e `--timeout`, con lo stesso significato di `deploy`.

### Tenant

Vedi [Tenant](tenant.md).

```sh
manent tenants list [-a <applicazione>] [-e <ambiente>] [--selector <selettore>]
manent tenants show <tenant>
manent deploy -f <yaml> --tenant <tenant> | --tenants <selettore> [--max-parallel <n>]
manent promote -a <applicazione> --from <amb> --to <amb> --tenant <tenant> | --tenants <selettore>
manent rollback -a <applicazione> -e <amb> --tenant <tenant> | --run <rilascio multi-tenant>
manent list|inventory|activity|dora -t <tenant>          # dora --by-tenant: metriche per tenant
manent status <rilascio multi-tenant> | cancel <rilascio multi-tenant> | approve <rilascio multi-tenant>
```

### Freeze e policy

Vedi [Policy in Rego](policy.md).

```sh
manent freeze add --name <nome> -e <ambiente> [-a <applicazione>] [--region <r>] (--for <durata> | --until <RFC 3339>) [--from <RFC 3339>] [--reason <testo>] [--hard]
manent freeze delete <nome>
manent freeze list

manent policy list
manent policy show <revisione|current>
manent policy pull <revisione|current> -d <dir>
manent policy push -d <dir> [-m <commento>]
manent policy diff <revisione> <revisione>
manent policy eval <revisione|current> -f <yaml> [--phase submit|validate|start|stage|tick] [--kind deploy|promotion|rollback]
manent policy impact <revisione> [--since 7d]
manent policy access <revisione|current> --subject <metodo:nome> [--group <g>]... [--claim k=v]... [--compare]
                     [--action <azione> -a <app> -e <env> [-t <tenant>] [-f <yaml>]] [--json]   # rbac.engine: opa
manent policy approve <revisione>        # attiva una bozza
manent policy activate <revisione>       # torna a una revisione già attiva in passato
```

## Log, timeline e metriche

```sh
manent logs [-f] [--json] <id>         # log del deploy; -f lo segue fino alla conclusione
manent steps <id>                      # step registrati con orari, durata, output ed errori
manent dora [--since 30d] [-a <applicazione>] [-e <ambiente>]
```

Vedi [Osservabilità](../operazioni/osservabilita.md).

## Release composte

```sh
manent release submit -f release.yml [--no-wait] [--timeout 2h]   # invia e segue la release
manent release status <id>
manent release list [-n 20]
manent release cancel <id>
```

`submit` stampa una riga a ogni cambio di stato di un deploy (e del suo rollback) e termina con errore se la release non riesce. Vedi [Catene di deploy](catene.md#release-composte).

## Attività e worker { #attivita-e-worker }

```sh
manent activity [-a <applicazione>] [-e <ambiente>] [-w 5s] [--json]   # deploy attivi
manent queues [-w 5s] [--json]                                            # code, limiti e worker in ascolto
manent workers [-w 5s] [--json]                                         # worker e lavoro in corso
```

`activity` mostra i deploy non ancora conclusi, raggruppati in **in esecuzione** (con il worker che li esegue, l'ultimo step registrato e l'avanzamento delle ondate), **in attesa di approvazione**, **in attesa delle dipendenze** (con i deploy ancora attesi), **in coda** (in attesa di uno slot libero dell'ambiente) e **pianificati** (`schedule.at`). Sono elencati solo i deploy che puoi leggere.

`workers` elenca i worker registrati con stato (`alive`, `stale` se manca l'heartbeat, `stopped`, `unregistered` per un executor con workflow in sospeso ma assente dal registro), region, versione dei workflow e del binario, ultimo heartbeat e i workflow in esecuzione su ciascuno: deploy, passi di un'ondata in una region, health gate, approvazioni in attesa di una decisione. Richiede il permesso [`platform.read`](../operazioni/accesso.md#permessi-rbac); i workflow delle applicazioni che non puoi leggere sono solo contati. Una seconda tabella mostra le risorse che ogni worker comunica a ogni heartbeat (CPU usata rispetto alle CPU utilizzabili, memoria del processo rispetto al limite del container o alla memoria dell'host, carico dell'host, spazio libero della workdir, deploy eseguiti insieme) e le code che ascolta.

`queues` elenca le code: quella dei deploy, partizionata per applicazione e ambiente, quella delle approvazioni e delle attese, che non occupa slot di deploy, e una coda per region con il routing per region. Per ciascuna: workflow in attesa di uno slot, in esecuzione e pianificati, l'attesa più lunga, i limiti di concorrenza e i worker attivi in ascolto. Una coda con lavoro e nessun worker in ascolto (per esempio una region con tutti i worker fermi) è la prima cosa da controllare quando i deploy non partono. Richiede `platform.read`.

Tutti e tre stampano una tabella; `--json` stampa la risposta dell'API, `-w` ripete l'elenco all'intervallo indicato fino a Ctrl-C.

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
