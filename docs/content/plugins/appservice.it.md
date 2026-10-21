# appservice

Il plugin `appservice` rilascia su web app di **Azure App Service**. L'artefatto può essere:

- un **pacchetto** (`.zip`, `.war`, `.jar` o `.ear`), inviato a Kudu con l'API OneDeploy;
- un'**immagine di container**, che diventa `linuxFxVersion` (o `windowsFxVersion`) `DOCKER|<repository>@sha256:<digest>`.

Parla direttamente con l'API REST di Azure Resource Manager e con Kudu: sul worker non servono né la Azure CLI né l'SDK.

La web app, il suo piano App Service, gli slot, le impostazioni e l'identità appartengono a chi gestisce l'infrastruttura (Terraform, Bicep). manent cambia solo il contenuto e i metadati della release. Una web app o uno slot che non esistono ancora sono un errore.

## Con uno slot o direttamente in produzione

| | Con `slot` (consigliato) | Senza `slot` |
|---|---|---|
| Dove va la release | Nello slot (ad esempio `staging`). Dopo il suo controllo di salute, uno **swap** con la produzione. | Direttamente in produzione. |
| Release in esercizio | App setting `MANENT_VERSION` e `MANENT_DEPLOYMENT` dello slot: passano in produzione con lo swap. | Tag `manent-version`, `manent-deployment` e, per un'immagine, `manent-image` sulla web app. Scrivere i tag non riavvia l'app. |
| Rollback | Uno swap all'indietro: la versione precedente è ancora nello slot. | Solo per un'immagine: torna l'immagine precedente. Un pacchetto senza slot non ha rollback. |
| Rilascio per percentuale | Sì: le `rampUpRules` mandano allo slot una quota del traffico di produzione, poi lo swap. | No. |

Lo swap è il modello di rilascio raccomandato da Azure. Lo slot si scalda prima di ricevere il traffico di produzione, e la produzione non esegue mai una versione rilasciata a metà.

Le due impostazioni `MANENT_*` non devono essere *impostazioni dello slot* (sticky): devono passare con lo swap. Il plugin lo verifica dopo ogni swap.

## Configurazione

```yaml title="manent.yml"
plugins:
  appservice:
    artifact_types: [appservice]      # default
    # management_endpoint: https://management.azure.com   # cloud sovrani
    # authority: https://login.microsoftonline.com
```

`scm_endpoint` e `site_endpoint` sostituiscono `https://<host>` di Kudu e delle web app, seguiti da `/<host>`. Servono per i proxy e per l'Azure emulato dei [casi d'uso](../casi-uso.md).

## Target e credenziali

Ogni target è una web app: `host: <nome>`, con il resource group del parametro `resource_group`, oppure `host: <resource group>/<nome>`. Più web app formano un rilascio multi-region (una per region, ciascuna dietro Front Door o Traffic Manager). L'etichetta `region` permette ai [rilasci a ondate](../guida/ondate.md) di rilasciare una region alla volta.

Le credenziali, in quest'ordine:

1. con `client_secret_secret`, un **service principal**: `tenant_id`, `client_id` e il segreto con il client secret;
2. altrimenti l'identità del worker:
    - **workload identity** su AKS (`AZURE_FEDERATED_TOKEN_FILE`, `AZURE_CLIENT_ID`, `AZURE_TENANT_ID`);
    - la **managed identity** di App Service o Container Apps (`IDENTITY_ENDPOINT`);
    - la managed identity della VM (IMDS); `client_id` sceglie un'identità assegnata dall'utente.

L'identità del worker è la scelta consigliata: nessun segreto da ruotare. L'identità ha bisogno del ruolo **Website Contributor** sulla web app (contenuto, impostazioni, swap) e di **Tag Contributor** per i tag. Kudu accetta lo stesso token di Entra ID, quindi le credenziali di pubblicazione basic possono restare disattivate.

