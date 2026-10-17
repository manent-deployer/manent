# appservice

The `appservice` plugin releases to **Azure App Service** web apps. The artifact can be:

- a **package** (`.zip`, `.war`, `.jar` or `.ear`), sent to Kudu with the OneDeploy API;
- a **container image**, which becomes `linuxFxVersion` (or `windowsFxVersion`) `DOCKER|<repository>@sha256:<digest>`.

It talks directly to the Azure Resource Manager REST API and to Kudu: the worker needs neither the Azure CLI nor the SDK.

The web app, its App Service plan, its slots, the settings and the identity belong to whoever manages the infrastructure (Terraform, Bicep). manent changes only the content and the release metadata. A web app or a slot that does not exist yet is an error.

## With a slot or straight to production

| | With `slot` (recommended) | Without `slot` |
|---|---|---|
| Where the release goes | To the slot (e.g. `staging`). After its health check, a **swap** with production. | Straight to production. |
| Release in service | `MANENT_VERSION` and `MANENT_DEPLOYMENT` app settings of the slot: they travel with the swap. | Tags `manent-version`, `manent-deployment` and, for an image, `manent-image` on the web app. Writing tags does not restart the app. |
| Rollback | A swap back: the previous version is still in the slot. | Only for an image: the previous image goes back. A package without a slot cannot be rolled back. |
| Percentage rollout | Yes: `rampUpRules` route a share of production traffic to the slot, then the swap. | No. |

The swap is the deployment model Azure recommends. The slot warms up before receiving production traffic, and production never runs a half-deployed version.

The two `MANENT_*` settings must not be *deployment slot settings* (sticky): they must travel with the swap. The plugin checks this after every swap.

## Configuration

```yaml title="manent.yml"
plugins:
  appservice:
    artifact_types: [appservice]      # default
    # management_endpoint: https://management.azure.com   # sovereign clouds
    # authority: https://login.microsoftonline.com
```

`scm_endpoint` and `site_endpoint` replace `https://<host>` of Kudu and of the web apps, followed by `/<host>`. They are for proxies and for the emulated Azure of the [use cases](../casi-uso.md).

## Targets and credentials { #target-e-credenziali }

Each target is a web app: `host: <name>`, with the resource group from the `resource_group` parameter, or `host: <resource group>/<name>`. Several web apps make a multi-region release (one per region, each behind Front Door or Traffic Manager). The `region` label lets [wave rollouts](../guida/ondate.md) release one region at a time.

Credentials, in order:

1. with `client_secret_secret`, a **service principal**: `tenant_id`, `client_id` and the secret with the client secret;
2. otherwise the identity of the worker:
    - **workload identity** on AKS (`AZURE_FEDERATED_TOKEN_FILE`, `AZURE_CLIENT_ID`, `AZURE_TENANT_ID`);
    - the **managed identity** of App Service or Container Apps (`IDENTITY_ENDPOINT`);
    - the managed identity of the VM (IMDS); `client_id` chooses a user-assigned one.

The identity of the worker is the recommended choice: no secret to rotate. The identity needs the **Website Contributor** role on the web app (content, settings, swap) and **Tag Contributor** for the tags. Kudu accepts the same Entra ID token, so basic publishing credentials can stay disabled.

```yaml title="envconfig/shop/prod.yml"
plugin: appservice
targets:
  - {host: shop-weu, labels: {region: westeurope}}
  - {host: rg-shop-us/shop-eus, labels: {region: eastus}}
params:
  subscription: 9b2c5e1a-0000-4000-8000-1234567890ab
  resource_group: rg-shop-eu
  slot: staging
  health_path: /healthz
rollback:
  enabled: true
```

## Deploy parameters

| Parameter | Default | Description |
|---|---|---|
| `subscription` | (required) | Subscription ID. |
| `resource_group` | | Resource group of the targets written as `<name>`. |
| `slot` | | Deployment slot to release to and swap with production; never `production`. |
| `health_path` | | Path probed until it answers `2xx`: on the slot before the swap, or on production without a slot. |
| `wait_timeout` | `15m` | Wait for Kudu, the health check and the swap, for each web app. |
| `tenant_id`, `client_id` | | Service principal or user-assigned managed identity. |
| `client_secret_secret` | | Secret with the client secret of the service principal. |

`artifact.version` is required. The artifact type is `appservice`; the extension tells a package from an image (`oci://` source, as for the [container](container.md) plugin).

## How it works

For each web app, in sequence:

1. with a slot, if production already reports this deploy (`MANENT_DEPLOYMENT`), it stops there: a retry or a recovery does not swap twice;
2. it writes the release in the slot settings (the slot restarts, production does not). Without a slot, it marks production with the `manent-pending` tag, so the rollback knows production was touched;
3. it releases the content:
    - a package: Kudu's `POST /api/publish?type=<zip|war|jar|ear>&async=true`, then it waits for the new deployment record. A failed deployment fails the deploy with Kudu's status and the log URL;
    - an image: the `linuxFxVersion` or `windowsFxVersion` of the site configuration;
4. with `health_path`, it waits for `2xx` on the slot (or on production);
5. with a slot, the **swap** (`slotsswap`, long-running operation with `preserveVnet`). Then it checks that production reports this deploy. Without a slot, the tags.

## Rollback and real state

- **Release in service** (`plugin` locator, [reconciliation](../guida/governance.md#stato-reale)): with a slot, the `MANENT_VERSION` setting of production. A swap done by hand is therefore seen as a change of version. Without a slot, the `manent-version` tag; for an image, a running image different from `manent-image` is no release of manent.
- **Rollback with a slot:** if the deploy reached the swap, a swap back, after checking that the slot holds the previous version. If the deploy stopped before the swap, production was never touched, and nothing changes.
- **Rollback without a slot:** an image goes back to the previous one, read at the checkpoint. A package cannot, and the rollback fails: use a slot.

The `appservice` artifact type allows rollback (`rollback.artifact_types.appservice.allowed: true` in the defaults).

## Percentage rollouts { #rilasci-per-percentuale }

With a `strategy: traffic` rollout ([Percentage rollouts](../guida/traffico.md)) and a slot:

- the candidate is released to the slot without traffic;
- every step writes a rule in the `experiments.rampUpRules` of production ("Testing in production"), which routes that share of the requests to the slot;
- testers reach the slot on its own host name (`tester_url`), or production with the `x-ms-routing-name=<slot>` cookie;
- the promotion removes the rule and swaps; the return removes the rule and, if already swapped, swaps back.

The previous version stays in the slot, as the point to go back to.

## Dry-run

In [dry-run](../guida/dry-run.md), for each web app it reads the web app, the slot, the site configuration and the settings and, for a package, the Kudu deployments. It changes nothing.
