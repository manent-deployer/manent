# Tenant

Un **tenant** è un team interno che possiede un insieme di applicazioni. Il tenant elenca nel registro le sue applicazioni con i loro **tag** (prodotto, criticità, …) ed è lo scope di permessi, code e restrizioni. L'identità di una release è **tenant/applicazione/artefatto/ambiente**: due team possono avere ciascuno un'applicazione chiamata `shop`, e le applicazioni che nessun tenant elenca appartengono al tenant riservato `untenanted`.

## Registro

Il registro è un unico file YAML, `tenants.yml`:

```yaml title="tenants.yml"
tenants:
  payments:
    description: Team pagamenti
    tags: {division: commerce, cost_center: "4410"}
    applications:
      shop: {tags: {product: checkout, criticality: high}}
      billing: {tags: {product: invoicing}}
    members: {groups: [payments-eng]}       # ruolo implicito rbac.tenant_roles.members
    admins: {subjects: ["oidc:alice"]}       # ruolo implicito rbac.tenant_roles.admins
    queue: deployments.payments              # coda dei deploy propria (facoltativa)
    concurrency: 2                           # quota della coda (facoltativa)
    secret_refs: ["vault:secret/payments/*"] # riferimenti ai segreti ammessi (facoltativo)
    artifact_sources: ["oci://registry.example.com/payments/"]   # sorgenti ammesse (facoltativo)
    callback_hosts: [hooks.payments.example.com]                 # host dei callback ammessi (facoltativo)
  retail:
    applications:
      shop: {tags: {product: storefront}}   # un altro shop: la release retail/shop
    disabled: false                          # un tenant disabilitato non può rilasciare
```

```yaml title="manent.yml"
tenants:
  file: /etc/manent/tenants.yml
  cache: 30s           # per quanto si tiene il registro prima di rileggerlo
  untenanted: allow    # allow (default) | reject: le applicazioni che nessun tenant elenca
```

- **Nomi:** i tenant usano lettere minuscole, cifre e `-` (al più 40); `untenanted` è riservato. Le applicazioni seguono la regola di `artifact.name`. Nei tag delle applicazioni sono riservati `application`, `tenant` e il prefisso `tenant.`, in quelli dei tenant `name`.
- **Stesso nome in più tenant:** ammesso. Ogni coppia tenant/applicazione è una release a sé, con storico, inventario, configurazione e permessi propri.
- **Tag solo nel registro:** il descrittore non può aggiungere tag, e il suo campo `tenant` sceglie soltanto tra i tenant che elencano l'applicazione.
- **Registro non valido:** se `tenants.yml` diventa illeggibile o non valido a server avviati, resta in vigore l'ultimo registro valido. manent registra un avviso (anche nel SIEM) e `/readyz` risponde 503 finché il file non viene corretto.
- **Revisione:** la sorgente del registro e la sua revisione (un prefisso dello sha256 del file) sono in `manent tenants show`.

```sh
manent tenants list [-a <applicazione>] [--selector "division=commerce"]
manent tenants show payments        # tag, applicazioni con i loro tag, coda
```

## Il tenant di un deploy

Il tenant viene dal registro. Il campo `tenant` del descrittore (o `--tenant`) serve solo quando più tenant elencano l'applicazione:

| Caso | Tenant del deploy |
|---|---|
| Un solo tenant elenca l'applicazione, `tenant` assente | Quel tenant |
| Più tenant la elencano, `tenant` assente | Rifiutato: l'errore elenca i tenant |
| `tenant` indicato e quel tenant elenca l'applicazione | Quel tenant |
| `tenant` indicato e quel tenant non la elenca | Rifiutato |
| Nessun tenant elenca l'applicazione | `untenanted`, oppure rifiutato con `tenants.untenanted: reject` |
| Il tenant è disabilitato | Rifiutato, mai `untenanted` |

```yaml title="deploy.yml"
application: shop
tenant: retail          # necessario solo perché shop è in due tenant
environment: prod
```

Il tenant risolto viene registrato nella richiesta e nel descrittore del deploy (attributo `tenant`, `Status.tenant`, campo `tenant` di callback e messaggi chat, esiti della CI). Promozioni e rollback restano nella sua identità. La stessa scelta vale per `rollback`, `promote`, `env validate` e `policy access` con `--tenant` (`-t`), e per `imports.<nome>.current.tenant`.

