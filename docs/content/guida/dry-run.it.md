# Dry-run

Il dry-run esegue la pipeline su un worker come un deploy reale, ma **senza modificare gli ambienti**. Risponde alla domanda: se lanciassi questo deploy adesso, andrebbe a buon fine?

```sh
manent deploy -f descriptor.yml --dry-run
```

```sh
curl -X POST 'localhost:8080/api/v1/deployments?dry_run=true' \
  -H 'Content-Type: application/json' -d @descriptor.json
```

In alternativa si può scrivere `dryRun: true` nel descrittore.

## Differenze con `validate`

| | `validate` | Dry-run |
|---|---|---|
| Dove gira | Sul server (API), senza accodare nulla | Su un worker, dalla coda |
| Piano e validazione del descrittore | ✓ | ✓ |
| Policy sui descrittori, callback, sorgente dell'artefatto | ✓ | ✓ |
| Configurazione di ambiente | ✓ (se non è raggiungibile, solo un avviso) | ✓ |
| Risoluzione dei segreti | | ✓ |
| Download e verifica dell'artefatto | | ✓ |
| Controlli QA | | ✓ |
| Verifica dei target tramite il plugin | | ✓ |
| Policy di rollback | ✓ | ✓ |
| Release in esercizio e supporto al rollback del plugin | | ✓ |

## Cosa fa ogni step

In dry-run uno step viene eseguito solo se dichiara di saperlo simulare. Uno step che non lo dichiara viene **saltato**: un nuovo step non produce mai effetti in dry-run per una dimenticanza.

| Step | In dry-run |
|---|---|
| `config` | Eseguito normalmente. |
| `secrets` | Eseguito normalmente: verifica i segreti. |
| `artifacts` | Eseguito normalmente: l'artefatto viene scaricato solo nella directory di lavoro del worker, poi rimossa. |
| `qa` | Eseguito normalmente. |
| `checkpoint:deploy` | Solo con il [rollback](rollback.md) attivo. Eseguito normalmente: verifica che il plugin supporti il rollback e legge la release in esercizio. |
| `deploy` | Sceglie e valida il plugin come in un deploy reale, poi gli chiede di **verificare** i target senza modificarli. |
| `post-test` | Saltato. |
| `activation` | Saltato. |

Nel risultato, gli step saltati hanno output `{"dry_run": "skipped"}`. Lo step `deploy` riporta `"dry_run": "checked"` con l'esito per target, oppure `"dry_run": "validated"` se il plugin non sa verificare i target e ci si è fermati alla validazione della richiesta.

Ad esempio il plugin [remote-copy](../plugins/remote-copy.md) si connette a ogni target, verifica che la directory di destinazione sia scrivibile (o creabile) e che `sha256sum` sia disponibile, ma non copia nulla e non esegue `post_command`.

## Regole

- **ID:** con un `id` nel descrittore, il dry-run usa come ID del workflow `<id>.dry-run`. Così non consuma la chiave di idempotenza e il deploy reale con lo stesso `id` resta eseguibile.
- **Coda:** il dry-run usa la stessa coda dei deploy e rispetta `environment_concurrency`. Mentre è in corso, un deploy reale dello stesso ambiente attende.
- **Schedule:** il dry-run non è ammesso insieme a `schedule`.
- **Callback:** i [callback](callback.md) ricevono gli eventi anche durante un dry-run, con `"dry_run": true`.
- **Rollback:** un dry-run fallito non esegue mai compensazioni, anche se il rollback è attivo.

## Supportare il dry-run in un plugin

Vedi [Scrivere un plugin](../plugins/index.md#dry-run).
