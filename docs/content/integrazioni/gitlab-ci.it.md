# GitLab CI

Il repository contiene un template GitLab, `integrations/gitlab/manent.yml`, che si include con `inputs` tipizzati. Aggiunge un job di deploy che installa il client se l'immagine non lo ha, esegue `manent deploy --ci`, fallisce se il deploy fallisce e scrive l'esito in un **report dotenv**, così i job successivi ricevono `deployment_id`, `state` e `version` come variabili.

!!! abstract "Cosa serve"
    Una versione di GitLab che supporti `spec:inputs` nei file inclusi · un server manent raggiungibile dai runner · un token per la CI ([token statico](../operazioni/accesso.md) in `auth.tokens_file`)

## 1. Variabili

In **Settings → CI/CD → Variables** del progetto (o del gruppo):

| Variabile | Valore | Opzioni |
|---|---|---|
| `MANENT_SERVER` | `https://manent.example.com` | |
| `MANENT_TOKEN` | il token della CI | Masked, Protected |

Le variabili protette arrivano solo alle pipeline sui branch e sui tag protetti: una merge request da un fork non può leggere il token. Dai al token un ruolo limitato agli ambienti che la CI può toccare, come in [Deploy da una pipeline di CI](../tutorial/ci.md).

## 2. Includi il template

```yaml title=".gitlab-ci.yml"
stages: [build, deploy]

build:
  stage: build
  script:
    - make dist VERSION=$CI_COMMIT_TAG
    - 'curl -fsS -u "$NEXUS_USER:$NEXUS_PASSWORD" --upload-file dist/shop-$CI_COMMIT_TAG.tgz https://nexus.example.com/repository/releases/shop/'
  rules:
    - if: $CI_COMMIT_TAG

include:
  - remote: https://raw.githubusercontent.com/joeg-ita/manent/v0.1.0/integrations/gitlab/manent.yml
    inputs:
      job: deploy-test
      stage: deploy
      descriptor: deploy/test.yml
      version: $CI_COMMIT_TAG
      source: https://nexus.example.com/repository/releases/shop/shop-$CI_COMMIT_TAG.tgz
      id: gl-$CI_PIPELINE_ID
      timeout: 30m
      manent_ref: v0.1.0
```

| Input | Default | Significato |
|---|---|---|
| `job`, `stage` | `manent-deploy`, `deploy` | Nome e stage del job generato. |
| `descriptor` | | Descrittore YAML nel repository. |
| `version`, `source`, `sha256` | `""` | Sostituiscono i campi dell'artefatto del descrittore. |
| `id` | `""` | ID del deploy, la chiave di idempotenza: `gl-$CI_PIPELINE_ID` fa restituire a un job ritentato il deploy già fatto. |
| `dry_run` | `false` | Esegue un [dry-run](../guida/dry-run.md) su un worker. |
| `timeout` | `30m` | Attesa massima, approvazioni comprese. |
| `image` | `golang:1.27-alpine` | Immagine del job; se manca, il client viene installato con `go install`. |
| `manent_ref` | `main` | Versione del client da installare: fissala a un tag. |

Includi il template più volte, con nomi di `job` diversi, per più ambienti. Per aggiungere `rules`, `environment` o `needs` al job generato, estendilo per nome:

```yaml
deploy-test:
  environment: {name: test}
  rules:
    - if: $CI_COMMIT_TAG
```

!!! tip "Job più veloci"
    L'immagine predefinita compila il client a ogni esecuzione. Un'immagine tua che ha già `manent` nel `PATH` (e una shell, che serve a GitLab per eseguire lo script) evita l'installazione: passala come `image`. L'immagine ufficiale `ghcr.io/joeg-ita/manent` è distroless, senza shell, quindi non è adatta come immagine di un job GitLab.

## 3. Usa l'esito nei job successivi

```yaml
notify:
  stage: .post
  needs: [deploy-test]
  script:
    - echo "deploy $deployment_id della $version terminato $state"
  when: always
```

Il report dotenv viene scritto anche quando il deploy fallisce, così un job `when: always` può riportare l'errore.

## 4. Promuovi in produzione

```yaml
promote-prod:
  stage: deploy
  image: golang:1.27-alpine
  needs: [deploy-test]
  environment: {name: production}
  variables:
    MANENT_TOKEN: $MANENT_PROMOTE_TOKEN       # un token che può rilasciare in produzione
  script:
    - command -v manent >/dev/null || GOBIN=/usr/local/bin go install github.com/joeg-ita/manent/cmd/manent@v0.1.0
    - manent promote -a shop --from test --to prod --version "$CI_COMMIT_TAG" --timeout 4h
  rules:
    - if: $CI_COMMIT_TAG
      when: manual
```

`MANENT_PROMOTE_TOKEN` è una variabile protetta separata, con `deployment.read` su test e `deployment.submit` in produzione. Le [approvazioni](../guida/governance.md#approvazioni) di manent valgono comunque, oltre al job manuale di GitLab.

## 5. Verifica le modifiche alla configurazione di ambiente

Nella pipeline delle merge request del repository della configurazione:

```yaml
envconfig-check:
  image: golang:1.27-alpine
  rules:
    - if: $CI_PIPELINE_SOURCE == "merge_request_event"
      changes: ["envconfig/**/*"]
  script:
    - command -v manent >/dev/null || GOBIN=/usr/local/bin go install github.com/joeg-ita/manent/cmd/manent@v0.1.0
    - manent env validate -a shop -e prod -f envconfig/shop/shop/prod.yml
```

Riferimento: [CI e infrastruttura come codice](../guida/ci-iac.md).
