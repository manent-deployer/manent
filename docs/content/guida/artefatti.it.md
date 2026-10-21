# Applicazioni con più artefatti

Un'applicazione è spesso fatta di più artefatti rilasciati separatamente: un frontend, un backend, le migrazioni del database. Ogni artefatto è una release a sé: configurazione di ambiente, coda, inventario, rollback, promozione e retention sono per applicazione, artefatto e ambiente. L'artefatto è `artifact.name`, obbligatorio in ogni descrittore.

```yaml
# backend
application: shop
environment: prod
artifact: {type: war, name: shop-api, version: 1.5.0, source: https://repo.example.com/shop-api-1.5.0.war}
---
# frontend
application: shop
environment: prod
artifact: {type: tgz, name: shop-web, version: 3.2.0, source: https://repo.example.com/shop-web-3.2.0.tgz}
```

| | Per applicazione, artefatto e ambiente |
|---|---|
| Release in esercizio (inventario) | una voce per artefatto |
| Partizione della coda (`environment_concurrency`) | `<app>+<artifact>/<env>`: artefatti diversi si rilasciano in parallelo |
| Rollback, promozione, `requires` | dell'artefatto |
| Retention `keep_releases` | per artefatto |
| Nome di default delle schedule | `<app>-<artifact>-<env>` |

`artifact.name` accetta lettere, cifre, `.`, `_`, `:` e `-` (al più 128 caratteri, iniziale alfanumerica): entra nelle partizioni della coda, nelle chiavi e nei percorsi. Il percorso di un'immagine va in `source`. Un'applicazione con un solo artefatto di solito gli dà il nome dell'applicazione.

L'ordine tra artefatti (migrazioni, poi backend, poi frontend) si esprime con [`after` o con una release composta](catene.md).

## Configurazione di ambiente per artefatto

I template dei provider contengono `{artifact}`: un documento per artefatto, senza nulla di condiviso (target e segreti comuni si ripetono in ogni documento).

```yaml title="manent.yml"
envconfig:
  provider: file
  required: true                                   # un documento mancante è un errore: intercetta un artefatto scritto male
  file:
    path: "envconfig/{application}/{artifact}/{environment}.yml"
  # consul:
  #   key: "manent/{application}/{artifact}/{environment}"
```

```text
envconfig/
  shop/
    shop-api/prod.yml
    shop-web/prod.yml
    shop-db/prod.yml
```

- **Nomi:** un `artifact.name` che non può stare in un percorso o in una chiave (es. `team/api`) è rifiutato.
- **Un template senza `{artifact}`** legge un documento per applicazione/ambiente, condiviso da tutti i suoi artefatti.
- **`descriptor_policy.secret_refs`** accetta `{artifact}`: `vault:secret/{application}/{artifact}/{environment}/*`.
- **`manent env validate --artifact <nome>`** verifica un documento candidato di un artefatto.

## Rollback, promozione e inventario

```sh
manent inventory -a shop                              # una voce per artefatto
manent rollback -a shop -e prod --artifact shop-api   # -A in breve
manent promote -a shop --from staging --to prod -A shop-web
manent list -a shop -A shop-db
manent dora -a shop -A shop-api
```

Con un solo artefatto nell'ambiente `--artifact` si può omettere; con più di uno la richiesta è rifiutata e l'errore li elenca. In una release composta con `onFailure: rollback` ogni deploy viene riportato indietro sul proprio artefatto. Gli import della release in esercizio (`imports.<nome>.current`) accettano anche `artifact`, obbligatorio se l'applicazione ha più artefatti in quell'ambiente.

## Permessi e governance per artefatto

Binding, regole e freeze accettano un elenco opzionale di pattern su `artifact.name`:

```yaml title="manent.yml"
rbac:
  bindings:
    - groups: [web-team]
      role: deployer
      applications: [shop]
      environments: ["*"]
      artifacts: ["shop-web*"]       # solo i deploy di questi artefatti
governance:
  rules:
    - name: migrazioni
      applications: ["*"]
      environments: [prod]
      artifacts: ["*-db"]
      approvals: {required: 2}
  freezes:
    - name: freeze-frontend
      applications: [shop]
      environments: [prod]
      artifacts: [shop-web]
      from: 2026-12-20T00:00:00Z
      to: 2027-01-07T00:00:00Z
```

- **I binding con `artifacts`** valgono solo per i deploy di quegli artefatti, mai per le azioni sull'intero ambiente (freeze, elenco delle schedule, tenant): per quelle serve un binding senza `artifacts`. Lo stesso campo è accettato nei grant di una [policy dei permessi](permessi.md) (`data.manent.rbac.bindings`, `grants`).
- **Le regole con `artifacts`** valgono solo per quegli artefatti; in Rego la libreria legge `input.artifact.name`.
- **I freeze con `artifacts`** bloccano solo quegli artefatti; dove l'artefatto non è noto (es. gli step personalizzati che controllano l'ambiente) un freeze parziale vale comunque. `manent freeze add -A shop-web` ne crea uno a runtime.

## Tenant

Parametri, segreti e parametri degli step personalizzati di un [tenant](tenant.md) si possono dare per un solo artefatto con la chiave `<applicazione>/<artefatto>`; hanno priorità su quelli dell'applicazione:

```yaml title="envconfig/tenants/acme.yml"
params:
  shop: {db_host: acme-db.internal}
  shop/shop-db: {db_host: acme-db-admin.internal}
```

## Rinominare un artefatto

Un `artifact.name` nuovo è una release nuova, senza storia. Per conservarla si rinomina l'artefatto nello storico:

```sh
manent-server -c manent.yml admin rename-artifact -a shop --from api --to shop-api [--dry-run]
```

Il comando cambia l'attributo `artifact` dei deploy conclusi (e delle loro approvazioni e attese): inventario, rollback e promozioni seguono il nuovo nome, e un rollback rilascia la versione vecchia con il nuovo nome. I workflow non conclusi sono elencati in `pending` e lasciati invariati: va rilanciato quando terminano. I processi dell'API ricostruiscono l'inventario entro 10 minuti. Le schedule conservano il descrittore con cui sono state create.
