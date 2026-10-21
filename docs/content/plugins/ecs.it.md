# ecs

Il plugin `ecs` rilascia un'immagine di container su un servizio Amazon ECS, su Fargate o EC2. Registra una nuova revisione della task definition in uso con la sola immagine cambiata, aggiorna il servizio e attende che il deployment di ECS sia completato. Parla con l'API di ECS tramite l'SDK ufficiale di AWS: sul worker non serve la CLI `aws`.

Servizio, cluster, rete, ruoli IAM e il resto della task definition appartengono a chi gestisce l'infrastruttura (Terraform, CloudFormation, CDK): manent cambia solo l'immagine. Un servizio che non esiste ancora è un errore, non qualcosa che il plugin crea.

## L'artefatto è un'immagine

L'artefatto è un'immagine in un registry, indicata con una sorgente `oci://`, come per il plugin [container](container.md): il digest del manifest diventa lo sha256 dell'artefatto e il servizio esegue sempre `repository@sha256:<digest>`, mai il tag.

```yaml
artifact:
  type: ecs
  name: shop
  version: "1.4.0"
  source: oci://123456789012.dkr.ecr.eu-west-1.amazonaws.com/shop:1.4.0
```

Per leggere il manifest da ECR, lo step `artifacts` ha bisogno di credenziali `basic` con utente `AWS` e la password stampata da `aws ecr get-login-password`, che dura 12 ore. Conservala in un file rinnovato periodicamente (ad esempio da un timer systemd o da un sidecar) e leggila con il provider `file:`:

```yaml title="manent.yml"
artifacts:
  sources:
    - prefix: oci://123456789012.dkr.ecr.eu-west-1.amazonaws.com/
      auth: {type: basic, username: AWS, secret_ref: "file:/run/manent/ecr-password"}
```

## Configurazione

```yaml title="manent.yml"
plugins:
  ecs:
    artifact_types: [ecs]                # default
    # endpoint: http://localhost:4566    # endpoint alternativo di ECS, es. LocalStack
```

## Target e credenziali

Ogni target è una **region**, eventualmente con il cluster: `eu-west-1` oppure `eu-west-1/prod`. Più region formano un rilascio multi-region, e l'etichetta `region` permette ai [rilasci a ondate](../guida/ondate.md) di rilasciare una region alla volta.

Senza parametri per le credenziali il worker usa la catena standard dell'SDK AWS: variabili d'ambiente, IRSA su EKS (`AWS_ROLE_ARN`, `AWS_WEB_IDENTITY_TOKEN_FILE`), il ruolo del task ECS, l'instance profile EC2. È la scelta consigliata: nessuna chiave da conservare.

| Parametro | Descrizione |
|---|---|
| `access_key_id_secret`, `secret_access_key_secret` | Al posto della catena: segreti con chiavi statiche. |
| `session_token_secret` | Segreto con il session token, per chiavi temporanee. |
| `role_arn` | Ruolo da assumere con STS, ad esempio un ruolo di deploy nell'account dell'ambiente. La sessione si chiama `manent-<id del deploy>`, così CloudTrail collega ogni chiamata al deploy. |
| `external_id` | External ID richiesto dalla trust policy del ruolo. |

Definisci `role_arn` e le credenziali nella [configurazione di ambiente](../guida/configurazione-ambiente.md) e lasciali fuori dai parametri del plugin in `descriptor_policy`: un descrittore non deve poter scegliere l'account su cui rilasciare.

Il ruolo deve avere `ecs:DescribeServices`, `ecs:DescribeTaskDefinition`, `ecs:ListTaskDefinitions`, `ecs:RegisterTaskDefinition`, `ecs:TagResource`, `ecs:UpdateService` e `iam:PassRole` sul task role e sull'execution role della task definition.

```yaml title="envconfig/shop/shop/prod.yml"
plugin: ecs
targets:
  - {host: eu-west-1/prod, labels: {region: eu-west-1}}
  - {host: us-east-1/prod, labels: {region: us-east-1}}
params:
  service: shop-web
  role_arn: arn:aws:iam::123456789012:role/manent-deploy
rollback:
  enabled: true
```

## Parametri del deploy

