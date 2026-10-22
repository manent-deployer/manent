# CI e infrastruttura come codice

manent si usa da una pipeline senza scrivere chiamate all'API: il client ha le opzioni per la CI, il repository contiene una GitHub Action e un template GitLab, e un provider Terraform tiene la release in esercizio uguale alla configurazione.

| Strumento | Dove | Cosa fa |
|---|---|---|
| `manent deploy --ci` | client | Esiti come righe `chiave=valore` per GitHub o GitLab, riepilogo del job, versione e sorgente dalla pipeline. |
| `manent env validate` | client | Verifica una configurazione di ambiente candidata prima di applicarla. |
| GitHub Action | `integrations/github` | Installa il client e rilascia, valida un descrittore o una configurazione. |
| Template GitLab | `integrations/gitlab/manent.yml` | Job di deploy con report dotenv. |
| Provider Terraform | `integrations/terraform` | `manent_deployment`, `manent_inventory`, `manent_environment_check`, `manent_tenants`. |

## Il client nella CI

Il descrittore resta nel repository; la pipeline aggiunge ciò che conosce solo lei:

```sh
manent deploy -f deploy/prod.yml --version "$TAG" --source "https://nexus.example.com/shop-$TAG.tgz" --id "ci-$RUN_ID" --ci
```

| Opzione | Effetto |
|---|---|
| `--version`, `--source`, `--sha256` | Sostituiscono i campi dell'artefatto del descrittore. |
| `--id` | ID del deploy: con lo stesso ID un job rieseguito non rilascia di nuovo (idempotenza). |
| `--outputs <file>` | Aggiunge al file `deployment_id`, `state`, `application`, `environment`, `version`, `dry_run` come righe `chiave=valore`, anche se il deploy fallisce, e gli [export](catene.md#export-import) di un deploy riuscito come `export_<nome>=<valore>` (solo valori su una riga); `config_revision` con il [provider git](../integrazioni/git.md). |
| `--summary <file>` | Aggiunge un riepilogo in Markdown: versione, deploy, esito, rollout, rollback ed errore. |
| `--ci` | Su GitHub Actions (`GITHUB_ACTIONS=true`) esiti in `$GITHUB_OUTPUT` e riepilogo in `$GITHUB_STEP_SUMMARY`; su GitLab CI (`GITLAB_CI=true`) esiti in `manent.env` (o `$MANENT_DOTENV`). Fuori da una CI riconosciuta è un errore. |

Server e token arrivano da `MANENT_SERVER` e `MANENT_TOKEN` (un secret della CI, con un [token statico](../operazioni/accesso.md) del gruppo che può rilasciare). Il comando termina con un codice diverso da zero se il deploy non riesce, quindi il job fallisce; con un'[approvazione](governance.md#approvazioni) richiesta attende fino a `--timeout`.

### Configurazione di ambiente candidata { #env-validate }

```sh
manent env validate -a shop -e prod -f envconfig/shop/shop/prod.yml [-d deploy/prod.yml]
```

Il server fonde la configurazione con un descrittore di esempio (default: la richiesta del deploy in esercizio) e applica le verifiche dell'accodamento: campi sconosciuti, [policy sui descrittori](../operazioni/configurazione.md), fusione, rollback, piano delle [ondate](ondate.md) e del [traffico](traffico.md), plugin e controlli di salute. Non scrive nulla: è il controllo da eseguire nella merge request del repository delle configurazioni, prima che Consul o il file le ricevano. API: `POST /api/v1/environments/validate`, permesso `deployment.read`.

## GitHub Action

```yaml
jobs:
  deploy:
    runs-on: ubuntu-latest
    environment: production
    steps:
      - uses: actions/checkout@v4
      - id: manent
        uses: joeg-ita/manent/integrations/github@v0.1.0
        with:
          server: https://manent.example.com
          token: ${{ secrets.MANENT_TOKEN }}
          descriptor: deploy/prod.yml
          version: ${{ github.ref_name }}
          id: gh-${{ github.run_id }}-${{ github.run_attempt }}
      - run: echo "deploy ${{ steps.manent.outputs.deployment-id }}: ${{ steps.manent.outputs.state }}"
```

| Input | Default | Significato |
|---|---|---|
| `server`, `token` | | Server e token (obbligatori). |
| `command` | `deploy` | `deploy`, `validate` (descrittore) o `env-validate` (configurazione candidata). |
| `descriptor` | | Descrittore YAML; per `env-validate` è il descrittore di esempio, facoltativo. |
| `version`, `source`, `sha256`, `id` | | Come le opzioni del client. |
| `dry-run` | `false` | [Dry-run](dry-run.md) su un worker. |
| `wait`, `timeout` | `true`, `30m` | Attesa della conclusione. |
| `after` | | ID di deploy che devono riuscire prima, separati da spazi. |
| `application`, `environment`, `env-config` | | Per `env-validate`. |
| `ca-file` | | CA privata del server. |

Output: `deployment-id`, `state`, `version`; il riepilogo del job compare nella pagina del workflow. Se il client non è nel `PATH`, l'action lo compila dal proprio checkout (stessa versione dell'action) con `actions/setup-go`.

