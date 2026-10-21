# ecs

The `ecs` plugin releases a container image to an Amazon ECS service, on Fargate or EC2. It registers a new revision of the task definition in use with only the image changed, updates the service and waits for the ECS deployment to complete. It talks to the ECS API with the official AWS SDK: the worker does not need the `aws` CLI.

The service, the cluster, the network, the IAM roles and the rest of the task definition belong to whoever manages the infrastructure (Terraform, CloudFormation, CDK): manent changes only the image. A service that does not exist yet is an error, not something the plugin creates.

## The artifact is an image

The artifact is an image in a registry, given as an `oci://` source, as for the [container](container.md) plugin: the manifest digest becomes the artifact's sha256 and the service always runs `repository@sha256:<digest>`, never the tag.

```yaml
artifact:
  type: ecs
  name: shop
  version: "1.4.0"
  source: oci://123456789012.dkr.ecr.eu-west-1.amazonaws.com/shop:1.4.0
```

To read the manifest from ECR, the `artifacts` step needs `basic` credentials with user `AWS` and the password printed by `aws ecr get-login-password`, which lasts 12 hours. Keep it in a file refreshed periodically (for example by a systemd timer or a sidecar) and read it with the `file:` provider:

```yaml title="manent.yml"
artifacts:
  sources:
    - prefix: oci://123456789012.dkr.ecr.eu-west-1.amazonaws.com/
      auth: {type: basic, username: AWS, secret_ref: "file:/run/manent/ecr-password"}
```

## Configuration

```yaml title="manent.yml"
plugins:
  ecs:
    artifact_types: [ecs]                # default
    # endpoint: http://localhost:4566    # alternative ECS endpoint, e.g. LocalStack
```

## Targets and credentials

Each target is a **region**, optionally with the cluster: `eu-west-1` or `eu-west-1/prod`. Several regions make a multi-region release, and the `region` label lets [wave rollouts](../guida/ondate.md) release one region at a time.

Without credential parameters the worker uses the standard AWS SDK chain: environment variables, IRSA on EKS (`AWS_ROLE_ARN`, `AWS_WEB_IDENTITY_TOKEN_FILE`), the ECS task role, the EC2 instance profile. This is the recommended choice: no keys to store.

| Parameter | Description |
|---|---|
| `access_key_id_secret`, `secret_access_key_secret` | Instead of the chain: secrets with static keys. |
| `session_token_secret` | Secret with the session token, for temporary keys. |
| `role_arn` | Role to assume with STS, for example a deploy role in the account of the environment. The session is called `manent-<deploy id>`, so CloudTrail links every call to the deploy. |
| `external_id` | External ID required by the role's trust policy. |

Define `role_arn` and credentials in the [environment configuration](../guida/configurazione-ambiente.md) and leave them out of the plugin's parameters in `descriptor_policy`: a descriptor must not be able to choose the account it deploys to.

The role needs `ecs:DescribeServices`, `ecs:DescribeTaskDefinition`, `ecs:ListTaskDefinitions`, `ecs:RegisterTaskDefinition`, `ecs:TagResource`, `ecs:UpdateService` and `iam:PassRole` on the task role and the execution role of the task definition.

```yaml title="envconfig/shop/shop/prod.yml"
plugin: ecs
targets:
  - {host: eu-west-1/prod, labels: {region: eu-west-1}}
  - {host: us-east-1/prod, labels: {region: us-east-1}}
params:
  service: shop-web
  role_arn: arn:aws:iam::123456789012:role/manent-deploy
rollback:
  enabled: true
```

## Deploy parameters

| Parameter | Default | Description |
|---|---|---|
| `cluster` | `default` | Cluster, for targets that give only the region. |
| `service` | application name | ECS service. |
| `container` | the only container, or the one named like the application | Container whose image changes. Sidecars keep theirs. |
| `wait_timeout` | `15m` | How long to wait for the ECS deployment. |
| `search_revision` | `100` | Revisions examined to find the one of a version during a rollback without details. |

`artifact.version` is required: it becomes a tag of the task definition.

## How it works

For each region, in sequence:

1. it reads the service and its task definition. A service with the `CODE_DEPLOY` or `EXTERNAL` deployment controller is rejected;
2. if the task definition in use was already registered by this deploy with this image, it skips to the wait: retries and recovery do not create new revisions;
3. it registers a new revision of the same family, copying every field of the current one (containers, environment, secrets, CPU and memory, roles, volumes, network mode) and changing only the image of the chosen container. It adds the tags `manent.application`, `manent.environment`, `manent.version` and `manent.deployment` to the existing ones;
4. it updates the service with the new revision;
5. it waits for the ECS deployment to be `COMPLETED`. A `FAILED` deployment, for example stopped by the deployment circuit breaker, fails the deploy at once with the reason given by ECS.

The deploy stops at the first region that fails. How tasks are replaced (`minimumHealthyPercent`, `maximumPercent`, load balancer health checks, circuit breaker) is the service's configuration: ECS replaces tasks without downtime when it is set accordingly.

## Native ECS strategies { #strategie-native }

The ECS controller has strategies that shift traffic on their own, on the ALB, the NLB or Service Connect: `BLUE_GREEN`, `LINEAR` and `CANARY` (`deploymentConfiguration.strategy` of the service, set with the infrastructure). The plugin does not replace them: it **delegates** to them. It updates the service as usual, reports the strategy in the target details (`strategy`) and extends the wait for the deployment by the expected duration (final bake, plus the steps of the linear strategy or the canary wait). Steps, bake, lifecycle hooks and the return on CloudWatch alarms are handled by ECS.

With delegation manent's gates, approvals between steps and freezes do not come between one step and the next. To have them, use a [percentage rollout](../guida/traffico.md) with two ECS services, one per slot (targets with the `slot` label), and the `aws-alb` router on their target groups.

## Rollback

- **Live release** (`plugin` locator): the `manent.version` tag of the task definition in use. A service whose task definition has no tag (never released by manent) has no release; different versions across regions stop the deploy at the checkpoint.
- **Rollback:** on each region it updates the service with the task definition of the previous release, saved at the checkpoint, and waits for the deployment. Without that detail (a manual rollback) it looks for the most recent revision of the family with the version tag. It carries on if a region fails. Nothing is registered: the previous revision is reused as it is.

If the circuit breaker has `rollback: true`, ECS already goes back by itself when the deployment fails; manent's rollback is still useful when a later step fails (`post-test`, `activation`).

The `ecs` artifact type allows rollback (`rollback.artifact_types.ecs.allowed: true` in the defaults).

## Dry-run

In [dry-run](../guida/dry-run.md), for each region: credentials, service, task definition and container. It registers nothing and does not change the service.

## Result

```json
{
  "plugin": "ecs",
  "targets": [
    {
      "host": "eu-west-1/prod",
      "status": "deployed",
      "details": {
        "region": "eu-west-1", "cluster": "prod", "service": "shop-web", "container": "shop",
        "previous": "1.3.0",
        "task_definition": "arn:aws:ecs:eu-west-1:123456789012:task-definition/shop:42"
      }
    }
  ]
}
```
