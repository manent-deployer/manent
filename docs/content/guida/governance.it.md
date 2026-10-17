# Governance dei rilasci

manent tiene traccia di cosa è in esercizio in ogni ambiente e applica le regole con cui un artefatto ci arriva:

- **inventario:** la release in esercizio per applicazione e ambiente;
- **promozione obbligatoria:** in un ambiente si rilascia solo ciò che è già passato da un altro;
- **approvazioni:** il deploy parte solo dopo l'approvazione di altre persone;
- **finestre di freeze:** periodi in cui i deploy sono bloccati, con deroga per le emergenze;
- **rollback manuale:** si torna a una versione precedente con un comando;
- **change management:** un ticket in ServiceNow o Jira per ogni deploy, con l'eventuale attesa della sua approvazione (`change` nella regola, vedi [Chat e change management](integrazioni.md#change-management)).

Le regole sono nella sezione `governance` della [configurazione](../operazioni/configurazione.md) e valgono per CLI e API.

```yaml title="manent.yml"
governance:
  rules:
    - name: pagamenti
      applications: ["pay-*"]
      environments: [prod]
      requires: [preprod]
      approvals: {required: 2, timeout: 48h}
      change: {approval: true}       # ticket di change approvato nell'ITSM
    - name: produzione
      applications: ["*"]
      environments: [prod]
      requires: [preprod]
      approvals: {required: 1}
  freezes:
    - name: fine-anno
      reason: chiusura contabile
      applications: ["*"]
      environments: [prod]
      from: 2026-12-20T00:00:00+01:00
      to: 2027-01-07T00:00:00+01:00
    - name: weekend
      applications: ["*"]
      environments: [prod]
      cron: "0 18 * * FRI"
      duration: 62h
      timezone: Europe/Rome
```

Per ogni deploy vale **la prima regola** (in ordine) la cui `applications` e `environments` corrispondono. Senza regole non ci sono vincoli. I [dry-run](dry-run.md) non sono soggetti alle regole, perché non modificano gli ambienti.

## Inventario

L'inventario si ricostruisce dallo storico dei deploy: per ogni applicazione e ambiente, l'ultimo deploy concluso con successo.

```sh
manent inventory [-a myapp] [-e prod]
```

```json
[{
  "application": "myapp",
  "environment": "prod",
  "current": {
    "id": "7c0e…", "state": "SUCCESS", "artifact": "myapp", "version": "1.4.0",
    "sha256": "2d27fbdf…", "kind": "promotion", "source": "b3a1…",
    "requested_by": "token:ci", "updated_at": "2026-10-10T09:12:00Z"
  },
  "last_failure": {"id": "91aa…", "state": "ERROR", "version": "1.5.0", "rollback": "completed"}
}]
```

