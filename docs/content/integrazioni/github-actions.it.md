# GitHub Actions

Il repository contiene un'azione composita, `manent-deployer/manent/integrations/github`, che rilascia con manent, valida un descrittore o verifica una configurazione di ambiente candidata. Installa il client se il runner non lo ha, attende l'esito, fa fallire il job se il deploy fallisce e pubblica il risultato come output dello step e nel riepilogo del job.

!!! abstract "Cosa serve"
    Un server manent raggiungibile dai runner (i runner ospitati da GitHub richiedono un indirizzo pubblico; altrimenti usa runner self-hosted nella tua rete) · un token per la CI ([token statico](../operazioni/accesso.md) in `auth.tokens_file`)

## 1. Token e permessi

```sh
TOKEN=$(openssl rand -hex 32)
printf '%s' "$TOKEN" | sha256sum         # nel file dei token di manent
gh secret set MANENT_TOKEN --body "$TOKEN"
```

```yaml title="/etc/manent/tokens.yml"
tokens:
  - {name: gh-shop, sha256: "<sha256>", groups: [ci], expires_at: 2027-06-30T00:00:00Z}
```

```yaml title="manent.yml"
rbac:
  bindings:
    - {subjects: ["token:gh-shop"], role: deployer, applications: [shop], environments: [test, staging]}
    - {subjects: ["token:gh-shop"], role: tester, applications: [shop], environments: [prod]}   # solo dry-run
```

Un token per repository mantiene leggibile l'audit (`requested_by: token:gh-shop`) e limita ciò che può fare un token trafugato.

## 2. Deploy a ogni tag

```yaml title=".github/workflows/release.yml"
on:
  push:
    tags: ["v*"]

jobs:
  deploy-test:
    runs-on: ubuntu-latest
    environment: test
    steps:
      - uses: actions/checkout@v4
      - id: manent
        uses: manent-deployer/manent/integrations/github@v0.1.0
        with:
          server: https://manent.example.com
          token: ${{ secrets.MANENT_TOKEN }}
          descriptor: deploy/test.yml
          version: ${{ github.ref_name }}
          source: https://nexus.example.com/repository/releases/shop/shop-${{ github.ref_name }}.tgz
          id: gh-${{ github.run_id }}
          timeout: 30m
      - run: echo "deploy ${{ steps.manent.outputs.deployment-id }} terminato ${{ steps.manent.outputs.state }}"
```

| Input | Default | Significato |
|---|---|---|
| `server`, `token` | | Indirizzo e token di manent (obbligatori). |
| `command` | `deploy` | `deploy`, `validate` (descrittore) o `env-validate` (configurazione di ambiente candidata). |
| `descriptor` | | Descrittore YAML nel repository. Per `env-validate` è il descrittore di esempio, facoltativo. |
| `version`, `source`, `sha256` | | Sostituiscono i campi dell'artefatto del descrittore. |
| `id` | | ID del deploy, la chiave di idempotenza. |
| `dry-run` | `false` | Esegue un [dry-run](../guida/dry-run.md) su un worker. |
| `wait`, `timeout` | `true`, `30m` | Attende la fine (approvazioni comprese) fino a `timeout`; dopo il timeout il deploy prosegue. |
| `after` | | ID di deploy che devono riuscire prima, separati da spazi. |
| `application`, `environment`, `env-config` | | Per `env-validate`. |
| `ca-file` | | CA del server, se privata. |

Output: `deployment-id`, `state` e `version`.

!!! tip "Scegliere l'`id`"
    `gh-${{ github.run_id }}` fa sì che **Re-run jobs** restituisca il deploy già fatto invece di rilasciare di nuovo. Usa `gh-${{ github.run_id }}-${{ github.run_attempt }}` quando una riesecuzione deve rilasciare di nuovo.

## 3. Dry-run sulle pull request

```yaml title=".github/workflows/pr.yml"
on: pull_request

jobs:
  manent-dry-run:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: manent-deployer/manent/integrations/github@v0.1.0
        with:
          server: https://manent.example.com
          token: ${{ secrets.MANENT_TOKEN }}
          descriptor: deploy/prod.yml
          dry-run: true
```

Un worker risolve i segreti, scarica e verifica l'artefatto e controlla i target di produzione senza modificarli. Con `command: validate` la verifica resta sul server dell'API e non tocca segreti, artefatto o target.

## 4. Promuovi in produzione

L'azione copre `deploy`, `validate` ed `env-validate`. Per una promozione installa il client e chiamalo direttamente:

```yaml title=".github/workflows/release.yml (seguito)"
  promote-prod:
    needs: deploy-test
    runs-on: ubuntu-latest
    environment: production                   # i reviewer di GitHub, se li usi, vengono prima
    steps:
      - uses: actions/setup-go@v5
        with: {go-version: "1.27"}
      - run: go install github.com/manent-deployer/manent/cmd/manent@v0.1.0
      - run: manent promote -a shop --from test --to prod --version "${{ github.ref_name }}" --timeout 4h
        env:
          MANENT_SERVER: https://manent.example.com
          MANENT_TOKEN: ${{ secrets.MANENT_PROMOTE_TOKEN }}
```

Il token deve avere `deployment.read` su `test` e `deployment.submit` su `prod`. Le [approvazioni](../guida/governance.md#approvazioni) di manent valgono comunque: il job attende, fino a `--timeout`, che una persona approvi in manent, in Slack o nell'interfaccia web.

## 5. Verifica le modifiche alla configurazione di ambiente

Nel repository che contiene la configurazione di ambiente:

```yaml title=".github/workflows/envconfig.yml"
on:
  pull_request:
    paths: ["envconfig/**"]

jobs:
  check:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: manent-deployer/manent/integrations/github@v0.1.0
        with:
          server: https://manent.example.com
          token: ${{ secrets.MANENT_TOKEN }}
          command: env-validate
          application: shop
          environment: prod
          env-config: envconfig/shop/shop/prod.yml
```

Il server fonde la candidata con la richiesta del deploy in esercizio ed esegue tutti i controlli dell'invio (fusione, policy sui descrittori, piano di rollout e di traffico, plugin). Non scrive nulla; il permesso necessario è `deployment.read`.

## CA privata e runner self-hosted

Con un certificato del server emesso da una CA aziendale, versiona o predisponi la CA sul runner e passala con `ca-file: /etc/ssl/certs/corp-ca.pem`. Sui runner self-hosted con il client già installato, l'azione usa quello e salta la preparazione di Go.

Riferimento: [CI e infrastruttura come codice](../guida/ci-iac.md#github-action).
