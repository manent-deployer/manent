# Pipeline e step

## Gli step standard

| Step | Cosa fa | Output principali |
|---|---|---|
| `config` | Legge la [configurazione di ambiente](../guida/configurazione-ambiente.md) e la fonde con il descrittore. Non fa parte del piano: è sempre eseguito per primo. | `source`, `revision`, `targets` |
| `secrets` | Verifica che tutti i segreti dichiarati siano risolvibili, senza restituirne i valori. Controlla anche che i segreti usati dai callback siano dichiarati. | `count` |
| `artifacts` | Scarica l'artefatto nella directory di lavoro del deploy e ne calcola lo sha256. | `local_path`, `sha256`, `size` |
| `qa` | Controlli di qualità: artefatto non vuoto e, se `artifact.sha256` è indicato, corrispondenza dell'hash. | esito di ogni controllo |
| `deploy` | Sceglie il plugin, valida la richiesta ed esegue il deploy sui target. | `plugin`, `result` |
| `post-test` | Chiama gli endpoint di `postTest.http` e verifica lo status HTTP atteso (default 200). | esito di ogni URL |
| `activation` | Rende operativo il rilascio, quando serve: per molti artefatti basta il deploy, per questo è opzionale. L'implementazione iniziale registra soltanto l'attivazione. | `status` |

Con il [rollback](../guida/rollback.md) attivo il worker aggiunge altri step DBOS, che non fanno parte del piano:

| Step | Quando | Output principali |
|---|---|---|
| `checkpoint:deploy` | Prima di `deploy`: verifica il supporto al rollback e registra la release in esercizio. | `plugin`, `locator`, `previous_version` |
| `rollback:<step>` | Dopo il fallimento di uno step in `rollback.on`, per ogni step già avviato che sa annullarsi (`deploy`, `activation`), in ordine inverso. | `rollback` (`completed` o `skipped`), `version`, `result` |

## Il piano

Il piano è l'elenco ordinato degli step da eseguire. Viene calcolato **prima** di accodare il deploy, combinando la configurazione (`pipeline.steps` in `manent.yml`) con la sezione `steps` del descrittore, e viene passato al workflow come input.

```yaml title="configs/manent.yml"
pipeline:
  steps:
    - name: secrets
      mode: mandatory
    - name: artifacts
      mode: mandatory
      max_retries: 3
    - name: qa
      mode: optional
      enabled: true
    - name: deploy
      mode: mandatory
      max_retries: 2
    - name: post-test
      mode: optional
      enabled: false
      max_retries: 5
    - name: activation
      mode: optional
      enabled: true
```

Regole:

- gli step `mandatory` sono sempre eseguiti: il descrittore non può disattivarli;
- gli step `optional` partono dal valore di `enabled` e il descrittore può cambiarlo;
- l'ordine segue sempre la configurazione, mai il descrittore;
- un nome sconosciuto nella sezione `steps` del descrittore è un errore.

```yaml title="Descrittore"
steps:
  qa: false
  post-test: true
```

Il piano si può vedere senza eseguire nulla, con `manent validate` o con `POST /api/v1/deployments/plan`.

## Retry

`max_retries` indica i tentativi aggiuntivi di uno step in caso di errore. I retry sono gestiti da DBOS con un intervallo crescente. Esaurito l'ultimo tentativo, il deploy si ferma con stato `ERROR` e gli step successivi non vengono eseguiti. Se la policy lo prevede, prima di terminare il worker esegue il [rollback](../guida/rollback.md).

## Step compensabili

Uno step che modifica gli ambienti può dichiarare come annullare i propri effetti, implementando:

```go
// Compensator annulla gli effetti dello step durante il rollback. Deve essere idempotente.
type Compensator interface {
	Compensate(ctx context.Context, in Input, checkpoint Output) (Output, error)
}

// Checkpointer registra, prima dello step, lo stato a cui tornare. Non deve avere effetti.
type Checkpointer interface {
	Checkpoint(ctx context.Context, in Input) (Output, error)
}
```

Il checkpoint è uno step DBOS distinto da quello che protegge: i retry dello step non lo ripetono, quindi registra sempre lo stato precedente a ogni modifica. Uno step che non implementa `Compensator` non viene annullato.

## Dati tra step

Gli step si scambiano dati solo tramite il loro output, una mappa `stringa → stringa` che DBOS salva nel database. Lo step `deploy`, ad esempio, legge `local_path` e `sha256` dall'output di `artifacts`.

!!! danger "Gli output non contengono mai segreti"
    Gli output sono persistiti e visibili nello stato del deploy. Lo step `secrets` verifica soltanto che i segreti siano risolvibili; i plugin ne leggono i valori solo al momento dell'uso.

## Directory di lavoro

Ogni deploy usa la directory `workdir/<deployment-id>` del worker che lo esegue. Non serve che `workdir` sia condivisa tra i worker: se un deploy viene ripreso su un worker che non ha l'artefatto, lo step `deploy` lo riscarica e ne verifica lo sha256 registrato da `artifacts`. Al termine del deploy, con successo o errore, la directory viene rimossa.

## Scelta del plugin

Lo step `deploy` sceglie il plugin tramite una **policy**, indicata da `deploy.policy`. La policy di default:

1. usa `deploy.plugin`, se indicato, e verifica che supporti il tipo di artefatto;
2. altrimenti usa il primo plugin attivo, in ordine alfabetico, che supporta `artifact.type`.