```sh
manent deploy -f deploy.yml --tenant retail
manent rollback -a shop -e prod --tenant payments
manent list -t payments              # deploy delle applicazioni di payments
manent inventory -t payments
```

Senza registro ogni applicazione è `untenanted`: per chi non usa i tenant non cambia nulla.

## Il tenant è necessario per rilasciare?

| Configurazione | Deploy di un'applicazione che nessun tenant elenca |
|---|---|
| Nessun registro (`tenants.file` vuoto, il default) | Ammesso, come `untenanted`. |
| Registro, `tenants.untenanted: allow` (default) | Ammesso, come `untenanted`: applicazioni dei tenant e applicazioni senza tenant coesistono. |
| Registro, `tenants.untenanted: reject` | Rifiutato all'invio: `application <nome> is not registered in any tenant`. |

Un'applicazione `untenanted` non ha coda, restrizioni né ruoli impliciti: danno accesso solo i binding espliciti con pattern senza prefisso (`applications: [shop]`).

## Configurazione di ambiente

I template di default contengono `{tenant}`, così due applicazioni con lo stesso nome leggono documenti diversi:

| Provider | Template di default |
|---|---|
| `file` | `./envconfig/{tenant}/{application}/{artifact}/{environment}.yml` |
| `consul` | `manent/{tenant}/{application}/{artifact}/{environment}` |
| `git` | `{tenant}/{application}/{artifact}/{environment}.yml` |

Le applicazioni senza tenant usano il segmento `untenanted`. `{tenant}` è disponibile anche in `descriptor_policy.secret_refs`. Un template senza `{tenant}` resta valido se i nomi delle applicazioni sono unici.

## Pattern delle applicazioni qualificati

In binding, grant della policy dei permessi, regole di governance, freeze e instradamento delle notifiche i pattern delle applicazioni sono **qualificati**:

| Pattern | Vale per |
|---|---|
| `shop`, `uc-*`, `*` | Solo le applicazioni `untenanted` |
| `payments/shop`, `payments/*` | Le applicazioni di `payments` |
| `*/shop` | Ogni `shop`, di qualunque tenant |
| `*/*` | Ogni applicazione |

Un binding largo scritto per le applicazioni senza tenant (`applications: ["*"]`) non raggiunge quindi mai l'applicazione di un tenant. Senza registro tutto è `untenanted`, quindi `"*"` continua a coprire ogni applicazione.

```yaml title="manent.yml"
rbac:
  bindings:
    - {groups: [platform], role: admin, applications: ["*/*"], environments: ["*"]}
    - {groups: [retail-ci], role: deployer, applications: ["retail/*"], environments: ["*"]}
governance:
  rules:
    - {name: critiche, applications: ["*/*"], environments: [prod], selector: "criticality=high", approvals: {required: 2}}
  freezes:
    - {name: chiusura-retail, applications: ["retail/*"], environments: [prod], from: 2026-12-20T00:00:00Z, to: 2027-01-07T00:00:00Z}
```

Se un deployer di `payments` indica per errore `tenant: retail`, il deploy è rifiutato con 403, salvo un permesso esplicito sull'applicazione di `retail`: i ruoli impliciti coprono solo il proprio tenant, e i pattern senza prefisso solo le applicazioni `untenanted`.

## Selettori sui tag

I selettori sulle applicazioni leggono i tag dell'applicazione, `application`, `tenant` e i tag del tenant con il prefisso `tenant.`: `criticality=high`, `tenant=payments`, `tenant=untenanted`, `tenant.division in (commerce)`. I selettori sui tenant (`tenants`) leggono i tag del tenant e `name`.

| Dove | Campo |
|---|---|
| Binding RBAC e grant della policy dei permessi | `tenants` (proprietario) e `selector` (applicazioni) |
| Regole di governance e freeze | `tenants` e `selector`; in Rego `input.tenant` e `input.application_tags` |
| Elenchi | `manent inventory --selector …`, `manent list --selector …`, query `selector` dell'API |
| Freeze a runtime | `manent freeze add … --selector criticality=high` |

