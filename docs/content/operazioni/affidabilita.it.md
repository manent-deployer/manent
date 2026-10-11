# Recovery, arresto e retention

Questa pagina spiega come manent si comporta quando un worker si ferma, come si arresta un worker senza interrompere i deploy e come si contiene la crescita del database.

## Registro dei worker

Ogni worker si registra nel database di sistema (tabelle `manent.executors` e `manent.leases` su Postgres, `manent_executors` e `manent_leases` su SQLite) e aggiorna il proprio **heartbeat** ogni `recovery.heartbeat_interval`. Un worker che non invia heartbeat da `recovery.stale_after`, o che si è arrestato, è considerato **perso**.

```sh
manent-server admin executors
```

```json
{
  "version": "0.1.0",
  "executors": [
    {"id": "manent-worker-0", "version": "0.1.0", "build": "0.3.0", "hostname": "manent-worker-0",
     "started_at": "…", "heartbeat_at": "…", "registered": true, "alive": true},
    {"id": "manent-worker-1", "version": "0.1.0", "heartbeat_at": "…", "registered": true, "alive": false,
     "pending": {"0.1.0": 2}}
  ]
}
```

| Campo | Significato |
|---|---|
| `alive` | Il worker invia heartbeat. |
| `pending` | Workflow in sospeso (`PENDING`) assegnati al worker, per versione dei workflow. Solo lui, al riavvio, o una recovery li riprendono. |
| `recovered_by` | Chi ha già ripreso i workflow del worker perso. |
| `registered: false` | Worker con workflow in sospeso ma assente dal registro, ad esempio perso prima dell'introduzione del registro. |

Le stesse informazioni sono nelle metriche `manent_executor_up` e `manent_executor_heartbeat_timestamp_seconds` (vedi [Osservabilità](osservabilita.md)).

## Worker perso

All'avvio un worker riprende solo i deploy rimasti in sospeso con il proprio `executor_id`. Se un nodo non torna, ad esempio perché la macchina è stata dismessa o un pod di uno StatefulSet è stato rimosso con uno scale down, i suoi workflow restano `PENDING` e, con `environment_concurrency: 1`, bloccano anche i deploy successivi dello stesso ambiente.

Ci sono due modi per farli riprendere da un altro worker.

### Recovery automatica

```yaml title="manent.yml"
recovery:
  heartbeat_interval: 15s
  stale_after: 1m
  auto_recover_after: 10m   # 0s = disattivata (default)
```

Un worker alla volta (lease `recovery`) controlla il registro. I workflow in sospeso di un worker **della stessa versione** arrestato o senza heartbeat da `auto_recover_after` vengono rimessi in coda e ripresi da un worker qualsiasi, dall'ultimo step concluso.

### Recovery manuale

```sh
manent-server admin recover --executor manent-worker-1 --dry-run   # cosa verrebbe ripreso
manent-server admin recover --executor manent-worker-1
```

Il comando rifiuta un worker che risulta ancora attivo, uno sconosciuto al registro o uno già recuperato. `--force` salta queste verifiche: va usato solo dopo aver accertato che il nodo è davvero fermo.

### Garanzie e limiti

- **Una sola recovery per worker.** Il worker perso viene *rivendicato* nel registro con un'operazione atomica: due worker, o un worker e un operatore, non possono riprendere gli stessi workflow.
- **Nessuna sovrapposizione con il riavvio.** Un worker che riparte attende che una recovery in corso dei suoi workflow sia conclusa, al più 2 minuti, prima di avviare la propria recovery DBOS. Una recovery può rivendicare solo un worker che non invia heartbeat. Così lo stesso workflow non viene ripreso due volte.
- **Lo step interrotto viene rieseguito.** Gli step conclusi non vengono ripetuti, mentre quello in corso al momento della perdita riparte. Per questo `Deploy` e `Rollback` dei plugin devono essere idempotenti.
- **Rete partizionata.** Un nodo vivo ma isolato dal database per più di `auto_recover_after` viene considerato perso. Quando torna raggiungibile, lo step che stava eseguendo può concludersi mentre lo stesso deploy riparte altrove. Il nodo se ne accorge al primo heartbeat e lo segnala con un errore nel log: va riavviato. Per questo `auto_recover_after` deve superare di molto le interruzioni di rete attese; nel dubbio va lasciata disattivata e si usa la recovery manuale.
- **Approvazioni in attesa.** Anche i workflow di approvazione di un worker perso vengono ripresi, senza perdere le decisioni già registrate.