```yaml title="envconfig/shop/shop/prod.yml"
plugin: appservice
targets:
  - {host: shop-weu, labels: {region: westeurope}}
  - {host: rg-shop-us/shop-eus, labels: {region: eastus}}
params:
  subscription: 9b2c5e1a-0000-4000-8000-1234567890ab
  resource_group: rg-shop-eu
  slot: staging
  health_path: /healthz
rollback:
  enabled: true
```

## Parametri del deploy

| Parametro | Default | Descrizione |
|---|---|---|
| `subscription` | (obbligatorio) | ID della sottoscrizione. |
| `resource_group` | | Resource group dei target scritti come `<nome>`. |
| `slot` | | Slot su cui rilasciare e da scambiare con la produzione; mai `production`. |
| `health_path` | | Percorso interrogato finché risponde `2xx`: sullo slot prima dello swap, o sulla produzione senza slot. |
| `wait_timeout` | `15m` | Attesa di Kudu, del controllo di salute e dello swap, per ogni web app. |
| `tenant_id`, `client_id` | | Service principal o managed identity assegnata dall'utente. |
| `client_secret_secret` | | Segreto con il client secret del service principal. |

`artifact.version` è obbligatoria. Il tipo di artefatto è `appservice`; l'estensione distingue un pacchetto da un'immagine (sorgente `oci://`, come per il plugin [container](container.md)).

## Come funziona

Per ogni web app, in sequenza:

1. con uno slot, se la produzione riporta già questo deploy (`MANENT_DEPLOYMENT`), si ferma lì: un retry o una recovery non fanno due swap;
2. scrive la release nelle impostazioni dello slot (lo slot si riavvia, la produzione no). Senza slot marca la produzione con il tag `manent-pending`, così il rollback sa che la produzione è stata toccata;
3. rilascia il contenuto:
    - un pacchetto: `POST /api/publish?type=<zip|war|jar|ear>&async=true` di Kudu, poi attende il nuovo record di deployment. Un deployment fallito fa fallire il deploy con lo stato di Kudu e l'URL del log;
    - un'immagine: `linuxFxVersion` o `windowsFxVersion` della configurazione del sito;
4. con `health_path`, attende `2xx` sullo slot (o sulla produzione);
5. con uno slot, lo **swap** (`slotsswap`, operazione asincrona con `preserveVnet`). Poi verifica che la produzione riporti questo deploy. Senza slot, i tag.

## Rollback e stato reale

- **Release in esercizio** (locator `plugin`, [riconciliazione](../guida/governance.md#stato-reale)): con uno slot, l'impostazione `MANENT_VERSION` della produzione. Uno swap fatto a mano si vede quindi come un cambio di versione. Senza slot, il tag `manent-version`; per un'immagine, un'immagine in esecuzione diversa da `manent-image` non è una release di manent.
- **Rollback con uno slot:** se il deploy era arrivato allo swap, uno swap all'indietro, dopo aver verificato che lo slot contenga la versione precedente. Se il deploy si era fermato prima dello swap, la produzione non è mai stata toccata e non cambia nulla.
- **Rollback senza slot:** un'immagine torna a quella precedente, letta al checkpoint. Un pacchetto no, e il rollback fallisce: serve uno slot.

Il tipo di artefatto `appservice` ammette il rollback (`rollback.artifact_types.appservice.allowed: true` nei default).

## Rilasci per percentuale { #rilasci-per-percentuale }

Con un rollout `strategy: traffic` ([Rilasci per percentuale](../guida/traffico.md)) e uno slot:

- la candidata va nello slot, senza traffico;
- ogni gradino scrive una regola nelle `experiments.rampUpRules` della produzione ("Testing in production"), che manda allo slot quella quota delle richieste;
- i tester raggiungono lo slot con il suo host name (`tester_url`), o la produzione con il cookie `x-ms-routing-name=<slot>`;
- la promozione toglie la regola e fa lo swap; il ritorno toglie la regola e, se lo swap c'è già stato, lo ripete all'indietro.

La versione precedente resta nello slot, come punto a cui tornare.

## Dry-run

In [dry-run](../guida/dry-run.md), per ogni web app legge la web app, lo slot, la configurazione del sito e le impostazioni e, per un pacchetto, i deployment di Kudu. Non cambia nulla.