## Template GitLab

```yaml
include:
  - remote: https://raw.githubusercontent.com/joeg-ita/manent/v0.1.0/integrations/gitlab/manent.yml
    inputs:
      descriptor: deploy/prod.yml
      version: $CI_COMMIT_TAG
      id: gl-$CI_PIPELINE_ID
      manent_ref: v0.1.0
```

Gli input sono `job`, `stage`, `descriptor`, `version`, `source`, `sha256`, `id`, `dry_run`, `timeout`, `image` (default `golang:1.27-alpine`: il client viene installato con `go install` se non c'è) e `manent_ref`. `MANENT_SERVER` e `MANENT_TOKEN` sono variabili CI/CD del progetto (il token mascherato e protetto). Il report dotenv `manent.env` rende `deployment_id`, `state` e `version` variabili dei job successivi, anche quando il deploy fallisce.

## Provider Terraform

Il provider è in `integrations/terraform` (modulo Go separato: non aggiunge dipendenze a manent) e si compila con `make terraform-provider`. Finché non è pubblicato nel registry si usa con `dev_overrides` nel file di configurazione della CLI di Terraform:

```hcl
provider_installation {
  dev_overrides { "joeg-ita/manent" = "/opt/terraform/providers" }   # directory con terraform-provider-manent
  direct {}
}
```

```hcl
terraform {
  required_providers {
    manent = { source = "joeg-ita/manent" }
  }
}

provider "manent" {}   # server e token da MANENT_SERVER e MANENT_TOKEN (o server, token, token_file, ca_file)

resource "manent_deployment" "shop" {
  application          = "shop"
  environment          = "prod"
  descriptor           = file("deploy/prod.yml")
  version              = var.shop_version
  source               = "https://nexus.example.com/shop-${var.shop_version}.tgz"
  timeout              = "30m"
  redeploy_out_of_sync = true
}
```

### `manent_deployment`

La risorsa è la **release in esercizio** di un'applicazione in un ambiente.

- **Create e update** eseguono un deploy con il descrittore e gli attributi (`version` è obbligatoria, `source` e `sha256` facoltative) e ne attendono la fine entro `timeout`, approvazioni comprese. Ogni modifica è un deploy nuovo: l'`id` del descrittore viene ignorato. Un deploy non riuscito fa fallire l'apply; con il rollback automatico l'ambiente torna alla versione precedente e il plan successivo ritenta.
- **Read** legge l'[inventario](governance.md#inventario): se qualcuno ha rilasciato o fatto rollback di un'altra versione fuori da Terraform, il plan mostra la differenza (`"1.0.0" -> "1.1.0"`) e l'apply rilascia di nuovo quella configurata.
- Con `redeploy_out_of_sync = true`, un ambiente con target cambiati fuori da manent secondo la [riconciliazione](governance.md#stato-reale) (`out_of_sync`) compare nel plan come `"1.1.0 (target fuori sincronia)" -> "1.1.0"`, e l'apply lo rilascia di nuovo.
- **Destroy** non tocca i target: manent non rimuove le release, la risorsa esce solo dallo stato (con un avviso).
- **Import**: `terraform import manent_deployment.shop shop/prod`.
- Attributi calcolati: `id` (deploy in esercizio), `state`, `out_of_sync`.

### `manent_environment_check`

Verifica una configurazione di ambiente candidata come `manent env validate`: se non è valida il **plan fallisce**, prima che un'altra risorsa la scriva nel sistema di configurazione.

```hcl
data "manent_environment_check" "shop_prod" {
  application = "shop"
  environment = "prod"
  config      = local.shop_prod_config        # YAML o JSON
}

resource "consul_keys" "shop_prod" {
  key {
    path  = "manent/shop/shop/prod"
    value = data.manent_environment_check.shop_prod.valid ? local.shop_prod_config : null
  }
}
```

Attributi calcolati: `valid`, `plugin`, `targets` (host del deploy effettivo), `stages` (stadi del rollout) e `warnings`.

### `manent_inventory`

```hcl
data "manent_inventory" "prod" { environment = "prod" }
# data.manent_inventory.prod.entries[*]: application, artifact, environment, tenant, version, deployment_id, sha256, out_of_sync
```

### `manent_tenants`

`manent_tenants` (`application`, `selector`) restituisce `names` e `tenants` (nome, tag, applicazioni possedute): i team del [registro dei tenant](tenant.md).

!!! note "Cosa resta fuori"
    I permessi sono nella configurazione di manent (`manent.yml`), letta all'avvio, come le regole YAML di [governance](governance.md). Le policy Rego e i freeze a runtime invece si gestiscono con `manent policy` e `manent freeze`, oppure arrivano da un bundle o da una directory versionati altrove ([Policy in Rego](policy.md)). La configurazione di ambiente resta nel suo sistema (Consul, file nel repository): il provider la verifica, la scrivono `consul_keys` o il repository.