## Arresto ordinato (drain)

Con `SIGTERM` o `SIGINT` il worker:

1. smette di prelevare deploy dalla coda e `/readyz` risponde `503`;
2. attende fino a `dbos.drain_timeout` (default 2 minuti) che i deploy in esecuzione si concludano;
3. arresta DBOS e i plugin esterni e registra l'arresto.

Le approvazioni in attesa non vengono attese: riprendono al riavvio o, con la recovery automatica, su un altro worker. I deploy non conclusi entro il timeout restano in sospeso e vengono ripresi allo stesso modo.

Il timeout di arresto della piattaforma deve superare `drain_timeout`: `terminationGracePeriodSeconds` in Kubernetes (il [chart](installazione.md#kubernetes-helm) lo imposta a 6 minuti), `TimeoutStopSec` con systemd.

## Retention dello storico

Ogni deploy lascia nel database di sistema il workflow, gli step, gli eventi e il log. Senza pulizia il database cresce indefinitamente.

```yaml title="manent.yml"
retention:
  max_age: 2160h      # 90 giorni; 0s = nessuna pulizia (default)
  keep_releases: 10   # deploy riusciti conservati comunque per applicazione e ambiente
  interval: 1h
```

Un worker alla volta (lease `retention`) elimina ogni `interval` i workflow di manent (deploy, approvazioni, esecuzioni delle schedule) **conclusi** da più di `max_age`, con step, eventi e log. Per ogni applicazione e ambiente conserva comunque gli ultimi `keep_releases` deploy riusciti e le loro approvazioni: l'[inventario](../guida/governance.md#inventario), la promozione e il rollback manuale continuano a funzionare. Deploy in corso, in coda o pianificati non vengono mai toccati.

```sh
manent-server admin prune --dry-run                    # quanti workflow verrebbero eliminati
manent-server admin prune --max-age 720h --keep-releases 5
```

!!! warning "Audit"
    La retention elimina lo storico operativo, non l'[audit](accesso.md#audit): il file di audit e il SIEM restano il registro a lungo termine di chi ha fatto cosa. `max_age` deve essere coerente con i requisiti di conservazione dell'organizzazione.

Le [metriche DORA](osservabilita.md#metriche-dora) e la promozione di una versione vecchia vedono solo lo storico conservato.

## Backup e ripristino

Lo stato di manent è tutto nel database di sistema: deploy in corso e conclusi, code, schedule, approvazioni, log dei deploy e registro dei worker. Configurazione, configurazione di ambiente (Consul) e segreti stanno fuori e vanno salvati separatamente. La directory di lavoro dei worker contiene solo artefatti temporanei.

- **Backup.** Su Postgres conviene un backup continuo con point-in-time recovery (WAL archiving, ad esempio con pgBackRest o il servizio gestito del cloud). `pg_dump` del database va bene per volumi contenuti.
- **Ripristino.** Un database ripristinato a un momento precedente contiene i deploy che allora erano in corso o in coda. Al riavvio i worker li riprenderebbero, anche se nel frattempo erano stati completati sui target. Prima di avviare i worker su un database ripristinato:
    1. elencare i deploy in sospeso e in coda (`manent list --state PENDING`, `--state ENQUEUED`, `--state DELAYED`);
    2. annullare con `manent cancel` quelli già eseguiti dopo il punto di ripristino, confrontandoli con l'audit nel SIEM;
    3. verificare le schedule (`manent schedule list`) e sospendere quelle da non eseguire.
- **SQLite.** Il file del database (con i file `-wal` e `-shm`) va copiato a processi fermi, oppure con `sqlite3 manent.db ".backup copia.db"`.
