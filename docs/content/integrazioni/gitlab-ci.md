# GitLab CI

The repository ships a GitLab template, `integrations/gitlab/manent.yml`, included with typed `inputs`. It adds a deploy job that installs the client if the image does not have it, runs `manent deploy --ci`, fails when the deploy fails and writes the outcome to a **dotenv report**, so the following jobs receive `deployment_id`, `state` and `version` as variables.

!!! abstract "What you need"
    A GitLab version that supports `spec:inputs` in included files · a manent server reachable from the runners · a token for CI ([static token](../operazioni/accesso.md) in `auth.tokens_file`)

## 1. Variables

In **Settings → CI/CD → Variables** of the project (or of the group):

| Variable | Value | Flags |
|---|---|---|
| `MANENT_SERVER` | `https://manent.example.com` | |
| `MANENT_TOKEN` | the CI token | Masked, Protected |

Protected variables reach only pipelines on protected branches and tags: a merge request from a fork cannot read the token. Give the token a role limited to the environments CI may touch, as in [Deploy from a CI pipeline](../tutorial/ci.md).

## 2. Include the template

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

| Input | Default | Meaning |
|---|---|---|
| `job`, `stage` | `manent-deploy`, `deploy` | Name and stage of the generated job. |
| `descriptor` | | YAML descriptor in the repository. |
| `version`, `source`, `sha256` | `""` | Replace the artifact fields of the descriptor. |
| `id` | `""` | Deploy ID, the idempotency key: `gl-$CI_PIPELINE_ID` makes a retried job return the deploy already done. |
| `dry_run` | `false` | Run a [dry-run](../guida/dry-run.md) on a worker. |
| `timeout` | `30m` | Maximum wait, approvals included. |
| `image` | `golang:1.27-alpine` | Job image; the client is installed with `go install` if missing. |
| `manent_ref` | `main` | Version of the client to install: pin it to a tag. |

Include the template several times, with different `job` names, for several environments. To add `rules`, `environment` or `needs` to the generated job, extend it by name:

```yaml
deploy-test:
  environment: {name: test}
  rules:
    - if: $CI_COMMIT_TAG
```

!!! tip "Faster jobs"
    The default image compiles the client at every run. An image of yours that already has `manent` on the `PATH` (and a shell, which GitLab needs to run the script) skips the installation: pass it as `image`. The official `ghcr.io/joeg-ita/manent` image is distroless, without a shell, so it is not suitable as a GitLab job image.

## 3. Use the outcome in later jobs

```yaml
notify:
  stage: .post
  needs: [deploy-test]
  script:
    - echo "deploy $deployment_id of $version ended $state"
  when: always
```

The dotenv report is written even when the deploy fails, so a `when: always` job can report the failure.

## 4. Promote to production

```yaml
promote-prod:
  stage: deploy
  image: golang:1.27-alpine
  needs: [deploy-test]
  environment: {name: production}
  variables:
    MANENT_TOKEN: $MANENT_PROMOTE_TOKEN       # a token allowed to submit to production
  script:
    - command -v manent >/dev/null || GOBIN=/usr/local/bin go install github.com/joeg-ita/manent/cmd/manent@v0.1.0
    - manent promote -a shop --from test --to prod --version "$CI_COMMIT_TAG" --timeout 4h
  rules:
    - if: $CI_COMMIT_TAG
      when: manual
```

`MANENT_PROMOTE_TOKEN` is a separate protected variable, with `deployment.read` on test and `deployment.submit` on production. manent's [approvals](../guida/governance.md#approvazioni) still apply on top of GitLab's manual job.

## 5. Check environment configuration changes

In the merge request pipeline of the configuration repository:

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

Reference: [CI and infrastructure as code](../guida/ci-iac.md).
