# Design: tenant

| | |
|---|---|
| Stato | Superato: il tenant ora è il team proprietario delle applicazioni (vedi [Tenant](../content/guida/tenant.it.md)); il modello per cliente descritto qui è stato rimosso |
| Data | 2026-10-18 |
| Ambito | `internal/envconfig`, `internal/descriptor`, `internal/engine` (submit, coda, inventario, governance, release), `internal/authz`, `internal/policy`, `internal/cluster`, API/CLI/UI, provider Terraform |

## 1. Contesto

In Octopus Deploy un **tenant** è un cliente o un'istanza: lo stesso progetto viene rilasciato in più copie, una per tenant, in ogni ambiente. Ogni tenant ha le sue variabili, i suoi target, i suoi ambienti collegati e i suoi permessi. Un rilascio può riguardare un tenant o un gruppo di tenant scelto per tag. La dashboard mostra la matrice tenant × ambiente.

manent oggi ha due dimensioni, `application` ed `environment`, e chi deve servire più clienti ha tre ripieghi:

| Ripiego | Come | Problema |
|---|---|---|
| Un ambiente per cliente | `prod-acme`, `prod-globex` | Gli ambienti si moltiplicano: regole e freeze vanno ripetuti con i glob, la promozione `requires: [preprod]` non sa più quale preprod. Non c'è un modo per rilasciare a tutti i clienti insieme. |
| Un'applicazione per cliente | `shop-acme`, `shop-globex` | L'inventario e le metriche DORA si frammentano. Lo stesso artefatto ha nomi diversi e la promozione non lo riconosce. |
| Un'etichetta sui target | `labels: {tenant: acme}` con le ondate | Un solo deploy per tutti: parametri e segreti per cliente non esistono, e un cliente fallito fa fallire o tornare indietro tutti gli altri. |

Nessuno dei tre dà quello che serve davvero:

- parametri e segreti per cliente;
- un deploy, uno stato, un rollback e un'approvazione per cliente;
- il rilascio a un gruppo di clienti con un limite di parallelismo;
- regole e permessi per cliente o per tag.

## 2. Obiettivi e non obiettivi

**Obiettivi**

