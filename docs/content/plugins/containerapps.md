# containerapps

The `containerapps` plugin releases a container image to an **Azure Container Apps** app. It changes the image of the template, which creates a new revision with a suffix derived from the deploy, and waits for the revision to be running and healthy. It talks directly to the Azure Resource Manager REST API: the worker needs neither the Azure CLI nor the SDK.

The app, its environment, ingress, scale rules, secrets and identity belong to whoever manages the infrastructure (Terraform, Bicep): manent changes only the image, two environment variables and the traffic. An app that does not exist yet is an error.

## The artifact is an image

As for the [container](container.md) plugin, the artifact is a registry image given as an `oci://` source: the digest becomes the artifact's sha256 and the revision always runs `repository@sha256:<digest>`.

```yaml
artifact:
  type: containerapps
  name: shop-api
  version: "2.4.0"
  source: oci://acme.azurecr.io/shop/api:2.4.0
```

The version is written in the **`MANENT_VERSION`** and **`MANENT_DEPLOYMENT`** environment variables of the container: every revision carries its release.

## Configuration

```yaml title="manent.yml"
plugins:
  containerapps:
    artifact_types: [containerapps]   # default
    # management_endpoint: https://management.azure.com
```

## Targets and credentials

Each target is an app: `host: <name>` with the `resource_group` parameter, or `host: <resource group>/<name>`. Credentials are those of the [appservice](appservice.md#target-e-credenziali) plugin: service principal with `client_secret_secret`, or the identity of the worker (workload identity, managed identity). The identity needs the **Container Apps Contributor** role on the app.

```yaml title="envconfig/shop-api/prod.yml"
plugin: containerapps
targets:
  - {host: shop-api, labels: {region: westeurope}}
params:
  subscription: 9b2c5e1a-0000-4000-8000-1234567890ab
  resource_group: rg-shop
  container: api
rollback:
  enabled: true
```

## Deploy parameters

| Parameter | Default | Description |
|---|---|---|
| `subscription` | (required) | Subscription ID. |
| `resource_group` | | Resource group of the targets written as `<name>`. |
| `container` | the only container, or the one named like the application | Container whose image changes. Sidecars and init containers keep theirs. |
| `wait_timeout` | `10m` | Wait for the revision and the traffic, for each app. |
| `tenant_id`, `client_id`, `client_secret_secret` | | As for [appservice](appservice.md). |

## How it works

For each app, in sequence:

1. if the revision `<app>--m<hash of the deploy>` already exists, it skips to the wait: retries and recoveries do not create revisions;
2. it changes image and `MANENT_*` variables of the container and the `revisionSuffix` of the template, and in **Multiple** mode it sets all traffic on the latest revision. The update is a `PATCH`: the secrets of the app are never read or rewritten;
3. it waits for the revision to be provisioned, running and healthy. A revision that fails or is unhealthy fails the deploy with the reason of Container Apps. Traffic stays on the previous revision: in Multiple mode the latest *ready* revision receives it, in Single mode the previous one stays active.

## Rollback and real state

- **Release in service:** the `MANENT_VERSION` variable of the revision receiving traffic (the largest weight; in Single mode, or without ingress, the latest ready revision).
- **Rollback, Multiple mode:** the traffic goes back to the previous revision, activated if needed. It is immediate: the revision already exists.
- **Rollback, Single mode:** a new revision with the previous image and version (suffix `r<hash>`), because Single mode keeps only one revision active.

An app the deploy did not change is left alone. The `containerapps` artifact type allows rollback in the defaults.

## Percentage rollouts { #rilasci-per-percentuale }

With `strategy: traffic` ([Percentage rollouts](../guida/traffico.md)) the app must be in **Multiple** mode with an ingress:

- the candidate revision is created with all the traffic held on the stable one;
- the steps split the ingress traffic between the two revisions;
- with `testers.tag` the candidate gets a label, with the URL `https://<app>---<label>.<environment domain>`;
- the promotion sends all the traffic to the latest revision, the return to the stable one;
- the replaced revision is deactivated after `scaleDownDelay`, unless it receives traffic again.

## Dry-run

In [dry-run](../guida/dry-run.md), for each app: credentials, app, container and the revision in service, without changes.
