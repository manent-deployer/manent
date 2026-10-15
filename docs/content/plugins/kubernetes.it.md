# kubernetes

Il plugin `kubernetes` rilascia manifest o chart Helm su uno o più cluster Kubernetes. Usa il server-side apply, attende che i workload completino il rollout e conserva nel cluster lo storico delle release, da cui torna alla precedente. Parla direttamente con l'API del cluster: sul worker non serve `kubectl`, e `helm` serve solo per renderizzare i chart.

## L'artefatto sono i manifest

L'artefatto è un file di manifest (YAML o JSON, anche con più documenti separati da `---`) oppure un archivio `.tar.gz` di file `.yaml`, `.yml` e `.json`, letti in ordine di nome. È l'artefatto verificato con lo sha256 e promosso tra gli ambienti: lo stesso file va in test e in produzione.

```yaml
artifact:
  type: kubernetes
  name: shop
  version: "1.4.0"
  source: https://nexus.example.com/repository/manifests/shop-1.4.0.yaml
  sha256: 8c1f…
```

Per un chart Helm vedi [Chart Helm](#chart-helm). Per le configurazioni Kustomize si rilascia l'output già generato in CI (`kustomize build`): l'artefatto resta immutabile e verificabile.

### Variabili

Ciò che cambia tra gli ambienti arriva dalla [configurazione di ambiente](../guida/configurazione-ambiente.md), non dai manifest. Nei manifest si possono usare queste variabili:

| Variabile | Valore |
|---|---|
| `${MANENT_APPLICATION}`, `${MANENT_ENVIRONMENT}`, `${MANENT_VERSION}` | Applicazione, ambiente e versione del deploy. |
| `${MANENT_NAMESPACE}` | Il parametro `namespace`. |
| `${MANENT_DEPLOYMENT}` | ID del deploy. |
| `${NOME}` | Il parametro `var.NOME`, ad esempio `var.REPLICAS: "3"` per `${REPLICAS}`. |

Le altre `${...}` restano invariate, perché possono far parte di script contenuti nei manifest.

## Configurazione

```yaml title="manent.yml"
plugins:
  kubernetes:
    # ca_file: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt   # manent dentro il cluster
    insecure_skip_tls_verify: false      # true solo in sviluppo
    artifact_types: [kubernetes]         # default
    helm:                                # solo per i chart
      command: helm                      # default; nell'immagine di manent-server è incluso
      # sha256: "<sha256 del binario>"   # verificato a ogni rendering
      timeout: 2m                        # default
```

## Chart Helm { #chart-helm }

L'artefatto può essere un chart impacchettato con `helm package` (un `.tgz` con `<nome>/Chart.yaml`), riconosciuto in automatico. Il pacchetto è l'artefatto verificato con lo sha256 e promosso tra gli ambienti; le dipendenze vanno incluse (`helm package --dependency-update`).

```yaml
artifact:
  type: kubernetes
  name: shop
  version: "1.4.0"
  source: https://nexus.example.com/repository/charts/shop-1.4.0.tgz
  sha256: 8c1f…
```

Il chart viene solo **renderizzato** con `helm template`, per ogni cluster, come fa Argo CD: gli oggetti generati seguono lo stesso percorso dei manifest (etichette, server-side apply, storico, attesa del rollout, prune, dry-run). Helm non installa nulla e non tiene uno stato nel cluster: niente secret `sh.helm.release.v1`, e `helm list` non mostra la release. Il rollback riapplica gli oggetti salvati nello storico e non richiede Helm.

- **Capabilities del cluster.** `--kube-version` e `--api-versions` vengono dalla discovery del cluster di destinazione, quindi `.Capabilities.KubeVersion` e `.Capabilities.APIVersions.Has` rispondono come in un `helm install`.
- **Values**, in ordine di precedenza crescente: `values.yaml` del chart, i file di `helm_values_files` (contenuti nel chart, ad esempio `values-prod.yaml`), `helm_values` (YAML), i singoli `helm_value.<percorso>`. Come gli altri parametri, arrivano dalla [configurazione di ambiente](../guida/configurazione-ambiente.md).
- **Hook.** Gli hook (`helm.sh/hook`) non hanno un equivalente nel server-side apply: di default non vengono applicati e sono elencati nel risultato (`helm_hooks_skipped`) e nel log. Con `helm_hooks: apply` sono applicati come oggetti normali, insieme agli altri; un Job di migrazione va allora reso idempotente o rinominato a ogni versione (ad esempio con `{{ .Chart.AppVersion }}` nel nome). I test (`helm.sh/hook: test`) non vengono mai applicati.
- **CRD** della cartella `crds/` incluse e applicate per prime (`helm_skip_crds: true` per escluderle).
- **`lookup`** non legge il cluster durante il rendering e restituisce sempre un valore vuoto.
- **Isolamento.** Helm gira con un ambiente vuoto e una home temporanea: non eredita le variabili del worker né legge `KUBECONFIG`, repository o credenziali locali. Con `helm.sha256` il binario viene verificato prima di ogni uso.
- Le variabili `${...}` dei manifest non si applicano ai chart: si usano i values.

```yaml title="envconfig/shop/prod.yml"
plugin: kubernetes
targets:
  - {host: k8s-eu1.example.com, labels: {region: eu-1}}
params:
  namespace: shop-prod
  token_secret: kube-token
  helm_values_files: values-prod.yaml
  helm_value.replicaCount: "4"
  helm_values: |
    ingress:
      enabled: true
      host: shop.example.com
```

Un'applicazione già installata con `helm install` può passare a manent: gli oggetti vengono adottati dal server-side apply. Poi va eliminato il solo stato di Helm (i secret `sh.helm.release.v1.<release>.*` nel namespace), non la release con `helm uninstall`, che cancellerebbe gli oggetti.

## Target e credenziali

Ogni target è il server API di un cluster: `host` (o un URL `https://…`) e `port` (default 6443). Le etichette `region` e `az` permettono di rilasciare un cluster alla volta con i [rilasci a ondate](../guida/ondate.md).

| Parametro | Descrizione |
|---|---|
| `token_secret` | Segreto con il bearer token, ad esempio quello di un ServiceAccount. |
| `client_cert_secret`, `client_key_secret` | In alternativa al token: certificato client e chiave PEM. |
| `ca_secret` | Segreto con la CA del cluster (PEM). Default: `ca_file` del plugin o le CA di sistema. |

Con manent in esecuzione nel cluster, il token del service account si legge con il secret provider `file:`:

```yaml title="envconfig/shop/prod.yml"
plugin: kubernetes
targets:
  - {host: kubernetes.default.svc, port: 443, labels: {region: eu-1}}
params:
  namespace: shop-prod
  token_secret: kube-token
  var.REPLICAS: "4"
secrets:
  - {name: kube-token, ref: "file:/var/run/secrets/kubernetes.io/serviceaccount/token"}
```

I permessi del token decidono cosa manent può modificare: un Role limitato al namespace dell'applicazione è la scelta tipica. Oltre agli oggetti dei manifest, servono i permessi su ConfigMap nel namespace per lo storico delle release.

## Parametri del deploy

| Parametro | Default | Descrizione |
|---|---|---|
| `namespace` | `default` | Namespace degli oggetti che non ne indicano uno, e dello storico. |
| `wait_timeout` | `5m` | Attesa del rollout dei workload. |
| `prune` | `false` | `true`: dopo il deploy elimina gli oggetti della release precedente che i nuovi manifest non contengono più. I Namespace non vengono mai eliminati. |
| `keep_releases` | `5` | Release conservate nello storico (minimo 2). |
| `require_image_digest` | `false` | `true`: rifiuta i workload con immagini non fissate al digest (`image@sha256:…`). |
| `var.NOME` | | Valore di `${NOME}` nei manifest. |
| `helm_release` | nome dell'applicazione | Nome della release Helm (`.Release.Name`). |
| `helm_values_files` | | File di values contenuti nel chart, separati da virgola, applicati in ordine. |
| `helm_values` | | Values in YAML. |
| `helm_value.<percorso>` | | Un singolo value, interpretato come YAML: `helm_value.image.tag: "1.4.0"`, `helm_value.replicaCount: "3"`. |
| `helm_hooks` | `skip` | `apply`: applica gli hook del chart come oggetti normali. |
| `helm_skip_crds` | `false` | `true`: non applica le CRD della cartella `crds/`. |

## Funzionamento

Per ogni cluster, in sequenza:

1. legge i manifest e sostituisce le variabili, oppure renderizza il chart con le capabilities del cluster; poi verifica ogni oggetto con la discovery del cluster (apiVersion e kind esistenti). Agli oggetti senza namespace assegna `namespace`;
2. aggiunge le etichette `app.kubernetes.io/managed-by: manent`, `manent.io/application`, `manent.io/environment` e le annotazioni `manent.io/version` e `manent.io/deployment`;
3. applica prima i Namespace, poi salva la release nello storico, poi applica gli altri oggetti in ordine di dipendenza (CRD, permessi, ConfigMap e Secret, Service, workload). Il server-side apply usa il field manager `manent` ed è idempotente;
4. attende il rollout di Deployment, StatefulSet e DaemonSet. Un Deployment oltre il suo `progressDeadlineSeconds` fallisce subito, senza attendere `wait_timeout`;
5. con `prune`, elimina gli oggetti rimossi dai manifest;
6. aggiorna la release in esercizio ed elimina dallo storico le release oltre `keep_releases`.

Per bloccare un deploy che non parte senza aspettare il timeout conviene impostare `progressDeadlineSeconds` nei Deployment.

## Storico e rollback

Lo storico è fatto di ConfigMap nel namespace:

- `manent-<app>-<env>`: la release in esercizio;
- `manent-<app>-<env>-<versione>`: gli oggetti applicati da ogni release, compressi (al più circa 900 KiB per release).

Il **rollback** riapplica su ogni cluster gli oggetti salvati della release precedente, ne attende il rollout e, con `prune`, elimina gli oggetti creati solo dalla release fallita. Non serve riscaricare l'artefatto precedente. La release in esercizio (locator `plugin`) si legge dallo storico; cluster con release diverse fermano il deploy nel checkpoint.

Il tipo di artefatto `kubernetes` ammette il rollback (`rollback.artifact_types.kubernetes.allowed: true` nei default).

## Rilasci per percentuale { #rilasci-per-percentuale }

Con un rollout `strategy: traffic` ([Rilasci per percentuale](../guida/traffico.md)) il plugin tiene due versioni affiancate in due **slot**, `a` e `b`, alternati a ogni rilascio, e sposta il traffico con i pesi di una `HTTPRoute` (o `GRPCRoute`) della Gateway API.

- **Oggetti dello slot.** Deployment, StatefulSet, DaemonSet, Service, HorizontalPodAutoscaler e PodDisruptionBudget dei manifest ricevono il suffisso `-<slot>` (`shop` → `shop-b`) e l'etichetta `manent.io/track: <slot>`, anche nei selettori e nei pod; `serviceName` degli StatefulSet e `scaleTargetRef` degli HPA seguono il nuovo nome. Gli altri oggetti (ConfigMap, Secret, …) sono comuni ai due slot: una modifica vale subito anche per la stabile.
- **La route** è dell'infrastruttura: manent cambia solo i pesi dei `backendRefs` `<traffic_service>-a` e `<traffic_service>-b` (deve averli entrambi) con una merge patch condizionata al `resourceVersion` letto. La regola con un match sull'header dei tester (`rollout.traffic.testers.header`) porta solo alla candidata.
- **Storico.** La ConfigMap della release in esercizio registra anche lo slot e, dopo una promozione, la release sostituita. Il ritorno ridimensiona a zero la candidata; dopo `scaleDownDelay` il ritiro porta a zero i workload dello slot sostituito ed elimina i suoi HPA.

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: shop, namespace: shop}
spec:
  parentRefs: [{name: public}]
  rules:
    - matches: [{headers: [{name: x-canary, value: always}]}]
      backendRefs: [{name: shop-a, port: 80}]
    - backendRefs:
        - {name: shop-a, port: 80, weight: 100}
        - {name: shop-b, port: 80, weight: 0}