```yaml title="manent.yml"
rbac:
  selector_keys: [criticality, product, tenant]   # chiavi che i selettori dei permessi possono leggere
  bindings:
    - {groups: [sre], role: approver, applications: ["*/*"], environments: [prod], selector: "criticality=high"}
```

I selettori dei permessi possono leggere solo le chiavi di `rbac.selector_keys` (vuoto: nessun selettore nei permessi): un tag cosmetico non deve dare accessi.

## Membri, amministratori e visibilità

```yaml title="manent.yml"
rbac:
  tenant_roles: {members: deployer, admins: admin}   # default; un ruolo vuoto non dà nulla
  cross_tenant_inventory: true                       # default
```

- **Membri e amministratori** di un tenant ricevono il ruolo di `tenant_roles` sulle applicazioni del tenant (`<tenant>/*`), in ogni ambiente. I ruoli impliciti non danno mai `policy.*`, decisi solo da `rbac.bindings`, e non coprono mai le applicazioni `untenanted`.
- **I collaboratori** di altri team ricevono binding espliciti (`applications: ["payments/*"]` o `tenants: "name=payments"`).
- **Tra tenant** chi appartiene a un tenant può leggere l'inventario degli altri (`cross_tenant_inventory`); tutto il resto richiede un permesso.
- **I ruoli di piattaforma** (`platform.read`, `policy.*`, break-glass) restano trasversali.

## Isolamento tra tenant

`untenanted` conta come un tenant a sé.

- **ID dei deploy:** un ID già usato da un altro tenant è rifiutato (`deployment id … is already in use`), senza dire di chi è.
- **Dipendenze:** `after` verso un deploy di un altro tenant è rifiutato, salvo che il deploy sia il produttore di un import; tra tenant gli import leggono solo gli export con `public: true`.
- **Release:** una release composta contiene le applicazioni di un solo tenant.
- **Restrizioni:** `secret_refs`, `artifact_sources` e `callback_hosts` del tenant sono verificati all'invio e di nuovo dallo step `config` sul descrittore effettivo (le restrizioni sono registrate nella richiesta).

```yaml title="export di un deploy"
exports:
  vpc_id: {from: deploy.vpc_id, pattern: 'vpc-[0-9a-f]+', public: true}   # leggibile da altri tenant
  admin_host: {from: deploy.admin, pattern: '[a-z0-9.-]+'}                # solo i deploy del proprietario
```

## Spostare un'applicazione in un altro tenant

Quando un'applicazione `untenanted` entra in un tenant, o passa da un tenant a un altro, la sua identità cambia. Per conservare storico, inventario e rollback, sposta il suo storico dopo aver aggiornato il registro:

```sh
manent-server -c manent.yml admin move-application -a legacy --from untenanted --to retail [--dry-run]
```

Cambia l'attributo `tenant` dei deploy conclusi (e delle loro approvazioni e attese); i workflow non conclusi sono elencati in `pending`. Sposta anche la configurazione di ambiente (per esempio da `untenanted/legacy/…` a `retail/legacy/…`). `admin rename-artifact` accetta `--tenant` per le applicazioni di un tenant.

## Code e worker per tenant

Un tenant con `queue` esegue i suoi deploy su quella coda; gli altri, `untenanted` compreso, usano la coda di default (`dbos.queue`). Ogni worker registra all'avvio le code del registro e, di default, serve qualunque tenant. Un pool dedicato ad alcuni team, con le loro credenziali nel proprio `manent.yml`, ascolta solo le loro code:

```yaml title="manent.yml dei worker di payments"
dbos:
  tenants: [payments]      # solo deployments.payments (la coda di default per i tenant che non ne hanno)
plugins:
  ecs: {role_arn: "arn:aws:iam::111111111111:role/payments-deployer"}
```

- `concurrency` del tenant è il limite globale della sua coda (quota); `environment_concurrency` vale comunque per release (tenant, applicazione, artefatto, ambiente).
- Con il routing per region i passi di un deploy del tenant girano su `<coda>.<region>`.
- Una coda aggiunta al registro viene registrata dai worker al loro avvio successivo: fino ad allora i suoi deploy restano in coda.
- Approvazioni, attese delle dipendenze e release restano sulla coda condivisa delle approvazioni: non eseguono codice dei plugin.
