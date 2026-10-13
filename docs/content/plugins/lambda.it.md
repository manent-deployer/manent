# lambda

Il plugin `lambda` rilascia il codice di una funzione AWS Lambda. Aggiorna il codice (un'immagine di container o un pacchetto `.zip`), pubblica una versione immutabile e vi sposta un alias. La release in esercizio è la versione puntata dall'alias: il rollback riporta indietro l'alias, senza ricaricare il codice. Usa l'SDK ufficiale di AWS.

La funzione (ruolo, memoria, variabili d'ambiente, trigger, permessi) appartiene a chi gestisce l'infrastruttura (Terraform, CloudFormation, SAM): manent cambia solo il codice. Chi invoca la funzione, o i suoi trigger, deve usare l'**alias** (l'ARN qualificato `…:function:shop:live`), non `$LATEST`.

## L'artefatto

Due tipi di artefatto, in base al tipo di pacchetto della funzione:

- **immagine** (`PackageType: Image`): una sorgente `oci://`, come per il plugin [container](container.md). Il digest diventa lo sha256 dell'artefatto e Lambda riceve `repository@sha256:<digest>`. Lambda accetta solo immagini ECR della stessa region della funzione: per un'immagine di un'altra region il plugin usa lo stesso repository nella region del target (`123456789012.dkr.ecr.<region>.amazonaws.com/…`), che esiste con la [replica di ECR](https://docs.aws.amazon.com/AmazonECR/latest/userguide/replication.html). Il digest garantisce che sia la stessa immagine.
- **pacchetto `.zip`** (`PackageType: Zip`): un file `.zip` da qualunque sorgente, al più 50 MiB perché viene caricato direttamente. Dopo il caricamento il plugin verifica che lo sha256 calcolato da Lambda coincida con quello dell'artefatto.

```yaml
artifact:
  type: lambda
  name: shop-api
  version: "1.4.0"
  source: https://nexus.example.com/repository/lambda/shop-api-1.4.0.zip
  sha256: 8c1f…
```

Per le credenziali di ECR nello step `artifacts`, vedi il plugin [ecs](ecs.md#lartefatto-e-unimmagine).

## Configurazione

```yaml title="manent.yml"
plugins:
  lambda:
    artifact_types: [lambda]             # default
    # endpoint: http://localhost:4566    # endpoint alternativo di Lambda, es. LocalStack
```

## Target e credenziali

Ogni target è una **region** (`eu-west-1`), con una funzione con lo stesso nome. Le credenziali funzionano come per il plugin [ecs](ecs.md#target-e-credenziali): la catena standard dell'SDK AWS, oppure `access_key_id_secret`/`secret_access_key_secret`/`session_token_secret`, più `role_arn` ed `external_id` per assumere un ruolo.

Il ruolo deve avere `lambda:GetFunctionConfiguration`, `lambda:UpdateFunctionCode`, `lambda:PublishVersion`, `lambda:ListVersionsByFunction`, `lambda:GetAlias`, `lambda:CreateAlias` e `lambda:UpdateAlias` sulla funzione. Per le immagini, la policy del repository ECR deve permettere a Lambda di leggerle.

```yaml title="envconfig/shop-api/prod.yml"
plugin: lambda
targets:
  - {host: eu-west-1, labels: {region: eu-west-1}}
  - {host: eu-central-1, labels: {region: eu-central-1}}
params:
  function: shop-api-prod
  role_arn: arn:aws:iam::123456789012:role/manent-deploy
rollback:
  enabled: true
```

## Parametri del deploy

| Parametro | Default | Descrizione |
|---|---|---|
| `function` | nome dell'applicazione | Nome della funzione. |
| `alias` | `live` | Alias spostato sulla nuova versione. Una funzione condivisa da più ambienti può usare un alias per ambiente (`alias: prod`). |
| `wait_timeout` | `10m` | Attesa dell'aggiornamento del codice e della nuova versione. |

`artifact.version` è obbligatorio: viene scritto nella descrizione della versione e dell'alias (`manent version=1.4.0 deployment=<id>`).

## Come funziona

Per ogni region, in sequenza:

1. legge funzione e alias e verifica che il tipo di pacchetto corrisponda all'artefatto;
2. se l'alias punta già alla versione pubblicata da questo deploy, non fa nulla: retry e recovery non toccano la funzione;
3. attende la fine di un eventuale aggiornamento in corso, poi aggiorna il codice (`UpdateFunctionCode`) e attende `LastUpdateStatus: Successful`. Un aggiornamento fallito (ad esempio un'immagine che non esiste nella region) fa fallire il deploy, e l'alias resta dov'era;
4. pubblica una versione con il `CodeSha256` del codice: se nel frattempo qualcuno ha cambiato il codice, Lambda rifiuta la pubblicazione. Pubblicare codice invariato restituisce la versione esistente;
5. attende che la versione sia `Active`, poi crea l'alias o lo sposta sulla nuova versione. Gli eventuali pesi di traffico sull'alias vengono azzerati.

Lo spostamento dell'alias è atomico: le invocazioni vanno alla versione vecchia finché l'alias non si sposta, poi alla nuova.

## Rollback

- **Release in esercizio** (locator `plugin`): la versione nella descrizione dell'alias. Un alias assente o non scritto da manent non ha release; versioni diverse tra le region fermano il deploy al checkpoint.
- **Rollback:** su ogni region riporta l'alias alla versione della funzione della release precedente, salvata al checkpoint. Senza quel dettaglio (un rollback manuale) cerca la versione più recente pubblicata con quella release. È immediato, perché le versioni sono immutabili e restano disponibili. `$LATEST` mantiene il codice nuovo fino al deploy successivo.

Il tipo di artefatto `lambda` ammette il rollback (`rollback.artifact_types.lambda.allowed: true` nei default).

## Dry-run

In [dry-run](../guida/dry-run.md), per ogni region: credenziali, funzione, tipo di pacchetto, alias, e `UpdateFunctionCode` con `DryRun`, che verifica la richiesta e i permessi senza cambiare il codice.

## Risultato

```json
{
  "plugin": "lambda",
  "targets": [
    {
      "host": "eu-west-1",
      "status": "deployed",
      "details": {"region": "eu-west-1", "function": "shop-api-prod", "alias": "live", "previous": "1.3.0", "function_version": "17"}
    }
  ]
}
```