```

| Parametro | Default | Descrizione |
|---|---|---|
| `traffic_route` | | Nome della route che divide il traffico. |
| `traffic_service` | | Nome base dei Service degli slot (`<nome>-a`, `<nome>-b`). |
| `traffic_route_kind` | `HTTPRoute` | `HTTPRoute` o `GRPCRoute`. |
| `traffic_route_namespace` | `namespace` | Namespace della route. |
| `traffic_router` | `gateway-api` | `gateway-api` o `istio`. |
| `traffic_rename` | `true` | `false` se il chart dà già i nomi per slot: manent aggiunge solo l'etichetta `manent.io/track`. |

**Istio.** Con `traffic_router: istio` la route è un `VirtualService` (`networking.istio.io/v1`): manent cambia i pesi delle route verso i subset `a` e `b`, definiti da una `DestinationRule` sull'etichetta `manent.io/track`, e la voce `http` con un match sull'header dei tester porta solo alla candidata. I Service non hanno suffisso e restano comuni ai due slot; il traffico tra servizi della mesh segue gli stessi pesi.

**Chart Helm.** Il chart riceve lo slot in `.Values.manent.slot`. Se lo usa già nei nomi degli oggetti, `traffic_rename: "false"` evita il doppio suffisso.

Un'applicazione già rilasciata senza slot (oggetti senza suffisso) passa agli slot con un'operazione esplicita: il primo rilascio per percentuale crea `shop-a` accanto a `shop` e riceve subito tutto il traffico (non c'è una versione stabile negli slot); la route va preparata con i backend `shop-a` e `shop-b` e gli oggetti senza suffisso vanno rimossi a mano dopo la verifica.

## Dry-run

In [dry-run](../guida/dry-run.md), per ogni cluster: versione e credenziali, poi ogni oggetto viene applicato con `dryRun=All`. Il cluster lo valida con schema, admission webhook e RBAC, ma non salva nulla. Gli oggetti in un namespace che i manifest stessi creerebbero non sono verificabili e vengono contati in `objects_skipped`.

## Risultato

```json
{
  "plugin": "kubernetes",
  "targets": [
    {
      "host": "k8s-eu1.example.com",
      "status": "deployed",
      "details": {"namespace": "shop-prod", "objects": "4", "release": "manent-shop-prod-1-4-0-1a2b3c4d", "previous": "1.3.0", "pruned": "ConfigMap/shop-prod/shop-legacy"}
    }
  ]
}
```
