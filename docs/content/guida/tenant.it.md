# Tenant

Un **tenant** è un cliente o un'istanza: la stessa applicazione viene rilasciata in più copie, una per tenant, in ogni ambiente. Ogni tenant ha parametri, segreti e target propri, e i suoi deploy, inventario, rollback e approvazioni. Con un solo rilascio multi-tenant può ricevere la stessa release degli altri, un gruppo alla volta.

Il tenant è una terza dimensione accanto ad applicazione e ambiente, ed è facoltativo: un deploy senza tenant si comporta come prima.

## Registro dei tenant

Ogni tenant è un documento nel provider della [configurazione di ambiente](configurazione-ambiente.md), accanto agli ambienti:

```yaml title="envconfig/tenants/acme.yml"
description: ACME S.p.A.
tags: {tier: gold, region: eu, contract: "2026"}
applications:
  shop: [staging, prod]            # ambienti a cui il tenant è collegato
  billing: [prod]
params:
  shop:
    db_host: acme-db.internal      # valori dei tenant_params dell'ambiente
secrets:
  shop:
    - {name: license, ref: "vault:secret/data/shop/acme#license"}
step_params:
  shop:
    migrate: {database: acme_shop}
disabled: false                    # un tenant disabilitato non viene mai selezionato né rilasciato
```

| Provider | Dove | Configurazione |
|---|---|---|
| `file` | `./envconfig/tenants/{tenant}.yml` | `envconfig.file.tenants` |
| `consul` | chiave `manent/tenants/{tenant}` | `envconfig.consul.tenants_key` |

- **Nome:** lettere minuscole, cifre e `-`, al più 40 caratteri. Diventa parte di ID e nomi di directory.
- **Tag:** coppie chiave-valore, scelte con gli stessi selettori delle [ondate](ondate.md) (`tier=gold, region in (eu, us)`). `name` seleziona per nome ed è riservato.
- **`applications`:** un tenant riceve deploy solo negli ambienti a cui è collegato.
- **Parametri e segreti:** per applicazione, e dei segreti solo i riferimenti.
- **Lettura:** il documento viene riletto all'avvio di ogni deploy, nello step `config`, che registra `tenant_source` e `tenant_revision`. L'elenco, usato da selettori e permessi, resta in cache per `envconfig.tenants_cache` (default 30s).

```sh
manent tenants list [-a shop] [-e prod] [--selector "tier=gold"]
manent tenants show acme              # parametri e nomi dei segreti, mai i riferimenti
```

## Configurazione di ambiente

```yaml title="envconfig/shop/prod.yml"
plugin: kubernetes
tenancy: tenanted              # untenanted (default) | tenanted | both
tenant_params:
  db_host: {pattern: '[a-z0-9.-]+', required: true}
  feature_x: {pattern: 'true|false', default: "false"}
targets:
  - {host: k8s-eu-1, labels: {region: eu, tenant: "*"}}    # condiviso da tutti i tenant
  - {host: k8s-acme, labels: {region: eu, tenant: acme}}   # dedicato
params:
  namespace: "shop-{tenant}"
secrets:
  - {name: db, ref: "vault:secret/data/shop/{tenant}#db"}
```

- **`tenancy`:**
    - `untenanted` rifiuta i deploy con un tenant: è il comportamento di ogni ambiente senza la chiave;
    - `tenanted` ne richiede uno;
    - `both` accetta tutti e due, ad esempio durante una migrazione.
- **`tenant_params`:** i parametri che ogni tenant fornisce, con `pattern` ancorato, `required` e `default`. Il deploy viene rifiutato già all'accodamento se il tenant non fornisce un parametro obbligatorio, ne fornisce uno fuori pattern o uno non dichiarato. È l'ambiente a decidere cosa un tenant può variare: un tenant non può impostare `post_command`.
- **Target:**
    - un deploy di un tenant riceve i target con `tenant: <nome>` più quelli con `tenant: "*"`;
    - un deploy senza tenant riceve quelli senza etichetta più quelli condivisi.
