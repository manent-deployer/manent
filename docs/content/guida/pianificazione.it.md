# Deploy pianificati

La sezione `schedule` del descrittore pianifica il deploy invece di eseguirlo subito. Tutti i deploy, anche quelli pianificati, passano dalla stessa coda DBOS e ne rispettano i limiti di concorrenza.

## Deploy singolo a un orario

```yaml
schedule:
  at: 2026-10-10T02:00:00+02:00    # RFC 3339
```

Il deploy viene accodato subito in stato `DELAYED` e resta in attesa fino all'orario indicato, quando passa in coda come un deploy normale. Un orario nel passato viene rifiutato, tranne quando si reinvia un descrittore con lo stesso `id` di un deploy già accodato (ad esempio una CI che riprova dopo l'orario): in quel caso si riceve il deploy esistente con il suo stato.

Si annulla come qualsiasi deploy, con `manent cancel <id>` o con `POST /api/v1/deployments/{id}/cancel`.

## Deploy ricorrenti

```yaml
schedule:
  cron: "0 2 * * *"         # tutti i giorni alle 2:00
  timezone: Europe/Rome     # opzionale, default: fuso dell'executor
  name: myapp-nightly       # opzionale, default: <application>-<environment>
```

| Campo | Descrizione |
|---|---|
| `cron` | Espressione a 5 campi (minuto ora giorno mese giorno-settimana), a 6 campi con i secondi in testa, oppure un descrittore come `@daily` o `@hourly`. |
| `timezone` | Fuso IANA con cui interpretare l'espressione. |
| `name` | Nome della schedule. |

Comportamento:

- **creazione e aggiornamento:** inviare il descrittore crea la schedule, oppure la aggiorna se ne esiste già una con lo stesso nome. Il descrittore e il piano vengono salvati nella schedule e riusati a ogni esecuzione;
- **ID delle esecuzioni:** ogni scatto accoda un deploy con ID deterministico `<nome>@<orario UTC>`, ad esempio `myapp-nightly@20261010T000000Z`. Uno scatto non produce mai due deploy, anche con più worker attivi. Per lo stesso motivo `id` non è ammesso insieme a `cron`;
- **configurazione di ambiente:** ogni esecuzione legge quella valida al momento dell'esecuzione, non al momento della creazione della schedule;
- **propagazione delle modifiche:** le modifiche alle schedule vengono rilette dai worker ogni `dbos.schedule_poll_interval` (default 30 secondi).

```sh
manent deploy -f examples/descriptor-scheduled.yml   # crea o aggiorna
manent schedule list
manent schedule show myapp-nightly
manent list --schedule myapp-nightly                  # esecuzioni della schedule
manent schedule pause myapp-nightly
manent schedule resume myapp-nightly
manent schedule delete myapp-nightly
```

!!! note "Concorrenza"
    Lo scatto della schedule accoda il deploy senza attenderlo, quindi non occupa uno slot della coda dei deploy. Il deploy accodato rispetta `environment_concurrency` come gli altri: se un deploy dello stesso ambiente è in corso, attende.