- Il tenant come **terza dimensione facoltativa**, accanto ad application ed environment. Un deploy senza tenant si comporta esattamente come oggi.
- Un **registro dei tenant** con tag, ambienti collegati per applicazione, parametri e segreti, letto dallo stesso provider della configurazione di ambiente (`file`, `consul`) e modificabile senza riavvii.
- **Template di parametri** dichiarati dalla configurazione di ambiente e valorizzati dal tenant, con validazione e con le stesse regole di fusione di oggi (l'ambiente resta autorevole).
- Un **deploy per tenant**, con stato, log, inventario, rollback e approvazioni propri.
- Il **rilascio multi-tenant**: un descrittore che sceglie i tenant per nome o per tag e li rilascia con un limite di parallelismo, a gruppi (canary di tenant) e con una politica in caso di errore.
- Il tenant nella **governance** (input delle policy Rego, regole YAML, freeze) e nell'**RBAC** (binding per tenant o per tag).
- La **matrice tenant × ambiente** nell'interfaccia web, nella CLI e nel provider Terraform.

**Non obiettivi, per ora**

- L'isolamento tra tenant a livello di infrastruttura di manent (database o worker separati per cliente). I worker restano condivisi; le region già permettono di dedicare worker a una rete.
- Tenant gerarchici (cliente → sottocliente).
- Un registro dei tenant scritto da manent con un'API di scrittura. In questa proposta il registro arriva dal provider (§4); un'API di scrittura potrà venire in una fase successiva, sul modello dello store delle policy.
- Fatturazione o quote per tenant.

## 3. Proposta in sintesi

1. **Registro:** un documento per tenant nel provider della configurazione (`tenants/<tenant>.yml` o la chiave Consul `manent/tenants/<tenant>`), con tag, ambienti collegati, parametri e segreti.
2. **Configurazione di ambiente:** due campi nuovi.
    - `tenancy` (`untenanted`, `tenanted`, `both`) dice se l'ambiente vuole un tenant.
    - `tenant_params` dichiara i parametri che ogni tenant deve fornire, con pattern e default.

    I target del tenant si scelgono per etichetta (`tenant=<nome>`), con i target condivisi marcati `tenant: "*"`.
3. **Fusione** in tre strati: ambiente (autorevole), tenant (solo i parametri dichiarati in `tenant_params` e i segreti), descrittore (entro la `descriptor_policy`, come oggi).
4. **Esecuzione:** un deploy ha al più un tenant (`Request.Tenant`). La partizione della coda diventa `<app>/<env>/<tenant>`, quindi due deploy dello stesso tenant non si sovrappongono mentre tenant diversi procedono in parallelo.
5. **Multi-tenant:** un descrittore con `tenants` (nomi, selettore di tag, gruppi) diventa un workflow nuovo, `manent.tenants`. Fotografa l'elenco dei tenant all'accodamento, accoda un deploy per tenant con ID `<id>.<tenant>` rispettando `maxParallel` e i gruppi, e applica `onFailure`.
6. **Governance e accesso:**
    - `input.tenant` (nome e tag) per le policy Rego;
    - `tenants` nelle regole YAML, nei freeze e nei binding RBAC;
    - `requires` verificato per tenant.
7. **Superficie:**
    - API `/api/v1/tenants`, filtro `tenant` dove oggi ci sono application ed environment;
    - CLI `--tenant`/`--tenants`;
    - matrice nell'interfaccia web;
    - attributo `tenant` nel provider Terraform.

## 4. Registro dei tenant

### 4.1 Documento

```yaml title="envconfig/tenants/acme.yml"
name: acme
description: ACME S.p.A.
tags: {tier: gold, region: eu, contract: "2026"}
applications:
  shop: [staging, prod]            # environments the tenant is connected to
  billing: [prod]
params:
  shop:
    db_host: acme-db.internal
    feature_x: "true"
secrets:
  shop:
    - {name: db-password, ref: "vault:secret/data/shop/acme#db"}
step_params:
  shop:
    migrate: {database: acme_shop}
disabled: false                    # a disabled tenant is never selected nor deployable
```

- **Nome:** `[a-z0-9][a-z0-9-]{0,39}`. Diventa parte di ID di workflow, di directory e di chiavi: la stessa regola già applicata all'`id` del descrittore.
- **Tag:** coppie chiave-valore, interrogate con i selettori già usati dalle ondate (`labels.Parse`: `tier=gold,region in (eu,us)`).
- **`applications`:** il collegamento tenant ↔ applicazione ↔ ambienti. Un tenant non collegato a `shop/prod` non può ricevere un deploy di `shop` in `prod`: è il controllo che Octopus fa con i *connected projects*.
- **Parametri e segreti per applicazione:** solo riferimenti ai segreti, mai valori, come per la configurazione di ambiente. I parametri devono essere dichiarati in `tenant_params` dell'ambiente (§5): un parametro non dichiarato è un errore.

### 4.2 Provider

Il `Provider` di `internal/envconfig` si estende con un'interfaccia facoltativa:

```go
// TenantProvider reads the tenant registry.
type TenantProvider interface {
	Tenant(ctx context.Context, name string) (*TenantDocument, error)
	// Tenants lists all tenants (name, tags, connections), for selectors and listings.
	Tenants(ctx context.Context) ([]TenantSummary, error)
}
```

| Provider | Dove | Elenco |
|---|---|---|
| `file` | `envconfig.file.tenants` (default `./envconfig/tenants/{tenant}.yml`) | glob della directory |
| `consul` | `envconfig.consul.tenants_key` (default `manent/tenants/{tenant}`) | `GET /v1/kv/manent/tenants/?recurse` |

`TenantDocument` porta `Source` e `Revision`, come `Document`, così lo step `config` registra quale versione del tenant ha usato. L'elenco si tiene in cache per processo (`envconfig.tenants_cache`, default 30s). Serve ai selettori al submit e ai listing; il documento del singolo tenant si rilegge sempre allo step `config`.

### 4.3 Perché nel provider e non nel database

La configurazione di ambiente è già lì, gestita con Git, Terraform o Consul e verificabile con `manent env validate`. I tenant di solito nascono dall'onboarding di un cliente, che è un processo dell'infrastruttura. Metterli nel database di manent obbligherebbe a un secondo sistema di gestione. Uno store nel database (§13) resta possibile per chi non ha un provider e vuole gestirli da API, come lo store delle policy.

## 5. Configurazione di ambiente

```yaml title="envconfig/shop/prod.yml"
plugin: kubernetes
tenancy: tenanted              # untenanted (default) | tenanted | both
tenant_params:
  db_host: {pattern: '^[a-z0-9.-]+$', required: true}
  feature_x: {pattern: '^(true|false)$', default: "false"}
  replicas: {pattern: '^[0-9]{1,2}$', default: "2"}
targets:
  - {host: k8s-eu-1, labels: {region: eu, tenant: "*"}}          # shared by all tenants
  - {host: k8s-acme, labels: {region: eu, tenant: acme}}         # dedicated
params:
  namespace: "shop-{tenant}"                                     # placeholder, see below
secrets:
  - {name: registry, ref: "vault:secret/data/shop/registry"}
```

- **`tenancy`:**
    - `untenanted` è il comportamento di oggi, e un deploy con tenant viene rifiutato;
    - `tenanted` richiede un tenant;
    - `both` accetta tutti e due (utile durante una migrazione).
- **`tenant_params`:** lo schema dei parametri forniti dal tenant, con lo stesso modello dei `params` degli step personalizzati (`customspec`): pattern ancorato, `required`, `default`. Un tenant che non fornisce un parametro richiesto, o ne fornisce uno fuori pattern, fa fallire il deploy in modo permanente, già al submit se il registro è leggibile.
- **Target:**
    - un tenant riceve i target con `tenant=<nome>` più quelli con `tenant: "*"`;
    - senza target dedicati riceve solo quelli condivisi;
    - un ambiente `tenanted` senza alcun target per il tenant è un errore.

    I target restano nella configurazione di ambiente e non nel registro, perché chi gestisce l'infrastruttura di un ambiente decide anche dove gira ogni cliente.
- **Segnaposto `{tenant}`:** nei valori di `params`, nei `ref` dei segreti e nelle `secret_refs` della `descriptor_policy`, come già `{application}` ed `{environment}`. Copre i casi frequenti, come un namespace per cliente o un percorso Vault per cliente, senza un parametro di tenant.

### 5.1 Fusione

`envconfig.Merge` diventa a tre strati e conserva la regola di oggi (i valori in conflitto sono un errore, mai una sovrascrittura):

1. **Ambiente:** autorevole. I segnaposto `{tenant}` vengono sostituiti.
2. **Tenant:** solo i parametri dichiarati in `tenant_params` (più i default non forniti), i segreti e gli `step_params` del tenant per quell'applicazione. Un parametro del tenant che l'ambiente definisce già è un conflitto.
3. **Descrittore:** come oggi, entro la `descriptor_policy`. Il descrittore non può impostare un parametro dichiarato in `tenant_params`: è del tenant.

`Resolver.Resolve` riceve il tenant e restituisce nei metadati dello step `config` anche `tenant_source` e `tenant_revision`.

## 6. Descrittore

### 6.1 Un tenant

```yaml
application: shop
environment: prod
tenant: acme
artifact: {type: kubernetes, name: shop, version: 1.4.0, source: oci://registry/shop:1.4.0}
```

Il deploy ha ID `<id>` (o un UUID). La chiave di partizione è `shop/prod/acme`. `environment_concurrency` vale quindi per tenant: due deploy di acme non si sovrappongono, mentre acme e globex procedono in parallelo.

### 6.2 Più tenant

```yaml
application: shop
environment: prod
id: shop-1.4.0-prod
tenants:
  selector: "tier in (gold, silver), region=eu"   # or names: [acme, globex]
  groups:                                         # optional: tenant canary
    - {name: pilota, select: "tier=silver", bake: 30m}
    - {name: resto}
  maxParallel: 5
  onFailure: stop                                 # stop | continue
artifact: {...}
```

- **Selezione:** `names` o `selector`, mai entrambi. I tenant disabilitati o non collegati all'applicazione e all'ambiente non vengono selezionati.
- **Gruppi:** come le ondate ma tra tenant. Un gruppo parte solo quando il precedente è concluso e il suo `bake` è passato senza fallimenti. Un gruppo senza `select` prende i tenant rimasti. Le approvazioni tra gruppi (`approvalsBetween`) usano lo stesso meccanismo delle ondate.
- **`maxParallel`:** il numero di deploy di tenant in esecuzione contemporanea, su tutti i worker.
- **`onFailure`:**
    - `stop` non avvia i tenant non ancora partiti (quelli in corso finiscono);
    - `continue` va avanti con gli altri.

    Il rollback resta per tenant (automatico nel deploy del tenant, o manuale); un rollback di massa si chiede esplicitamente (§7.4).
- **Incompatibilità:** `tenant` e `tenants` sono alternativi. `tenants` non è ammesso in una release composta (`kind: Release`) nella prima versione, e con `schedule.at` sì. Con `schedule.cron` vedi §7.5.

## 7. Esecuzione

### 7.1 Richiesta e attributi

`Request` aggiunge:

- `Tenant string`, il tenant del deploy;
- `Tenants *TenantRun`, solo per il workflow multi-tenant: elenco fotografato, gruppi, parallelismo, `onFailure`.

L'attributo DBOS `tenant` si aggiunge ai workflow con un tenant; `toStatus` lo legge in `Status.Tenant`. I workflow senza l'attributo sono untenanted.

### 7.2 Partizione e concorrenza

`partitionKey(d)` diventa `<app>/<env>/<tenant>` se il tenant c'è, altrimenti resta `<app>/<env>`: i workflow già accodati e quelli untenanted non cambiano partizione.

| Limite | Ambito |
|---|---|
| `environment_concurrency` | per partizione: per tenant se c'è, per ambiente altrimenti |
| `tenants.maxParallel` | per rilascio multi-tenant |
| `dbos.tenant_concurrency` (nuovo, default 0 = nessun limite) | deploy con tenant in esecuzione per worker, così un rilascio a 500 clienti non satura un worker a scapito degli altri ambienti |

`dbos.tenant_concurrency` richiede una seconda coda DBOS: `<coda>-tenants`, partizionata allo stesso modo e con la propria `WorkerConcurrency`. Ci finiscono solo i deploy con tenant. I worker ascoltano entrambe le code; con le region, anche la variante `<coda>-tenants.<region>`.

### 7.3 Workflow `manent.tenants`

Il nome è nuovo; i workflow esistenti non cambiano. Gira sulla coda delle approvazioni (non partizionata, non occupa slot), come `manent.release`.

1. **Step `tenants:select`:** risolve il selettore sull'elenco **al momento dell'accodamento** e lo fotografa nell'input. In realtà la selezione la fa già `Engine.submit`, così l'utente vede subito l'elenco e gli errori. Il workflow riceve l'elenco fisso: una recovery non riseleziona.
2. **Per ogni gruppo:**
    - accoda i deploy dei tenant (`<id>.<tenant>`, `Request.Tenant`, attributo `tenants_run` = ID del workflow) fino a `maxParallel`;
    - attende le conclusioni con `dbos.Recv` sul topic `chain`, lo stesso meccanismo di `waitChain` e `notifyWaiters`, e ne accoda altri;
    - alla fine del gruppo applica il `bake` (sleep durevole) e le eventuali approvazioni.
3. **Stato** pubblicato con `SetEvent("tenants")`: per tenant `pending`, `running`, `succeeded`, `failed`, `skipped`; per gruppo il progresso.
4. **`onFailure: stop`:** i tenant non accodati diventano `skipped`; `Cancel` del workflow annulla quelli in coda, come `CancelRelease`.

`Status` di `<id>` restituisce lo stato del workflow `manent.tenants`. `Status` di `<id>.<tenant>` restituisce il deploy del singolo tenant. `manent tenants run status <id>` mostra la tabella.

### 7.4 Promozione e rollback

- `manent promote -a shop --from staging --to prod --tenant acme`: promozione del singolo tenant. `requires` è verificato **per tenant**: acme in prod richiede acme in staging, non un tenant qualsiasi. Senza `--tenant`, su un ambiente `tenanted` la promozione chiede `--tenants <selettore>` e diventa un rilascio multi-tenant.
- `manent rollback -a shop -e prod --tenant acme [--to 1.3.2]`: dall'inventario del tenant.
- `manent rollback -a shop -e prod --tenants run:<id>`: rollback dei tenant riusciti di un rilascio multi-tenant, in ordine inverso, come l'`onFailure: rollback` delle release. È un'azione esplicita, perché tornare indietro su cinquecento clienti per un fallimento è raramente la scelta giusta.

### 7.5 Schedule

Una schedule cron con `tenants` riesegue il selettore a ogni tick: un tenant aggiunto dopo la creazione entra nelle esecuzioni successive. Lo step `tenants:select` del workflow del tick registra l'elenco di quel tick. Con un singolo `tenant` la schedule si comporta come oggi.

## 8. Governance

### 8.1 Policy Rego

L'input (`governance.Input`, schema 2) aggiunge:

```json
"tenant": {"name": "acme", "tags": {"tier": "gold", "region": "eu"}},
"tenants_run": {"id": "shop-1.4.0-prod", "count": 42, "group": "pilota"}
```

Per un deploy untenanted `tenant` manca: le regole che lo richiedono non scattano. Esempio:

```rego
approvals := {"required": 2, "approvers": {"groups": ["account-gold"]}} if {
	input.environment == "prod"
	input.tenant.tags.tier == "gold"
}

deny contains {"code": "contract", "message": "the 2025 contracts stay on 1.x"} if {
	input.tenant.tags.contract == "2025"
	not startswith(input.artifact.version, "1.")
}
```

La fase `submit` di un rilascio multi-tenant valuta la policy **una volta per tenant**, ognuno con il proprio input, e salva le decisioni nell'input del workflow (`Request.Policy` per deploy). Un tenant negato dalla policy viene escluso con il motivo, oppure fa rifiutare tutto il rilascio con `tenants.onDenied: reject`. Il default è `skip`: un cliente con il contratto vecchio non blocca gli altri.

### 8.2 Regole YAML e freeze

```yaml
governance:
  rules:
    - name: gold-prod
      applications: ["*"]
      environments: [prod]
      tenants: "tier=gold"          # selector; absent = any tenant and untenanted
      approvals: {required: 2}
  freezes:
    - name: acme-quarter-close
      applications: ["*"]
      environments: [prod]
      tenants: "name=acme"
      from: 2026-12-28T00:00:00+01:00
      to: 2027-01-04T00:00:00+01:00
```

`manent freeze add --tenants "tier=gold"` crea un freeze per tenant a runtime. `manent.lib.rules` e `governance.Window.applies` imparano il selettore sui tenant.

### 8.3 Approvazioni

Ogni deploy di tenant ha la propria approvazione. Per un rilascio a molti tenant si propongono due modalità:

- `approvals.scope: tenant` (default): un'approvazione per tenant;
- `approvals.scope: run`: una sola approvazione per il rilascio multi-tenant, chiesta dal workflow `manent.tenants` prima del primo gruppo, che vale per tutti i suoi tenant. La decisione viene registrata nei deploy figli come `approval.source: <run>`.

## 9. Accesso

I binding RBAC aggiungono `tenants` (nomi o selettore di tag):

```yaml
rbac:
  bindings:
    - {groups: [support-acme], role: deployer, applications: ["*"], environments: [prod], tenants: "name=acme"}
    - {groups: [account-gold], role: approver, applications: ["*"], environments: [prod], tenants: "tier=gold"}
```

- **Default:** un binding senza `tenants` vale per tutti i tenant e per i deploy untenanted, così le configurazioni di oggi non cambiano.
- **Tag nel Guard:** `Policy.Allowed` riceve il tenant con i suoi tag, letti dall'elenco in cache del provider.
- **Multi-tenant:** un rilascio multi-tenant richiede il permesso su **ogni** tenant selezionato. Il Guard confronta l'elenco risolto, non il selettore.
- **Listing:** `List`, `Inventory`, `Activity` e `DORA` filtrano per tenant come oggi per application ed environment.
- **Policy Rego:** l'RBAC resta fuori dalle policy (vedi il design della governance, §10).

## 10. Inventario, riconciliazione, metriche

- **Inventario:** la chiave diventa `app/env/tenant`. `InventoryEntry` aggiunge `Tenant`. `inventoryCache` raggruppa per la terza dimensione. La vista per applicazione mostra la matrice tenant × ambiente, con la versione in ogni cella e le celle fuori sincronia evidenziate.
- **Riconciliazione:** per ogni tenant del deploy in `current` si leggono i suoi target (dedicati e condivisi). Sui target condivisi con un'installazione per tenant (ad esempio un namespace per cliente) il plugin legge la release del tenant con i parametri del tenant. Il `TargetStore` aggiunge la colonna `tenant` (migrazione con `addColumns`); i record senza tenant restano validi.
- **DORA:** metriche per tenant e aggregate. `manent dora --tenant acme` e un raggruppamento `--by tenant`.
- **Prometheus:** l'etichetta `tenant` su `manent_deployments_total` e `manent_targets` solo con `observability.tenant_labels: true`, perché con migliaia di tenant la cardinalità esploderebbe.
- **Callback, chat e change:**
    - **Callback:** gli eventi portano `tenant`.
    - **Chat:** le notifiche di un rilascio multi-tenant sono una per il workflow, con il riepilogo e i tenant falliti, non una per tenant.
    - **Change:** `change.scope: run` apre un ticket per l'intero rilascio, `tenant` uno per tenant.

## 11. API, CLI, interfaccia web, Terraform

**API**

| Metodo e percorso | Note |
|---|---|
| `GET /api/v1/tenants?selector=&application=&environment=` | elenco con tag e ambienti collegati (filtrato dai permessi) |
| `GET /api/v1/tenants/:name` | documento del tenant: parametri e nomi dei segreti, mai i riferimenti |
| `GET /api/v1/tenant-runs/:id`, `POST /api/v1/tenant-runs/:id/cancel` | rilascio multi-tenant |
| filtro `tenant=` | su `deployments`, `inventory`, `activity`, `metrics/dora`, `freezes` |

Gli endpoint vanno in `api/openapi.yaml`, e il servizio aggiunge `TenantService` in `service`, `engine`, `authz.Guard`, `api`, `apiclient` e nei servizi finti.

**CLI**

```sh
manent tenants list [-a shop] [-e prod] [--selector "tier=gold"]
manent tenants show acme
manent deploy -f deploy.yml --tenant acme
manent deploy -f deploy.yml --tenants "tier=gold" [--max-parallel 5]
manent tenants run status <id> | cancel <id>
manent inventory -a shop --tenant acme | --by tenant
manent promote -a shop --from staging --to prod --tenant acme
manent rollback -a shop -e prod --tenant acme
```

**Interfaccia web**

- matrice tenant × ambiente per applicazione, con filtro per tag;
- pagina del rilascio multi-tenant (gruppi, stato per tenant, ritentare i falliti);
- filtro tenant nelle viste esistenti.

**Terraform:** `manent_deployment` aggiunge `tenant`; la data source nuova `manent_tenants` (selettore) permette `for_each` su un deploy per tenant. In alternativa `manent_tenant_rollout` modella un rilascio multi-tenant come risorsa.

## 12. Compatibilità e migrazione

- **Default:** senza `tenancy` e senza `tenant` nel descrittore nulla cambia: partizione, ID, attributi, inventario, regole, binding e metriche restano quelli di oggi.
- **Workflow:** i nomi esistenti non cambiano; si aggiunge `manent.tenants`. Gli step nuovi (`tenants:select`) esistono solo nel workflow nuovo. Nel deploy di un tenant la sequenza degli step è quella di oggi, con un tenant nei metadati di `config`.
- **Dall'ambiente per cliente:** i deploy storici di `prod-acme` restano nell'inventario. Un comando `manent-server admin tenants adopt --environment prod-acme --as prod --tenant acme` registra nell'inventario la release in esercizio come deploy di `prod/acme`, con `kind: adoption` e senza toccare i target, così la promozione e il rollback ripartono dalla release giusta.
- **Schema dell'input delle policy:** si passa a `schema: 2` aggiungendo solo campi, come previsto dal design della governance.

## 13. Fasi

1. **Modello e singolo tenant:**
    - registro dei tenant nei provider `file` e `consul`;
    - `tenancy`, `tenant_params`, target per etichetta, segnaposto `{tenant}`;
    - fusione a tre strati e `descriptor.tenant`;
    - `Request.Tenant`, attributo, partizione, inventario per tenant;
    - `requires` per tenant, `input.tenant` nelle policy, filtri API e CLI.

    Con questa fase il ripiego "un ambiente per cliente" non serve più.
2. **Accesso e governance:** `tenants` nei binding RBAC, nelle regole e nei freeze (anche a runtime), approvazioni per tenant, riconciliazione e DORA per tenant.
3. **Multi-tenant:** workflow `manent.tenants` con selezione fotografata, `maxParallel`, gruppi con bake e approvazioni, `onFailure`, `onDenied`, schedule, coda `-tenants` con `tenant_concurrency`, approvazione e change con `scope: run`.
4. **Superficie:**
    - matrice e pagina del rilascio nell'interfaccia web;
    - provider Terraform;
    - `admin tenants adopt`;
    - caso d'uso UC24: tre tenant su target dedicati e condivisi, un rilascio multi-tenant con un tenant pilota, un tenant negato dal contratto, permessi del supporto limitati a un cliente, freeze per tag.
5. **Store dei tenant** (facoltativo): registro nel database con API di scrittura, per chi non ha un provider, sul modello dello store delle policy (revisioni, audit).

## 14. Alternative considerate

| Alternativa | Perché scartata |
|---|---|
| Tenant come etichetta sui target, con le ondate | Un solo deploy per tutti i clienti: niente parametri e segreti per cliente, un fallimento coinvolge tutti, stato e rollback per cliente impossibili. Le ondate restano per la topologia (region, AZ), i tenant per i clienti. |
| Tenant come prefisso dell'ambiente (`prod-acme`) con una convenzione | È il ripiego di oggi (§1). Rende impossibili la promozione per tenant, il rilascio a un gruppo e i permessi per tag. |
| Un deploy multi-tenant come release composta | Le release hanno un grafo di dipendenze statico e scritto a mano. Per i tenant servono un elenco dinamico, il parallelismo limitato e i gruppi. Si riusano i meccanismi (attesa con `Recv` sul topic `chain`, annullamento in cascata, stato con `SetEvent`), non il modello. |
| Registro dei tenant nel database fin da subito | Duplica il sistema in cui la configurazione di ambiente vive già. Lo store resta come fase facoltativa. |
| Parametri del tenant senza schema (qualunque chiave) | Un tenant potrebbe impostare parametri pericolosi (`post_command`) che l'ambiente non prevede: è lo stesso motivo della `descriptor_policy`. Con `tenant_params` è l'ambiente a decidere che cosa un tenant può variare. |

## 15. Questioni aperte

1. **`tenants.onDenied`:** il default `skip` (un tenant negato dalla policy viene escluso e gli altri procedono) è quello giusto, o è più sicuro `reject`?
2. **Approvazione `scope: run`:** una sola approvazione per centinaia di clienti è accettabile per gli auditor? Forse solo con una policy che la consente esplicitamente.
3. **Target condivisi con installazione per tenant** (un namespace o una directory per cliente sullo stesso host): basta il segnaposto `{tenant}` nei parametri, oppure i plugin devono sapere esplicitamente del tenant (nomi degli oggetti, storico in ConfigMap per tenant)?
4. **Cardinalità:** oltre quale numero di tenant serve paginare la matrice e l'inventario lato server, invece di filtrare in memoria come oggi?
5. **Variabili condivise tra applicazioni** (le *library variable set* di Octopus): servono dal primo giorno, o bastano i parametri per applicazione nel documento del tenant?

## 16. Implementazione

Implementate le fasi 1–4 (§13). La fase 5, lo store dei tenant nel database, resta facoltativa e non è stata fatta. Per le questioni aperte (§15) sono state scelte le proposte del documento:

- `onDenied: skip` di default;
- approvazioni per tenant, senza `scope: run`;
- per i target condivisi basta il segnaposto `{tenant}`;
- filtri in memoria;
- parametri per applicazione, senza variabili condivise.

Differenze rispetto al testo:

| Punto | Proposta | Implementazione |
|---|---|---|
| Coda dei tenant (`dbos.tenant_concurrency`, coda `-tenants`) | limite per worker dei deploy di tenant | non implementata: `maxParallel` per rilascio e `environment_concurrency` per tenant bastano. Può essere aggiunta senza cambiare i workflow. |
| Approvazione e change con `scope: run` | un'approvazione e un ticket per il rilascio | non implementati (questione aperta §15.2): un'approvazione per deploy di tenant più quelle tra i gruppi (`approvalsBetween`). |
| Notifiche in chat del rilascio | una per il rilascio | una per deploy di tenant, con il tenant nel messaggio. |
| `manent tenants run status|cancel` | comandi dedicati | `manent status|cancel|approve <id del rilascio>`: lo stato è quello del rilascio, con il dettaglio per tenant. |
| `admin tenants adopt` | sottocomando | `manent-server admin tenants-adopt`. |
| Etichetta `tenant` sulle metriche | con `observability.tenant_labels` | non implementata (cardinalità). |
| Riconciliazione sui target condivisi | con i parametri del tenant | sì: il resolver usa il tenant del deploy in esercizio; i record sono salvati con la chiave `scope|tenant`, senza migrazione. |
| Rollback di un rilascio | in ordine inverso | sì, come rilascio `<run>.rollback` con `onFailure: continue`; i tenant senza release precedente sono saltati. |
| Release composte con `tenants` | escluse nella prima versione | escluse (`tenant` singolo per deploy ammesso). |

Codice:

- `internal/tenant`: modello, selezione, cache;
- `internal/envconfig/tenant.go`: registro, fusione;
- `internal/engine/tenants.go` e `tenantrun.go`: deploy, rilascio multi-tenant, adozione;
- `internal/authz/tenants.go`;
- `internal/api/tenants.go`;
- CLI `manent tenants`, `--tenant`/`--tenants`;
- web UI: inventario e dettaglio;
- provider Terraform: `tenant`, `manent_tenants`;
- caso d'uso UC24;
- documentazione `guida/tenant`.