- **`{tenant}`** viene sostituito nei parametri, nei riferimenti ai segreti, nei parametri degli step personalizzati e nelle `secret_refs` della `descriptor_policy`. Un ambiente che lo usa accetta solo deploy con un tenant.

La **fusione** avviene in tre strati e mantiene la regola della configurazione di ambiente: i valori in conflitto sono un errore, mai una sovrascrittura.

1. L'**ambiente** è autorevole.
2. Il **tenant** aggiunge solo i parametri dichiarati, i suoi segreti e i parametri degli step personalizzati.
3. Il **descrittore** aggiunge ciò che la [descriptor policy](../operazioni/configurazione.md#descriptor_policy) permette, mai un parametro del tenant.

## Un deploy per un tenant

```yaml
application: shop
environment: prod
tenant: acme
artifact: {type: kubernetes, name: shop, version: 1.4.0, source: oci://registry/shop:1.4.0}
```

```sh
manent deploy -f deploy.yml --tenant acme
```

La partizione della coda è `<applicazione>/<ambiente>/<tenant>`: due deploy di acme non si sovrappongono mai, mentre acme e globex procedono in parallelo. `dbos.environment_concurrency` vale per tenant.

Stato, inventario, metriche DORA, eventi dei callback e messaggi in chat riportano il tenant. `--tenant` (o `?tenant=`) filtra `list`, `inventory`, `activity` e `dora`; per le metriche per tenant c'è `--by-tenant`.

**Promozione e rollback** sono per tenant:

```sh
manent promote -a shop --from staging --to prod --tenant acme
manent rollback -a shop -e prod --tenant acme [--to 1.3.2]
```

La promozione obbligatoria (`requires`) è verificata per tenant: acme in prod richiede acme in staging, non un tenant qualsiasi.

## Rilasciare a più tenant { #rilasciare-a-piu-tenant }

```yaml
application: shop
environment: prod
id: shop-1.4.0-prod
tenants:
  selector: "tier in (gold, silver), region=eu"   # oppure names: [acme, globex]
  groups:                                         # facoltativo: canary di tenant
    - {name: pilota, select: "tier=gold", bake: 30m}
    - {name: resto}
  maxParallel: 5
  onFailure: stop                                 # stop | continue
  onDenied: skip                                  # skip | reject
  approvalsBetween: [pilota]
artifact: {...}
```

```sh
manent deploy -f deploy.yml --tenants "tier=gold" [--max-parallel 5]
manent status shop-1.4.0-prod                     # stato di ogni tenant
manent cancel shop-1.4.0-prod                     # tenant non ancora conclusi
```

- **Selezione:** `names` o `selector`. Restano fuori i tenant disabilitati e quelli non collegati all'applicazione e all'ambiente. L'elenco è fissato all'accodamento: una recovery non riseleziona mai.
- **Un deploy per tenant**, con ID `<id>.<tenant>`, le sue approvazioni e il suo rollback automatico. Il rilascio è il workflow `manent.tenants`, sulla coda delle approvazioni: non occupa slot.
- **Gruppi:**
    - un gruppo parte quando il precedente è concluso e il suo `bake` è passato;
    - un gruppo senza `select` prende i tenant rimasti;
    - con `approvalsBetween` il rilascio attende un'approvazione dopo il gruppo (`manent approve <id>`), con la regola più severa tra quelle dei tenant.
- **`maxParallel`:** i deploy di tenant in esecuzione contemporanea (default 5).
- **`onFailure`:**
    - `stop` non avvia altri tenant dopo un fallimento, e quelli in corso finiscono;
    - `continue` va avanti con gli altri.
- **`onDenied`:**
    - con `skip` un tenant negato dalla [policy di governance](policy.md) viene escluso con il motivo e gli altri procedono;
    - con `reject` viene rifiutato tutto.
- **Stati** di un tenant: lo stato del deploy, oppure `PENDING_START`, `SKIPPED`, `DENIED`. Il rilascio finisce `succeeded`, `failed` o `stopped`.

Le **schedule ricorrenti** con `tenants` riselezionano i tenant a ogni esecuzione: un tenant aggiunto dopo la creazione della schedule entra nelle esecuzioni successive. `schedule.at` ritarda l'intero rilascio.

**Promozione e rollback di gruppo:**

```sh
manent promote -a shop --from staging --to prod --tenants "tier=gold"   # ognuno dalla propria release; gli altri restano fuori
manent rollback -a shop -e prod --run shop-1.4.0-prod                   # i tenant riusciti, in ordine inverso
```

Il rollback di un rilascio è un nuovo rilascio multi-tenant `<run>.rollback` con `onFailure: continue`. Restano fuori i tenant senza una release precedente (al primo rilascio).

## Governance

Il tenant entra in ogni decisione di governance:

- **Regole e freeze YAML** accettano `tenants`, un selettore:

  ```yaml
  governance:
    rules:
      - {name: gold-prod, applications: ["*"], environments: [prod], tenants: "tier=gold", approvals: {required: 2}}
    freezes:
      - {name: acme-close, applications: ["*"], environments: [prod], tenants: "name=acme", from: ..., to: ...}
  ```

  Una regola o una finestra senza `tenants` vale per ogni deploy, una con `tenants` solo per i tenant corrispondenti. `manent freeze add --tenants "tier=gold"` ne crea una a runtime.
- **Le policy Rego** ricevono `input.tenant` (`name`, `tags`) e `input.tenants_run` (schema 2 dell'input). `lib.tenant_matches(selettore, input.tenant)` applica un selettore. Vedi [Policy in Rego](policy.md).

```rego
deny contains {"code": "contract", "message": "the 2025 contracts stay on 1.0.x"} if {
	input.tenant.tags.contract == "2025"
	not startswith(input.artifact.version, "1.0.")
}
```

## Permessi

I binding RBAC accettano `tenants`, un selettore:

```yaml
rbac:
  bindings:
    - {groups: [support-acme], role: deployer, applications: ["*"], environments: [prod], tenants: "name=acme"}
    - {groups: [account-gold], role: approver, applications: ["*"], environments: [prod], tenants: "tier=gold"}
```

- Un binding senza `tenants` vale per ogni deploy, con o senza tenant: le configurazioni esistenti non cambiano.
- Un binding con `tenants` vale solo per i deploy dei tenant corrispondenti, con i tag letti dal registro.
- Un rilascio multi-tenant richiede il permesso su **ogni** tenant selezionato; approvarlo o annullarlo, su ogni tenant del rilascio.
- Gli elenchi (`list`, `inventory`, `activity`, `dora`, `tenants`) mostrano solo ciò che si può leggere.

## Da un ambiente per cliente

Prima dei tenant un cliente era spesso un ambiente (`prod-acme`). L'adozione registra la release in esercizio lì come deploy del tenant, con tipo `adoption` e senza toccare i target. Così promozioni e rollback del tenant ripartono dalla release giusta:

```sh
manent-server -c manent.yml admin tenants-adopt -a shop --environment prod-acme --as prod --tenant acme
```

## Terraform

`manent_deployment` accetta `tenant`, e la data source `manent_tenants` elenca i tenant per un `for_each`:

```hcl
data "manent_tenants" "gold" {
  application = "shop"
  environment = "prod"
  selector    = "tier=gold"
}

resource "manent_deployment" "shop" {
  for_each    = toset(data.manent_tenants.gold.names)
  application = "shop"
  environment = "prod"
  tenant      = each.key
  descriptor  = file("deploy/prod.yml")
  version     = var.version
}
```

## Non coperto

- Un'API per scrivere il registro: i tenant stanno nel provider, gestiti con Git, Terraform o Consul.
- Un'unica approvazione per un intero rilascio: ce n'è una per ogni deploy di tenant, più quelle tra i gruppi.
- L'etichetta `tenant` sulle metriche Prometheus, la cui cardinalità crescerebbe con i tenant.
