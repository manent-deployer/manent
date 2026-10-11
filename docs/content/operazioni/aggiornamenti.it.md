# Versioni e aggiornamenti

## Versioni di manent

manent adotta il [Semantic Versioning](https://semver.org/lang/it/). Le modifiche di ogni versione sono in [`CHANGELOG.md`](https://github.com/joeg-ita/manent/blob/main/CHANGELOG.md).

Una versione **major** (o, prima della 1.0, una **minor**) può introdurre modifiche incompatibili a:

- API HTTP e formato JSON delle risposte;
- descrittore di deploy (`apiVersion`) e configurazione `manent.yml`;
- protocollo dei [plugin esterni](../plugins/esterni.md) (`ProtocolVersion`) e package `pkg/plugin`;
- **versione dei workflow** (vedi sotto).

Le versioni **patch** correggono errori senza cambiare nulla di questo elenco.

```sh
manent version
```

## Due versioni diverse

| | Dove | Cosa indica |
|---|---|---|
| Versione del binario | `manent version`, metrica `manent_build_info` | Il rilascio di manent installato. |
| Versione dei workflow | `app.version` in `manent.yml` | La compatibilità dei workflow DBOS: un worker riprende **solo** i workflow della propria versione. |

DBOS registra con ogni workflow la versione dell'applicazione che l'ha avviato e, dopo un crash, lo riprende ripetendo gli step già registrati. Se il codice del workflow cambia in modo incompatibile (step in ordine diverso, input diverso), la ripresa con il nuovo codice darebbe risultati sbagliati. Per questo:

- **`app.version` non va cambiata a ogni rilascio** di manent, ma solo quando le note di rilascio lo indicano ("versione dei workflow cambiata");
- un worker con una nuova `app.version` non preleva né riprende i workflow della versione precedente: vanno completati prima dell'aggiornamento o migrati.

## Aggiornare manent

### Rilascio senza cambio della versione dei workflow

È il caso normale. Si aggiornano API e worker uno alla volta (rolling update, il comportamento predefinito del chart Helm):

1. ogni worker riceve `SIGTERM`, smette di prelevare deploy e attende quelli in corso fino a `dbos.drain_timeout` ([drain](affidabilita.md#arresto-ordinato-drain));
2. riparte con il nuovo binario e lo stesso `executor_id`, e riprende i deploy eventualmente rimasti in sospeso.

### Rilascio con una nuova versione dei workflow

1. **Svuotare.** Sospendere le schedule che scattano durante l'aggiornamento (`manent schedule pause`), poi attendere che i deploy in corso finiscano (`manent list --state PENDING`).
2. **Aggiornare** i worker e impostare la nuova `app.version`. I deploy accodati dopo l'aggiornamento usano la nuova versione.
3. **Migrare** ciò che è rimasto della versione precedente, ad esempio deploy ancora in coda o approvazioni in attesa:

    ```sh
    manent-server admin executors                                   # pending per versione
    manent-server admin recover --from-version 0.1.0 --dry-run      # cosa verrebbe migrato
    manent-server admin recover --from-version 0.1.0                # verso app.version
    ```

    Ogni workflow viene annullato e riportato alla nuova versione con lo **stesso ID**. Gli step conclusi restano registrati e il workflow riparte dal successivo. Con `--from-start` riparte invece da zero, rieseguendo tutti gli step, ad esempio se le note di rilascio lo richiedono. Il comando rifiuta la migrazione se un worker della versione precedente risulta ancora attivo.

4. Riattivare le schedule.

!!! note "Limiti della migrazione"
    - I deploy pianificati con `schedule.at` (`DELAYED`) non vengono migrati: vanno annullati e ripianificati.
    - Le decisioni inviate a un'approvazione dopo la perdita del suo worker e non ancora elaborate vanno ripetute dopo la migrazione.

## Rollback di un aggiornamento

Se la versione dei workflow non è cambiata, si torna al binario precedente con un altro rolling update. Altrimenti si ripristina anche `app.version` e si migrano all'indietro, con `manent-server admin recover --from-version`, i workflow avviati con la nuova versione.
