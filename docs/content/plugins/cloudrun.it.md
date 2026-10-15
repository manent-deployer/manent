# cloudrun

Il plugin `cloudrun` rilascia un'immagine di container su un servizio Google Cloud Run. Cambia l'immagine nel template del servizio, che crea una nuova revisione, e manda tutto il traffico all'ultima revisione pronta. La release in esercizio è la revisione che riceve il traffico: il rollback riporta il traffico sulla revisione precedente, senza ricrearla. Parla direttamente con l'Admin API v2 di Cloud Run: sul worker non serve `gcloud`.

Il servizio (rete, service account, scalabilità, variabili d'ambiente, segreti, IAM) appartiene a chi gestisce l'infrastruttura (Terraform): manent cambia solo l'immagine. Un servizio che non esiste ancora è un errore.

## L'artefatto è un'immagine

L'artefatto è un'immagine in un registry, indicata con una sorgente `oci://`, come per il plugin [container](container.md): il digest diventa lo sha256 dell'artefatto e la revisione esegue sempre `repository@sha256:<digest>`.

```yaml
artifact:
  type: cloudrun
  name: shop
  version: "1.4.0"
  source: oci://europe-docker.pkg.dev/acme/shop/shop:1.4.0
```

Per leggere il manifest da Artifact Registry, lo step `artifacts` può usare credenziali `basic` con utente `_json_key` e come password la chiave JSON di un service account con il ruolo Artifact Registry Reader:

```yaml title="manent.yml"
artifacts:
  sources:
    - prefix: oci://europe-docker.pkg.dev/acme/
      auth: {type: basic, username: _json_key, secret_ref: "vault:secret/data/manent/gcp#registry_key"}
```

## Configurazione

```yaml title="manent.yml"
plugins:
  cloudrun:
    artifact_types: [cloudrun]                 # default
    # endpoint: https://run.googleapis.com     # default
```

## Target e credenziali

Ogni target è una **region** (`europe-west1`), con un servizio con lo stesso nome nel progetto. Più region formano un rilascio multi-region; l'etichetta `region` permette ai [rilasci a ondate](../guida/ondate.md) di rilasciare una region alla volta.

Senza `credentials_secret` il worker usa le [Application Default Credentials](https://cloud.google.com/docs/authentication/application-default-credentials): `GOOGLE_APPLICATION_CREDENTIALS`, workload identity su GKE, il metadata server su Compute Engine e Cloud Run. È la scelta consigliata. Con `credentials_secret` il segreto contiene la chiave JSON di un service account; gli altri tipi di credenziali sono rifiutati, perché farebbero chiamare a manent URL scelti da chi scrive il segreto.

Definisci `project` e `credentials_secret` nella [configurazione di ambiente](../guida/configurazione-ambiente.md) e lasciali fuori dai parametri del plugin in `descriptor_policy`.

Il service account deve avere il ruolo Cloud Run Developer (`roles/run.developer`) sul servizio e Service Account User (`roles/iam.serviceAccountUser`) sul service account di esecuzione del servizio.

```yaml title="envconfig/shop/prod.yml"
plugin: cloudrun
targets:
  - {host: europe-west1, labels: {region: europe-west1}}
  - {host: us-central1, labels: {region: us-central1}}
params:
  project: acme-prod
  service: shop
rollback:
  enabled: true
```

## Parametri del deploy

| Parametro | Default | Descrizione |
|---|---|---|
| `project` | (obbligatorio) | ID del progetto Google Cloud. |
| `service` | nome dell'applicazione | Servizio Cloud Run: minuscole, cifre e `-`. |
| `container` | l'unico container, o quello con il nome dell'applicazione | Container a cui cambiare l'immagine. I sidecar mantengono la loro. |
| `credentials_secret` | Application Default Credentials | Segreto con la chiave JSON di un service account. |
| `wait_timeout` | `10m` | Attesa della nuova revisione e dello spostamento del traffico. |

`artifact.version` è obbligatorio: diventa l'annotazione `manent.io/version` della revisione.

## Come funziona

Per ogni region, in sequenza:

1. legge il servizio;
2. se il template ha già questa immagine, le annotazioni di questo deploy e tutto il traffico sull'ultima revisione, passa al controllo finale: retry e recovery non creano nuove revisioni;
3. cambia l'immagine del container scelto, aggiunge al template le annotazioni `manent.io/version` e `manent.io/deployment`, toglie un eventuale nome fisso della revisione (lo genera Cloud Run) e manda tutto il traffico all'ultima revisione. Il servizio viene riscritto con il suo `etag`: una modifica concorrente viene rifiutata invece che sovrascritta;
4. attende la fine dell'operazione. Una revisione che non si avvia (health check fallito, immagine mancante) fa fallire l'operazione e il deploy, con il messaggio di Cloud Run: il traffico resta sulla revisione pronta precedente, quindi il servizio continua a rispondere;
5. verifica che l'ultima revisione creata sia l'ultima pronta.

Il passaggio è immediato quando la revisione è pronta. Per un canary graduale Cloud Run supporta la divisione del traffico tra revisioni, che questo plugin non gestisce ancora.

## Rollback

- **Release in esercizio** (locator `plugin`): l'annotazione `manent.io/version` della revisione che riceve tutto il traffico. Una revisione senza l'annotazione non ha release; traffico diviso tra più revisioni, o versioni diverse tra le region, fermano il deploy al checkpoint.
- **Rollback:** su ogni region manda tutto il traffico alla revisione della release precedente, salvata al checkpoint. Senza quel dettaglio (un rollback manuale) cerca la revisione più recente con quella versione. È immediato: la revisione esiste già e non viene ricreata. Il traffico resta fissato su quella revisione fino al deploy successivo, che lo riporta sull'ultima revisione.

Il tipo di artefatto `cloudrun` ammette il rollback (`rollback.artifact_types.cloudrun.allowed: true` nei default).

## Rilasci per percentuale { #rilasci-per-percentuale }

Con un rollout `strategy: traffic` ([Rilasci per percentuale](../guida/traffico.md)) le revisioni sono gli slot: il deploy crea una revisione con un nome fisso derivato dal deploy (`<servizio>-m<hash>`) lasciando tutto il traffico sulla revisione stabile; con `testers.tag` la candidata riceve un tag e un URL dedicato per i tester. I gradini dividono il traffico del servizio tra le due revisioni; la promozione torna a mandare tutto il traffico all'ultima revisione. Il ritorno fissa il traffico sulla revisione stabile. Le revisioni senza traffico scalano a zero da sole.

Con il traffico diviso, la release in esercizio (rollback, inventario) è la revisione con la quota maggiore; a parità non è determinabile.

## Dry-run

In [dry-run](../guida/dry-run.md), per ogni region: credenziali, servizio e container, poi l'aggiornamento viene inviato con `validateOnly`. Cloud Run lo verifica con i permessi del service account, ma non cambia nulla.

## Risultato

```json
{
  "plugin": "cloudrun",
  "targets": [
    {
      "host": "europe-west1",
      "status": "deployed",
      "details": {"region": "europe-west1", "service": "shop", "previous": "1.3.0", "revision": "shop-00042-xuv", "url": "https://shop-abc123-ew.a.run.app"}
    }
  ]
}
```