| Parametro | Default | Descrizione |
|---|---|---|
| `cluster` | `default` | Cluster, per i target che indicano solo la region. |
| `service` | nome dell'applicazione | Servizio ECS. |
| `container` | l'unico container, o quello con il nome dell'applicazione | Container a cui cambiare l'immagine. I sidecar mantengono la loro. |
| `wait_timeout` | `15m` | Attesa del deployment di ECS. |
| `search_revision` | `100` | Revisioni esaminate per trovare quella di una versione in un rollback senza dettagli. |

`artifact.version` è obbligatorio: diventa un tag della task definition.

## Come funziona

Per ogni region, in sequenza:

1. legge il servizio e la sua task definition. Un servizio con deployment controller `CODE_DEPLOY` o `EXTERNAL` viene rifiutato;
2. se la task definition in uso è già stata registrata da questo deploy con questa immagine, passa all'attesa: retry e recovery non creano nuove revisioni;
3. registra una nuova revisione della stessa famiglia, copiando ogni campo di quella attuale (container, variabili, segreti, CPU e memoria, ruoli, volumi, modalità di rete) e cambiando solo l'immagine del container scelto. Ai tag esistenti aggiunge `manent.application`, `manent.environment`, `manent.version` e `manent.deployment`;
4. aggiorna il servizio con la nuova revisione;
5. attende che il deployment di ECS sia `COMPLETED`. Un deployment `FAILED`, ad esempio fermato dal deployment circuit breaker, fa fallire subito il deploy con il motivo riportato da ECS.

Il deploy si ferma alla prima region che fallisce. Il modo in cui i task vengono sostituiti (`minimumHealthyPercent`, `maximumPercent`, health check del load balancer, circuit breaker) dipende dalla configurazione del servizio: ECS sostituisce i task senza interruzioni se è impostata di conseguenza.

## Strategie native di ECS { #strategie-native }

Il controller ECS ha strategie che spostano il traffico da sole, sull'ALB, sull'NLB o su Service Connect: `BLUE_GREEN`, `LINEAR` e `CANARY` (`deploymentConfiguration.strategy` del servizio, impostata con l'infrastruttura). Il plugin non le sostituisce: le **delega**. Aggiorna il servizio come sempre, riporta la strategia nei dettagli del target (`strategy`) e allunga l'attesa del deployment della durata prevista (bake finale, più i gradini della lineare o l'attesa della canary). Gradini, bake, hook del ciclo di vita e ritorno sui CloudWatch alarm li gestisce ECS.

Con la delega i gate, le approvazioni tra gradini e i freeze di manent non entrano tra un gradino e l'altro. Per averli si usa un [rilascio per percentuale](../guida/traffico.md) con due servizi ECS, uno per slot (target con l'etichetta `slot`), e il router `aws-alb` sui loro target group.

## Rollback

- **Release in esercizio** (locator `plugin`): il tag `manent.version` della task definition in uso. Un servizio la cui task definition non ha il tag (mai rilasciato da manent) non ha release; versioni diverse tra le region fermano il deploy al checkpoint.
- **Rollback:** su ogni region aggiorna il servizio con la task definition della release precedente, salvata al checkpoint, e attende il deployment. Senza quel dettaglio (un rollback manuale) cerca la revisione più recente della famiglia con il tag della versione. Prosegue anche se una region fallisce. Non registra nulla: la revisione precedente viene riusata così com'è.

Se il circuit breaker ha `rollback: true`, ECS torna indietro da solo quando il deployment fallisce; il rollback di manent resta utile quando fallisce uno step successivo (`post-test`, `activation`).

Il tipo di artefatto `ecs` ammette il rollback (`rollback.artifact_types.ecs.allowed: true` nei default).

## Dry-run

In [dry-run](../guida/dry-run.md), per ogni region: credenziali, servizio, task definition e container. Non registra nulla e non modifica il servizio.

## Risultato

```json
{
  "plugin": "ecs",
  "targets": [
    {
      "host": "eu-west-1/prod",
      "status": "deployed",
      "details": {
        "region": "eu-west-1", "cluster": "prod", "service": "shop-web", "container": "shop",
        "previous": "1.3.0",
        "task_definition": "arn:aws:ecs:eu-west-1:123456789012:task-definition/shop:42"
      }
    }
  ]
}
```
