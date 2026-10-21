# manent-server

`manent-server` contiene l'API, il worker e le operazioni di piattaforma sul database di sistema. Gli utenti e la CI usano invece il [client `manent`](../guida/cli.md), che parla solo con l'API.

```sh
manent-server [-c <manent.yml>] <comando> [opzioni]
```

| Opzione globale | Descrizione |
|---|---|
| `-c`, `--config` | [Configurazione di manent](configurazione.md). Si può indicare anche con `MANENT_CONFIG`. Senza file valgono i default. |

## Processi

### `serve`

Avvia l'API HTTP e, nello stesso processo, un worker. È il modo più semplice per lo sviluppo e per installazioni con un solo nodo.

```sh
manent-server serve [-l :8080] [--no-worker]
```

| Opzione | Descrizione |
|---|---|
| `-l`, `--listen` | Indirizzo di ascolto (sovrascrive `http.listen`). |
| `--no-worker` | Solo API, come `api`. |

### `api`

Avvia solo l'API: accoda e interroga i deploy, ma non li esegue. Con più nodi le istanze API stanno dietro un load balancer e l'esecuzione è dei worker (vedi [Scalabilità](scalabilita.md)).

```sh
manent-server api [-l :8080]
```

### `worker`

Avvia solo il worker, senza API. Con `SIGINT` o `SIGTERM` smette di prelevare deploy e attende quelli in corso fino a `dbos.drain_timeout` ([drain](affidabilita.md#arresto-ordinato-drain)). I deploy interrotti vengono ripresi al riavvio di un worker con lo stesso `executor_id` o, con la recovery, da un altro worker. Sonde e metriche sono su `observability.listen`.

```sh
manent-server worker
```

## Operazioni di piattaforma

Lavorano direttamente sul database di sistema, quindi servono le sue credenziali (`dbos.database_url` o `DBOS_SYSTEM_DATABASE_URL`). Sono riservate agli operatori della piattaforma: chi ha accesso al database può agire su DBOS senza passare dai permessi di manent. L'identità registrata nell'audit è l'utente del sistema operativo (`cli:<utente>`).

### `admin`

```sh
manent-server admin executors                                        # worker registrati e workflow in sospeso
manent-server admin recover --executor <id> [--dry-run] [--force]   # riprende i workflow di un worker perso
manent-server admin recover --from-version <v> [--to-version <v>] [--from-start] [--dry-run]
manent-server admin prune [--max-age 2160h] [--keep-releases 10] [--dry-run]
manent-server admin specs [--hash <sha256>…] [--all]                # workflow e schedule con specifiche revocate degli step personalizzati
manent-server admin rename-artifact -a <applicazione> --from <nome> --to <nome> [--dry-run]   # rinomina un artefatto nello storico
```

`admin executors` legge il database; per vedere i worker da una postazione, tramite l'API e con i permessi di manent, si usa [`manent workers`](../guida/cli.md#attivita-e-worker) (`platform.read`). Recovery, retention e rinomine sono registrate nell'audit (`admin.recover`, `admin.prune`, `admin.rename-artifact`). `admin rename-artifact` è descritto in [Applicazioni con più artefatti](../guida/artefatti.md#rinominare-un-artefatto). `admin specs` è descritto in [Step personalizzati](../guida/step-personalizzati.md#la-specifica-nel-piano). Vedi [Recovery, arresto e retention](affidabilita.md) e [Versioni e aggiornamenti](aggiornamenti.md).

In Kubernetes si eseguono in un pod del server, che ha già configurazione e credenziali:

```sh
kubectl exec manent-worker-0 -- manent-server admin executors
```

### `audit verify`

```sh
manent-server audit verify [<file>...]
```

Verifica la [catena di hash](accesso.md#integrita-del-file) del file di audit. Senza argomenti usa `audit.file` della configurazione; con un solo file include i suoi file ruotati. La chiave HMAC si legge da `audit.hmac_key_ref`. Termina con errore alla prima voce modificata, inserita o mancante.

### `validate`

Verifica in locale una configurazione di manent e un descrittore, senza database e senza un server in esecuzione: utile prima di installare una nuova configurazione o in una pipeline che la versiona.

```sh
manent-server -c manent.yml validate -f examples/descriptor.yml [--offline]
```

Mostra il piano, il rollback, la regola di governance e, salvo `--offline`, il deploy effettivo letto dalla configurazione di ambiente (ad esempio da Consul) con gli stadi e i passi di un eventuale [rilascio a ondate](../guida/ondate.md). Per verificare un descrittore contro il server in esercizio si usa [`manent validate`](../guida/cli.md#validate).

### `version`

```sh
manent-server version
```

## Codici di uscita

Il comando termina con codice `0` se l'operazione riesce e con un codice diverso da `0` in caso di errore.
