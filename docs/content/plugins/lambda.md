# lambda

The `lambda` plugin releases the code of an AWS Lambda function. It updates the code (a container image or a `.zip` package), publishes an immutable version and moves an alias to it. The live release is the version the alias points to: the rollback moves the alias back, without uploading the code again. It uses the official AWS SDK.

The function (role, memory, environment variables, triggers, permissions) belongs to whoever manages the infrastructure (Terraform, CloudFormation, SAM): manent changes only the code. Whoever invokes the function, or its triggers, must use the **alias** (the qualified ARN `…:function:shop:live`), not `$LATEST`.

## The artifact

Two kinds of artifact, matching the function's package type:

- **image** (`PackageType: Image`): an `oci://` source, as for the [container](container.md) plugin. The digest becomes the artifact's sha256 and Lambda receives `repository@sha256:<digest>`. Lambda only accepts ECR images in the same region as the function: for an image in another region the plugin uses the same repository in the target's region (`123456789012.dkr.ecr.<region>.amazonaws.com/…`), which exists with [ECR replication](https://docs.aws.amazon.com/AmazonECR/latest/userguide/replication.html). The digest ensures it is the same image.
- **`.zip` package** (`PackageType: Zip`): a `.zip` file from any source, at most 50 MiB because it is uploaded directly. After the upload, the plugin checks that the sha256 computed by Lambda matches the artifact's.

```yaml
artifact:
  type: lambda
  name: shop-api
  version: "1.4.0"
  source: https://nexus.example.com/repository/lambda/shop-api-1.4.0.zip
  sha256: 8c1f…
```

For ECR credentials in the `artifacts` step, see the [ecs](ecs.md#the-artifact-is-an-image) plugin.

## Configuration

```yaml title="manent.yml"
plugins:
  lambda:
    artifact_types: [lambda]             # default
    # endpoint: http://localhost:4566    # alternative Lambda endpoint, e.g. LocalStack
```

## Targets and credentials

Each target is a **region** (`eu-west-1`), with a function of the same name. Credentials work as for the [ecs](ecs.md#targets-and-credentials) plugin: the standard AWS SDK chain, or `access_key_id_secret`/`secret_access_key_secret`/`session_token_secret`, plus `role_arn` and `external_id` to assume a role.

The role needs `lambda:GetFunctionConfiguration`, `lambda:UpdateFunctionCode`, `lambda:PublishVersion`, `lambda:ListVersionsByFunction`, `lambda:GetAlias`, `lambda:CreateAlias` and `lambda:UpdateAlias` on the function. For images, the ECR repository policy must allow Lambda to read them.

```yaml title="envconfig/shop-api/prod.yml"
plugin: lambda
targets:
  - {host: eu-west-1, labels: {region: eu-west-1}}
  - {host: eu-central-1, labels: {region: eu-central-1}}
params:
  function: shop-api-prod
  role_arn: arn:aws:iam::123456789012:role/manent-deploy
rollback:
  enabled: true
```

## Deploy parameters

| Parameter | Default | Description |
|---|---|---|
| `function` | application name | Function name. |
| `alias` | `live` | Alias moved to the new version. One function shared by several environments can use one alias per environment (`alias: prod`). |
| `wait_timeout` | `10m` | How long to wait for the code update and the new version. |

`artifact.version` is required: it is written in the description of the version and of the alias (`manent version=1.4.0 deployment=<id>`).

## How it works

For each region, in sequence:

1. it reads the function and the alias, and checks that the package type matches the artifact;
2. if the alias already points to the version published by this deploy, it does nothing: retries and recovery do not touch the function;
3. it waits for any update in progress to finish, then updates the code (`UpdateFunctionCode`) and waits for `LastUpdateStatus: Successful`. A failed update (for example an image that does not exist in the region) fails the deploy, and the alias stays where it was;
4. it publishes a version with the code's `CodeSha256`: if someone changed the code in the meantime, Lambda rejects the publication. Publishing unchanged code returns the existing version;
5. it waits for the version to be `Active`, then creates the alias or moves it to the new version. Any traffic weights on the alias are cleared.

Moving the alias is atomic: invocations go to the old version until the alias moves, then to the new one.

## Rollback

- **Live release** (`plugin` locator): the version in the alias description. An alias that is missing or not written by manent has no release; different versions across regions stop the deploy at the checkpoint.
- **Rollback:** on each region it moves the alias back to the function version of the previous release, saved at the checkpoint. Without that detail (a manual rollback) it looks for the most recent version published with that release. It is immediate, because versions are immutable and stay available. `$LATEST` keeps the new code until the next deploy.

The `lambda` artifact type allows rollback (`rollback.artifact_types.lambda.allowed: true` in the defaults).

## Percentage rollouts { #rilasci-per-percentuale }

With a `strategy: traffic` rollout ([Percentage rollouts](../guida/traffico.md)) the published versions are the slots: the deploy updates the code and publishes the version **without moving the alias**; the steps leave the alias on the stable version and send the candidate its share with `AdditionalVersionWeights`; the promotion moves the alias to the candidate and clears the weights. The return sends the alias back to the stable version, without weights. Testers invoke the candidate version directly (ARN qualified with the version number, reported in the output of `traffic:testers`). A rollout does not start if the alias already splits traffic. Versions without traffic use no resources: there is nothing to scale down.

## Dry-run

In [dry-run](../guida/dry-run.md), for each region: credentials, function, package type, alias, and `UpdateFunctionCode` with `DryRun`, which checks the request and the permissions without changing the code.

## Result

```json
{
  "plugin": "lambda",
  "targets": [
    {
      "host": "eu-west-1",
      "status": "deployed",
      "details": {"region": "eu-west-1", "function": "shop-api-prod", "alias": "live", "previous": "1.3.0", "function_version": "17"}
    }
  ]
}
```
