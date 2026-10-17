# containerapps

Il plugin `containerapps` rilascia un'immagine di container su un'app di **Azure Container Apps**. Cambia l'immagine del template, che crea una nuova revisione con un suffisso derivato dal deploy, e attende che la revisione sia in esecuzione e sana. Parla direttamente con l'API REST di Azure Resource Manager: sul worker non servono né la Azure CLI né l'SDK.

L'app, il suo ambiente, l'ingress, le regole di scala, i segreti e l'identità appartengono a chi gestisce l'infrastruttura (Terraform, Bicep): manent cambia solo l'immagine, due variabili d'ambiente e il traffico. Un'app che non esiste ancora è un errore.

## L'artefatto è un'immagine

Come per il plugin [container](container.md), l'artefatto è un'immagine in un registry, indicata come sorgente `oci://`: il digest diventa lo sha256 dell'artefatto e la revisione esegue sempre `repository@sha256:<digest>`.

```yaml
artifact:
  type: containerapps
  name: shop-api
  version: "2.4.0"
  source: oci://acme.azurecr.io/shop/api:2.4.0
```

La versione viene scritta nelle variabili d'ambiente **`MANENT_VERSION`** e **`MANENT_DEPLOYMENT`** del container: ogni revisione porta con sé la sua release.

## Configurazione

```yaml title="manent.yml"
plugins:
  containerapps:
    artifact_types: [containerapps]   # default
    # management_endpoint: https://management.azure.com
```

## Target e credenziali

Ogni target è un'app: `host: <nome>` con il parametro `resource_group`, oppure `host: <resource group>/<nome>`. Le credenziali sono quelle del plugin [appservice](appservice.md#target-e-credenziali): service principal con `client_secret_secret`, oppure l'identità del worker (workload identity, managed identity). L'identità ha bisogno del ruolo **Container Apps Contributor** sull'app.

```yaml title="envconfig/shop-api/prod.yml"
plugin: containerapps
targets:
  - {host: shop-api, labels: {region: westeurope}}
params:
  subscription: 9b2c5e1a-0000-4000-8000-1234567890ab
  resource_group: rg-shop
  container: api
rollback:
  enabled: true
```

## Parametri del deploy

| Parametro | Default | Descrizione |
|---|---|---|
| `subscription` | (obbligatorio) | ID della sottoscrizione. |
| `resource_group` | | Resource group dei target scritti come `<nome>`. |
| `container` | l'unico container, o quello con il nome dell'applicazione | Container di cui cambia l'immagine. Sidecar e init container mantengono la loro. |
| `wait_timeout` | `10m` | Attesa della revisione e del traffico, per ogni app. |
| `tenant_id`, `client_id`, `client_secret_secret` | | Come per [appservice](appservice.md). |

## Come funziona

Per ogni app, in sequenza:

1. se la revisione `<app>--m<hash del deploy>` esiste già, passa all'attesa: retry e recovery non creano revisioni;
2. cambia immagine e variabili `MANENT_*` del container e il `revisionSuffix` del template, e in modalità **Multiple** mette tutto il traffico sull'ultima revisione. L'aggiornamento è una `PATCH`: i segreti dell'app non vengono mai letti né riscritti;
3. attende che la revisione sia provisioned, in esecuzione e sana. Una revisione che fallisce o non è sana fa fallire il deploy con il motivo di Container Apps. Il traffico resta sulla revisione precedente: in modalità Multiple lo riceve l'ultima revisione *pronta*, in Single resta attiva la precedente.

## Rollback e stato reale

- **Release in esercizio:** la variabile `MANENT_VERSION` della revisione che riceve il traffico (il peso maggiore; in modalità Single, o senza ingress, l'ultima revisione pronta).
- **Rollback, modalità Multiple:** il traffico torna alla revisione precedente, riattivata se serve. È immediato: la revisione esiste già.
- **Rollback, modalità Single:** una nuova revisione con l'immagine e la versione precedenti (suffisso `r<hash>`), perché la modalità Single tiene attiva una sola revisione.

Un'app che il deploy non ha cambiato non viene toccata. Il tipo di artefatto `containerapps` ammette il rollback nei default.

## Rilasci per percentuale { #rilasci-per-percentuale }

Con `strategy: traffic` ([Rilasci per percentuale](../guida/traffico.md)) l'app deve essere in modalità **Multiple** con un ingress:

- la revisione candidata viene creata con tutto il traffico tenuto sulla stabile;
- i gradini dividono il traffico dell'ingress tra le due revisioni;
- con `testers.tag` la candidata riceve una label, con l'URL `https://<app>---<label>.<dominio dell'ambiente>`;
- la promozione manda tutto il traffico all'ultima revisione, il ritorno alla stabile;
- la revisione sostituita viene disattivata dopo `scaleDownDelay`, salvo che riceva di nuovo traffico.

## Dry-run

In [dry-run](../guida/dry-run.md), per ogni app: credenziali, app, container e revisione in esercizio, senza modifiche.
