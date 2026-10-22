# Tenant

Un **tenant** è un team interno che possiede un insieme di applicazioni. Ogni applicazione appartiene a un solo tenant, che la elenca nel registro con i suoi **tag** (prodotto, criticità, …). Il tenant è lo scope di permessi, code e restrizioni; i tag raggruppano e selezionano le applicazioni attraverso i team. Non c'è gerarchia: un proprietario per applicazione, tag liberi su tenant e applicazioni.

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
  platform:
    applications:
      infra: {}
    disabled: false                          # un tenant disabilitato non può rilasciare
```

```yaml title="manent.yml"
tenants:
  file: /etc/manent/tenants.yml
  cache: 30s           # per quanto si tiene il registro prima di rileggerlo
```

- **Proprietà:** un'applicazione elencata da due tenant rende non valido l'intero registro.
- **Solo applicazioni registrate:** con un registro configurato, il deploy di un'applicazione che nessun tenant elenca è rifiutato. Senza registro ogni applicazione è ammessa, senza proprietario.
- **Tag solo nel registro:** il descrittore non può indicare tenant né tag, altrimenti un deploy potrebbe mettersi nel perimetro di un altro team.
- **Nomi:** i tenant usano lettere minuscole, cifre e `-` (al più 40); le applicazioni seguono la regola di `artifact.name`. Nei tag delle applicazioni sono riservati `application`, `tenant` e il prefisso `tenant.`, in quelli dei tenant `name`.
- **Revisione:** lo step `config` registra il tenant del deploy; la sorgente del registro e la sua revisione (un prefisso dello sha256 del file) sono in `manent tenants show`.

```sh
manent tenants list [-a <applicazione>] [--selector "division=commerce"]
manent tenants show payments        # tag, applicazioni con i loro tag, coda
```

## Il tenant è necessario per rilasciare?

Dipende dalla configurazione del server:

| Configurazione | Deploy di un'applicazione |
|---|---|
| Nessun registro (`tenants.file` vuoto, il default) | Ammesso per ogni applicazione, che non ha proprietario. Valgono solo `rbac.bindings` e le regole di governance. |
| Registro configurato | Ammesso solo se un tenant elenca l'applicazione. Altrimenti è rifiutato all'invio, prima dell'accodamento: `application <nome> is not registered in any tenant`. |
| Registro configurato, tenant proprietario con `disabled: true` | Rifiutato: `tenant <nome>, owner of <applicazione>, is disabled`. |

Per aggiungere una nuova applicazione con un registro:

1. Inseriscila sotto il suo tenant in `tenants.yml`, con i suoi tag.
2. Attendi al più `tenants.cache` (default `30s`) che API e worker rileggano il registro.
3. Rilascia: il proprietario viene registrato in automatico, il descrittore non lo indica mai.

Un'applicazione elencata da due tenant rende non valido l'intero registro. Tieni il registro sotto controllo di versione e rivedilo come il resto della configurazione.

## Il tenant di un deploy

Il tenant non si sceglie: viene dall'applicazione. All'invio l'engine legge il proprietario dal registro e lo registra nella richiesta (attributo `tenant`, `Status.tenant`, campo `tenant` di callback e messaggi chat, `tenant` nel risultato dello step `config`). L'identità del deploy resta applicazione/artefatto/ambiente.

```sh
manent list -t payments              # deploy delle applicazioni di payments
manent inventory -t payments
manent dora -t payments --by-tenant
```

## Selettori sui tag

I selettori sulle applicazioni leggono i tag dell'applicazione, `application`, `tenant` e i tag del tenant con il prefisso `tenant.`: `criticality=high`, `tenant=payments`, `tenant.division in (commerce)`. I selettori sui tenant (`tenants`) leggono i tag del tenant e `name`.

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
    - {groups: [sre], role: approver, applications: ["*"], environments: [prod], selector: "criticality=high"}
governance:
  rules:
    - {name: critiche, applications: ["*"], environments: [prod], selector: "criticality=high", approvals: {required: 2}}
```

I selettori dei permessi possono leggere solo le chiavi di `rbac.selector_keys` (vuoto: nessun selettore nei permessi): un tag cosmetico non deve dare accessi.

## Membri, amministratori e visibilità

```yaml title="manent.yml"
rbac:
  tenant_roles: {members: deployer, admins: admin}   # default; un ruolo vuoto non dà nulla
  cross_tenant_inventory: true                       # default
```

- **Membri e amministratori** di un tenant ricevono il ruolo di `tenant_roles` sulle applicazioni del tenant, in ogni ambiente. I ruoli impliciti non danno mai `policy.*`, decisi solo da `rbac.bindings`.
- **I collaboratori** di altri team ricevono binding espliciti (`tenants: "name=payments"`).
- **Tra tenant** chi appartiene a un tenant può leggere l'inventario degli altri (`cross_tenant_inventory`); tutto il resto richiede un permesso.
- **I ruoli di piattaforma** (`platform.read`, `policy.*`, break-glass) restano trasversali.

## Isolamento tra tenant

- **ID dei deploy:** un ID già usato da un altro tenant è rifiutato (`deployment id … is already in use`), senza dire di chi è.
- **Dipendenze:** `after` verso un deploy di un altro tenant è rifiutato, salvo che il deploy sia il produttore di un import; tra tenant gli import leggono solo gli export con `public: true`.
- **Release:** una release composta contiene le applicazioni di un solo tenant.
- **Restrizioni:** `secret_refs`, `artifact_sources` e `callback_hosts` del tenant sono verificati all'invio e di nuovo dallo step `config` sul descrittore effettivo (le restrizioni sono registrate nella richiesta).

```yaml title="export di un deploy"
exports:
  vpc_id: {from: deploy.vpc_id, pattern: 'vpc-[0-9a-f]+', public: true}   # leggibile da altri tenant
  admin_host: {from: deploy.admin, pattern: '[a-z0-9.-]+'}                # solo i deploy del proprietario
```

## Code e worker per tenant

Un tenant con `queue` esegue i suoi deploy su quella coda; gli altri usano la coda di default (`dbos.queue`). Ogni worker registra all'avvio le code del registro e, di default, serve qualunque tenant. Un pool dedicato ad alcuni team, con le loro credenziali nel proprio `manent.yml`, ascolta solo le loro code:

```yaml title="manent.yml dei worker di payments"
dbos:
  tenants: [payments]      # solo deployments.payments (la coda di default per i tenant che non ne hanno)
plugins:
  ecs: {role_arn: "arn:aws:iam::111111111111:role/payments-deployer"}
```

- `concurrency` del tenant è il limite globale della sua coda (quota); `environment_concurrency` vale comunque per artefatto e ambiente.
- Con il routing per region i passi di un deploy del tenant girano su `<coda>.<region>`.
- Una coda aggiunta al registro viene registrata dai worker al loro avvio successivo: fino ad allora i suoi deploy restano in coda.
- Approvazioni, attese delle dipendenze e release restano sulla coda condivisa delle approvazioni: non eseguono codice dei plugin.