| Campo | Significato |
|---|---|
| `current` | Ultimo deploy riuscito: versione, `sha256` dell'artefatto effettivamente rilasciato, chi l'ha richiesto. |
| `kind` | `deploy`, `promotion` o `rollback`. Per promozioni e rollback `source` è il deploy da cui derivano. |
| `last_failure` | Presente se dopo `current` c'è un deploy fallito o annullato. Con `rollback: completed` l'ambiente è tornato a `current`; altrimenti il suo stato va verificato. |
| `targets`, `out_of_sync` | Stato letto dai target, se la riconciliazione è attiva: vedi [stato reale dei target](#stato-reale). |
| `drift` | `true` se `last_failure` è un [rilascio a ondate](ondate.md) fermato dopo aver aggiornato una parte delle region o AZ (`rollout_progress`, es. `2/4`): l'ambiente ha versioni diverse. |

Lo storico completo di un ambiente si legge con l'elenco dei deploy:

```sh
manent list -a myapp -e prod --state SUCCESS -n 50
```

!!! note
    L'inventario si ricostruisce dagli ultimi 10.000 deploy all'avvio del server e ogni 10 minuti, e nel frattempo segue i deploy nuovi e quelli in corso. Un'applicazione che non viene rilasciata da molto più tempo può non comparire: in quel caso fa fede il sistema di destinazione.

## Stato reale dei target { #stato-reale }

L'inventario dice cosa manent ha rilasciato. Con `reconcile.interval` (vedi [configurazione](../operazioni/configurazione.md#reconcile)) i worker leggono anche cosa c'è davvero sui target, e segnalano quelli cambiati fuori da manent: un deploy manuale, un rollback a mano, un nodo ricostruito con un'immagine vecchia.

- A ogni intervallo, per ogni applicazione e ambiente con una release in esercizio, il worker prende la richiesta del deploy in `current`, la fonde con la configurazione di ambiente **attuale** (così vede i target aggiunti o tolti dopo il deploy) e chiede al plugin la release in esercizio **un target alla volta**, con `reconcile.concurrency` letture insieme.
- Gli ambienti con un deploy in corso o in attesa di approvazione dentro un rollout vengono saltati: i loro target stanno cambiando.
- La lettura non modifica nulla: è la stessa che il rollback fa prima del deploy (`CurrentRelease`). Serve un plugin che la supporti: `remote-copy` con `layout: releases`, `container`, `kubernetes`, `ecs`, `lambda`, `cloudrun`, `appservice`, `containerapps`, `jboss`, `windows` e i plugin esterni che dichiarano `current_release`.
- Un solo worker per volta legge (lease `reconcile`). Con `dbos.region_routing` ogni region ha il proprio lease e i suoi worker leggono solo i target della propria region; i target senza region li legge un worker senza region.

L'inventario riporta il risultato in `targets`, e `out_of_sync: true` se almeno un target è `drifted` o `missing`:

```json
{
  "application": "myapp", "environment": "prod",
  "current": {"id": "7c0e…", "version": "1.4.0"},
  "out_of_sync": true,
  "targets": {
    "checked_at": "2026-10-17T09:20:00Z", "deployment": "7c0e…", "version": "1.4.0",
    "targets": [
      {"host": "web-1", "region": "eu-1", "az": "eu-1a", "version": "1.4.0", "state": "in-sync"},
      {"host": "web-2", "region": "eu-1", "az": "eu-1b", "version": "1.3.2", "state": "drifted"}
    ]
  }
}
```

| `state` | Significato |
|---|---|
| `in-sync` | Il target ha la versione di `current`. |
| `drifted` | Il target ha un'altra versione. |
| `missing` | Il target non ha alcuna release (ad esempio un nodo nuovo, mai rilasciato). |
| `standby` | Nei [rilasci per percentuale](traffico.md) con il router dell'ambiente: lo slot senza traffico, con la versione sostituita. Non è un'anomalia. |
| `unknown` | Il target non è leggibile (`detail` riporta l'errore, ad esempio SSH irraggiungibile). |

Una lettura conta solo per il deploy con cui è stata fatta: dopo un nuovo deploy l'inventario non mostra `targets` finché la riconciliazione successiva non ha confrontato i target con la nuova versione. Se l'intero ambiente non è leggibile (plugin senza `CurrentRelease`, configurazione di ambiente non valida) `targets.error` spiega perché.

```sh
manent inventory --out-of-sync      # solo gli ambienti con target cambiati fuori da manent
```

Ogni target che passa a `drifted` o `missing` produce un log di livello warning (`target cambiato fuori da manent`, con host, versione attesa e trovata), inviato anche al SIEM; il ritorno in linea un log informativo. La metrica `manent_targets` conta i target per stato (vedi [Osservabilità](../operazioni/osservabilita.md)). Nell'interfaccia web la cella dell'inventario elenca i target cambiati.

## Promozione { #promozione }

`requires` elenca gli ambienti in cui **lo stesso artefatto** deve essere già stato rilasciato con successo: stessa applicazione, stesso nome e stessa versione e, se il descrittore indica `artifact.sha256`, stesso hash. Un deploy che non rispetta la regola viene rifiutato all'accodamento (`422`):

```text
invalid request: myapp 2.0.0 was not released successfully in preprod: it must be promoted from there (governance)
```

Il modo più semplice di rispettarla è promuovere:

```sh
manent promote -a myapp --from preprod --to prod [--version 1.4.0] [--deploy-file deploy-prod.yml]
```

La promozione:

1. trova in `--from` l'ultimo deploy riuscito (o quello della versione indicata);
2. ne riprende il descrittore, cambia l'ambiente e fissa `artifact.sha256` all'hash registrato: lo step `qa` verifica che venga rilasciato esattamente lo stesso file;
3. **non copia `deploy.targets`**, che sono propri di ogni ambiente: arrivano dalla [configurazione di ambiente](configurazione-ambiente.md) di destinazione o da `--deploy-file`, un file YAML con la sezione `deploy` (`policy`, `plugin`, `targets`, `params`) che sostituisce quella di origine;
4. accoda il nuovo deploy con `kind: promotion`, soggetto come tutti a policy, approvazioni e freeze.

| Permesso | Ambiente |
|---|---|
| `deployment.read` | origine |
| `deployment.submit` (o `deployment.dry-run` con `--dry-run`) | destinazione |

## Approvazioni

Con `approvals.required` il deploy non viene eseguito subito: resta `AWAITING_APPROVAL` finché non riceve il numero richiesto di approvazioni da **identità diverse**.

| Chiave | Default | Descrizione |
|---|---|---|
| `required` | `0` | Approvazioni necessarie. |
| `allow_self` | `false` | Ammette l'approvazione di chi ha richiesto il deploy. |
| `timeout` | `72h` | Scaduto il tempo la richiesta passa a `EXPIRED` e il deploy non viene eseguito. |
| `when` | `before` | `before`: approvazioni prima del deploy. `rollout`: nessuna approvazione prima del deploy, quelle della regola si chiedono nei punti di approvazione del rollout (vedi sotto). |

Con un [rilascio a ondate](ondate.md#approvazioni-e-freeze) o un [rilascio per percentuale](traffico.md) che ha `approvalsBetween`, una regola con `approvals` chiede per default le approvazioni due volte: prima del deploy e in ogni punto del rollout. Con `when: rollout` la prima ondata (o la candidata a peso 0) parte senza approvazione, e le approvazioni della regola (numero, `allow_self`, `timeout`) si chiedono solo nei punti elencati in `approvalsBetween`. Un deploy soggetto a una regola così deve avere un rollout che si ferma davvero: una voce di `approvalsBetween` dopo un'ondata che non sia l'ultima, o un gradino del [rilascio per percentuale](traffico.md). Altrimenti viene rifiutato all'invio (`422`), o fallisce nello step `config`, prima di toccare i target, se nel frattempo la configurazione di ambiente è cambiata. Il primo rilascio per percentuale non ha una versione stabile e salta i gradini intermedi: l'approvazione viene allora chiesta prima che la candidata riceva traffico. L'approvazione del change nell'ITSM (`change.approval`) resta prima del deploy.

```sh
manent deploy -f prod.yml
# deploy in attesa di approvazione: manent approve 7c0e…

manent list --state AWAITING_APPROVAL       # richieste in attesa
manent approve 7c0e… -m "CHG-1234"          # chi ha il permesso deployment.approve
manent reject 7c0e… -m "manca il change"    # un rifiuto chiude la richiesta
```

Lo stato del deploy riporta le decisioni:

```json
{
  "id": "7c0e…", "state": "AWAITING_APPROVAL", "requested_by": "token:ci",
  "approval": {
    "status": "pending", "required": 2, "expires_at": "2026-10-13T09:00:00Z",
    "decisions": [{"approver": "oidc:anna", "approve": true, "comment": "CHG-1234", "time": "…"}]
  }
}
```

| Stato | Significato |
|---|---|
| `AWAITING_APPROVAL` | In attesa delle approvazioni. |
| `REJECTED` | Respinto: il deploy non viene eseguito. |
| `EXPIRED` | Non approvato entro il timeout. |
| `CANCELLED` | Ritirato con `manent cancel`. |

Raggiunte le approvazioni, il deploy viene accodato **con lo stesso ID** e prosegue come gli altri. Un [rilascio a ondate](ondate.md#approvazioni-e-freeze) può chiedere anche approvazioni tra un'ondata e l'altra (`approvalsBetween`): si danno con lo stesso `manent approve`. Se era pianificato con `schedule.at` attende comunque l'orario.

Regole:

- **Permesso.** Approvare e respingere richiede `deployment.approve` sull'applicazione e sull'ambiente, concesso dal ruolo predefinito `approver`. I ruoli `deployer` e `operator` non lo includono.
- **Auto-approvazione.** Chi ha richiesto il deploy non può approvarlo (`403`), salvo `allow_self: true`. Può però respingerlo, per ritirarlo.
- **Una decisione per persona.** Una seconda decisione della stessa identità è rifiutata.
- **Schedule ricorrenti.** Se l'ambiente richiede approvazioni quando la schedule viene creata, ogni esecuzione resta in attesa di approvazione.
- **Regola al momento della richiesta.** Le approvazioni richieste sono quelle valide quando il deploy viene accodato.
- **Coda separata.** L'attesa avviene su una coda dedicata (`<dbos.queue>-approvals`): non occupa slot dei worker e non blocca altri deploy dello stesso ambiente.

Ogni decisione è registrata nell'audit (`deployment.approve`, `deployment.reject`), così come l'esito finale (`approval.finish`).

## Finestre di freeze

Durante una finestra di freeze i deploy reali sull'applicazione e sull'ambiente sono bloccati.

| Chiave | Descrizione |
|---|---|
| `name`, `reason` | Nome e motivo, riportati nel messaggio di errore. |
| `applications`, `environments` | Pattern con `*`. |
| `regions` | Facoltativo: limita la finestra ai passi dei [rilasci a ondate](ondate.md#approvazioni-e-freeze) su queste region (pattern con `*`). Una finestra con `regions` non blocca l'accodamento: il rollout si mette in pausa prima della region e riparte a finestra chiusa. |
| `from`, `to` | Finestra singola (RFC 3339). |
| `cron`, `duration`, `timezone` | Finestra ricorrente: ogni occorrenza inizia a `cron` (5 campi) e dura `duration`. |

Il freeze è verificato due volte:

- **all'accodamento:** un deploy immediato, o pianificato con `schedule.at` dentro una finestra, viene rifiutato con `422`;
- **all'avvio sul worker,** nello step `config`: un deploy accodato prima (ad esempio da una schedule o approvato tardi) che parte durante una finestra fallisce subito, senza retry.

```sh
manent freezes [-a myapp] [-e prod]     # finestre attive e prossime occorrenze
```

### Deroga per le emergenze

Un deploy urgente può derogare indicando il motivo, ad esempio il numero dell'incidente:

```yaml title="descrittore"
freezeOverride: "INC-4711: correzione del calcolo IVA"
```

oppure `--freeze-override "INC-4711 …"` con `promote` e `rollback`. La deroga:

- richiede il permesso `deployment.freeze-override`, concesso solo da `admin` o da un ruolo dedicato;
- viene registrata nell'audit (campo `comment`) e nell'output dello step `config`;
- non esonera dalle approvazioni;
- non è ammessa con `schedule.cron`, perché vale per un singolo deploy.

## Rollback manuale

Il [rollback automatico](rollback.md) interviene quando un deploy fallisce. Quando invece un deploy riuscito si rivela difettoso, si torna a una versione precedente:

```sh
manent rollback -a myapp -e prod [--to 1.3.2] [--freeze-override "INC-…"]
```

Il rollback manuale:

1. sceglie nello storico dell'ambiente l'ultimo deploy riuscito di una versione diversa da quella in esercizio (o quello di `--to`). Se dopo l'ultimo deploy riuscito un deploy è fallito lasciando i target modificati (senza rollback automatico, con un rollback fallito o con un rollout interrotto dopo il primo stadio), la versione in esercizio non è più garantita: il rollback ripristina quella dell'ultimo deploy riuscito, che si può anche indicare con `--to`;
2. lo rilascia di nuovo con il suo descrittore e il suo `sha256`, come nuovo deploy con `kind: rollback`;
3. è ammesso solo per i tipi di artefatto per cui `rollback.artifact_types.<tipo>.allowed` è `true`. Le **migration** sono escluse di default: rilasciare la versione precedente di una migration non annulla le modifiche allo schema.

Richiede il permesso `deployment.rollback` (ruoli `deployer` e `operator`) ed è soggetto ad approvazioni e freeze come ogni deploy. Il plugin riceve un normale deploy della versione precedente: l'artefatto viene scaricato, verificato e copiato di nuovo sui target, quindi deve essere ancora disponibile alla sorgente.

## API

| Metodo | Percorso | Descrizione |
|---|---|---|
| `GET` | `/api/v1/inventory?application=&environment=` | Inventario. |
| `POST` | `/api/v1/promotions` | Promozione. |
| `POST` | `/api/v1/rollbacks` | Rollback manuale. |
| `POST` | `/api/v1/deployments/{id}/approve` | Approva. |
| `POST` | `/api/v1/deployments/{id}/reject` | Respinge. |
| `GET` | `/api/v1/freezes?application=&environment=` | Finestre di freeze. |

Vedi [API HTTP](api.md#governance).
