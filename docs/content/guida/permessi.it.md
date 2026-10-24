# Permessi come policy

Di default i permessi sono i `rbac.bindings` di `manent.yml` ([Permessi](../operazioni/accesso.md#permessi-rbac)). Con `rbac.engine: opa` li decidono le stesse revisioni Rego della [policy di governance](policy.md): caricate come bozza, provate, approvate da una seconda persona, attivate senza riavviare i server e registrate nell'audit.

Una policy può esprimere ciò che i binding non sanno dire:

- **accessi temporanei**: un permesso di reperibilità che smette di valere a fine turno (`until`);
- **proprietà**: un team riceve le applicazioni dei tenant con il tag `owner=<team>`, senza un binding per applicazione;
- **condizioni sulla richiesta**: i contractor non cambiano mai la produzione, le migrazioni in produzione solo dal gruppo DBA, un deploy annullato solo da chi l'ha chiesto.

```yaml title="manent.yml"
governance:
  engine: opa                 # obbligatorio: le due policy condividono revisioni e ciclo di vita
auth:
  oidc:
    policy_claims: [email, name, employee_type]   # claim visibili alla policy (default [email, name])
rbac:
  engine: opa                 # builtin (default) | opa
  opa:
    cache: 30s                # per quanto si riusano i permessi di un'identità
    cache_size: 10000         # identità in cache
  bindings:                   # policy.* sempre da qui
    - {groups: [platform-release], role: admin, applications: ["*"], environments: ["*"]}
```

Con `rbac.engine: opa` e nessun Rego scritto non cambia nulla: una revisione senza `package manent.authz` riceve un modulo predefinito che applica i `rbac.bindings` esattamente come il motore `builtin` (un test verifica l'equivalenza).

## Due parti: permessi e divieti

```text
data.manent.authz.grants  →  i permessi di un'identità (array o insieme di oggetti)
data.manent.authz.deny    →  motivi che rifiutano una modifica (stringhe o {code, message})
```

- **I permessi** si calcolano una volta per identità e restano in cache per `rbac.opa.cache`: letture, elenchi, interfaccia web e `GET /me` non valutano mai Rego.
- **I divieti** si valutano solo per le azioni che modificano qualcosa, dopo che i permessi le hanno consentite: `deployment.submit`, `deployment.dry-run`, `deployment.cancel`, `deployment.rollback`, `deployment.approve`, `deployment.freeze-override`, `schedule.write`, `freeze.write`.

### Permessi

Input:

```json
{
  "schema": 1,
  "now": "2026-10-20T09:00:00Z",
  "identity": {
    "subject": "oidc:alice", "method": "oidc", "name": "alice",
    "groups": ["team-shop", "oncall"],
    "claims": {"email": "alice@example.com", "employee_type": "contractor"}
  }
}
```

`claims` contiene solo i claim elencati in `auth.oidc.policy_claims`: i token possono contenere altri dati personali che la policy non deve vedere. I token statici hanno `method: token`, i loro gruppi e nessun claim.

Output, un oggetto per permesso:

| Campo | Note |
|---|---|
| `name` | Etichetta mostrata da `manent whoami`, nell'interfaccia web e nell'audit. |
| `role` o `actions` | Un ruolo (predefinito o di `rbac.roles`/`data.manent.rbac.roles`) oppure un elenco di azioni. Le azioni `policy.*` sono **scartate** con un avviso: le concede solo `rbac.bindings`. |
| `applications` | Pattern qualificati con `*`: `shop` solo untenanted, `payments/*` le applicazioni di un tenant, `*/*` tutte ([dettagli](tenant.md#pattern-delle-applicazioni-qualificati)). Obbligatorio. |
| `environments` | Pattern con `*`, obbligatori. |
| `tenants` | Selettore sui tag e sul nome del [tenant](tenant.md) proprietario; assente: tutte le applicazioni. |
| `artifacts` | Pattern su `artifact.name`; assente: tutti gli artefatti. |
| `selector` | Selettore sui [tag delle applicazioni](tenant.md), con le chiavi di `rbac.selector_keys`; assente: tutte le applicazioni. |
| `until` | Facoltativo, RFC 3339: il permesso smette di valere a quell'istante, anche se è in cache. L'istante si confronta con l'orologio del server API. |

Un permesso non valido (ruolo o azione sconosciuti, pattern mancanti, selettore non valido) viene scartato da solo, con un avviso nel log e in `manent_authz_invalid_grants_total`; alla compilazione, sugli input di esempio, è un errore.

### Divieti

Input:

```json
{
  "schema": 1,
  "now": "2026-10-20T09:00:00Z",
  "identity": {"subject": "oidc:alice", "method": "oidc", "name": "alice", "groups": ["team-shop"], "claims": {}},
  "action": "deployment.submit",
  "resource": {
    "application": "shop",
    "environment": "prod",
    "tenant": {"name": "payments", "tags": {"division": "commerce", "owner": "team-shop"}},
    "deployment": {
      "id": "shop-1.4.0-prod", "kind": "deploy", "dry_run": false,
      "artifact": {"type": "application", "name": "shop", "version": "1.4.0"},
      "requested_by": "oidc:alice"
    }
  }
}
```

`resource.deployment` è la richiesta per un invio e il deploy esistente per annullamento, rollback e approvazione; manca per le azioni senza deploy (schedule, freeze). L'input non contiene mai segreti né parametri dei plugin.

Un divieto prevale su ogni permesso. La risposta `403` e la voce di audit riportano i motivi e la revisione:

```text
permission denied: oidc:alice cannot perform deployment.submit on shop/prod: contractors cannot change production (policy 14:3f2a9c1e5b7d)
```

## Esempio

```rego title="authz.rego"
package manent.authz

import rego.v1

import data.manent.lib.authz

# I binding di manent.yml (data.manent.rbac), più le regole sotto.
grants := authz.bindings | owners | oncall

# Un team possiede le applicazioni dei tenant con il suo nome nel tag owner.
owners contains g if {
	some group in input.identity.groups
	startswith(group, "team-")
	g := {
		"name": sprintf("owner %s", [group]),
		"role": "deployer",
		"applications": ["*/*"],
		"environments": ["*"],
		"tenants": sprintf("owner=%s", [group]),
	}
}

# Reperibilità in produzione fino a fine turno (oncall/data.json).
oncall contains g if {
	some shift in data.oncall.shifts
	shift.subject == input.identity.subject
	g := {"name": "oncall", "role": "operator", "applications": ["*/*"], "environments": ["prod"], "until": shift.until}
}

# I contractor non cambiano mai la produzione.
deny contains "contractors cannot change production" if {
	input.identity.claims.employee_type == "contractor"
	startswith(input.resource.environment, "prod")
}

# Migrazioni del database in produzione: solo i DBA.
deny contains {"code": "dba", "message": "migrations in production are released by the DBA group"} if {
	input.action == "deployment.submit"
	input.resource.deployment.artifact.type == "migration"
	startswith(input.resource.environment, "prod")
	not "dba" in input.identity.groups
}
```

`data.manent.lib.authz` fornisce `bindings` (i permessi di `data.manent.rbac.bindings` per l'identità dell'input) e `binding_matches(binding, identity)`. Le restrizioni sono quelle della policy di governance: Rego v1, nessun built-in non deterministico, niente `http.send`. I test `*_test.rego` coprono anche `manent.authz`. I motivi dei divieti sono in inglese, come gli altri messaggi di errore.

### Partire dai binding

```sh
manent-server -c manent.yml policy export-rbac -d policy/
manent-server -c manent.yml policy test -d policy/
manent policy push -d policy/ -m "permessi da manent.yml"
```

`export-rbac` scrive `manent/rbac/data.json` (i binding come `data.manent.rbac`, che da quel momento non arriva più da `manent.yml`) e un `authz.rego` di partenza con esempi commentati. I file esistenti non vengono sovrascritti senza `--force`.

## Separazione dei poteri

| Azione | Chi decide |
|---|---|
| `policy.read`, `policy.write`, `policy.approve` | Solo `rbac.bindings` di `manent.yml`, qualunque sia il motore. |
| `manent-server admin …` (break-glass) | Chi ha accesso al database, fuori dai permessi. |
| Tutte le altre azioni | La revisione attiva. |

Una revisione non può concedersi il potere di cambiare la policy, e una revisione che toglie tutti i permessi non blocca chi deve correggerla: chi ha `policy.*` statico può ancora caricare, approvare e attivare una revisione nuova, o tornare a una precedente. La configurazione minima è quindi un binding statico per i release manager della piattaforma.

## Simulazione e impatto

Prima di approvare una revisione, simula le identità che riguarda. Nulla viene letto dal provider di identità né salvato: soggetto, gruppi e claim sono quelli che scrivi.

```sh
manent policy access 15 --subject oidc:carol --group team-shop --claim employee_type=contractor --compare
manent policy access 15 --subject oidc:carol --claim employee_type=contractor \
  --action deployment.submit -a shop -e prod [-t acme] [-f deploy.yml]
```

```text
Identity oidc:carol, revision 15:9be1c04d2f11

PERMISSION        ACTIONS   APPLICATIONS  ENVIRONMENTS  TENANT          UNTIL
owner team-shop   deployer  *             *             owner=team-shop -

Compared with the active revision 14:3f2a9c1e5b7d:
+ owner team-shop deployer  *             *             owner=team-shop -

Outcome: denied by the policy (permission owner team-shop)
  - contractors cannot change production
```

`current` è la revisione in vigore; `--json` stampa la risposta di `POST /api/v1/policies/{revision}/access`. La stessa simulazione è nella sezione **Accessi** della pagina Policy dell'[interfaccia web](interfaccia-web.md). Richiede `policy.write`.

## Cache e disponibilità

| Situazione | Effetto |
|---|---|
| Nuova revisione attivata | Vale subito: la cache è per revisione. |
| Permesso tolto dal provider di identità (gruppo, claim) | Vale entro `rbac.opa.cache` (default 30s). |
| `until` raggiunto | Vale subito, anche con il permesso in cache. |
| Nessuna revisione caricata, o più vecchia di `governance.opa.max_staleness` | Ogni azione non statica è rifiutata con `503`; le `policy.*` continuano a funzionare, così si può caricare una revisione. |
| Errore nel calcolo dei permessi | Nessun permesso per quell'identità: tutto negato, errore nel log. |
| Errore nella valutazione dei divieti | La modifica è rifiutata (`403`, `policy evaluation failed`). |

Per tornare a `rbac.engine: builtin` basta un riavvio: i binding di `manent.yml` sono sempre lì.

## Audit e metriche

Le modifiche consentite riportano nell'[audit](../operazioni/accesso.md#audit) `authz_revision` e il `grant` che le ha concesse; quelle negate la revisione e i motivi. `manent whoami` e `GET /api/v1/me` mostrano `authz_engine`, `authz_revision` e i permessi con `name` e `until`.

| Metrica | Descrizione |
|---|---|
| `manent_authz_grants_evaluations_total{outcome}` | Permessi calcolati (`ok`, `cached`, `error`). |
| `manent_authz_deny_evaluations_total{action,outcome}` | Divieti valutati (`allowed`, `denied`, `error`). |
| `manent_authz_eval_duration_seconds{part}` | Durata delle valutazioni (`grants`, `deny`). |
| `manent_authz_invalid_grants_total` | Permessi scartati perché non validi o con azioni `policy.*`. |
| `manent_authz_cache_entries` | Identità in cache. |
