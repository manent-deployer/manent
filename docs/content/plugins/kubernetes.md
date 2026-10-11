# kubernetes

The `kubernetes` plugin releases manifests to one or more Kubernetes clusters. It uses server-side apply, waits for workloads to finish rolling out and keeps the release history in the cluster, which it uses to go back to the previous release. It talks directly to the cluster API: the worker needs neither `kubectl` nor `helm`.

## The artifact is the manifests

The artifact is a manifest file (YAML or JSON, also with several documents separated by `---`) or a `.tar.gz` archive of `.yaml`, `.yml` and `.json` files, read in name order. It is the artifact verified with the sha256 and promoted between environments: the same file goes to test and to production.

```yaml
artifact:
  type: kubernetes
  name: shop
  version: "1.4.0"
  source: https://nexus.example.com/repository/manifests/shop-1.4.0.yaml
  sha256: 8c1f…
```

For Helm charts or Kustomize configurations, release the output generated in CI (`helm template`, `kustomize build`): the artifact stays immutable and verifiable.

### Variables

What changes between environments comes from the [environment configuration](../guida/configurazione-ambiente.md), not from the manifests. The manifests can use these variables:

| Variable | Value |
|---|---|
| `${MANENT_APPLICATION}`, `${MANENT_ENVIRONMENT}`, `${MANENT_VERSION}` | Application, environment and version of the deploy. |
| `${MANENT_NAMESPACE}` | The `namespace` parameter. |
| `${MANENT_DEPLOYMENT}` | Deploy ID. |
| `${NAME}` | The `var.NAME` parameter, for example `var.REPLICAS: "3"` for `${REPLICAS}`. |

Any other `${...}` is left unchanged, because it may be part of a script inside the manifests.

## Configuration

```yaml title="manent.yml"
plugins:
  kubernetes:
    # ca_file: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt   # manent inside the cluster
    insecure_skip_tls_verify: false      # true only in development
    artifact_types: [kubernetes]         # default
```

## Targets and credentials

Each target is a cluster's API server: `host` (or an `https://…` URL) and `port` (default 6443). The `region` and `az` labels let [wave rollouts](../guida/ondate.md) release one cluster at a time.

| Parameter | Description |
|---|---|
| `token_secret` | Secret with the bearer token, for example a ServiceAccount's. |
| `client_cert_secret`, `client_key_secret` | Instead of the token: PEM client certificate and key. |
| `ca_secret` | Secret with the cluster CA (PEM). Default: the plugin's `ca_file` or the system CAs. |

With manent running inside the cluster, the service account token is read with the `file:` secret provider:

```yaml title="envconfig/shop/prod.yml"
plugin: kubernetes
targets:
  - {host: kubernetes.default.svc, port: 443, labels: {region: eu-1}}
params:
  namespace: shop-prod
  token_secret: kube-token
  var.REPLICAS: "4"
secrets:
  - {name: kube-token, ref: "file:/var/run/secrets/kubernetes.io/serviceaccount/token"}
```

The token's permissions decide what manent can change: a Role limited to the application's namespace is the usual choice. Besides the objects in the manifests, it needs permissions on ConfigMaps in the namespace for the release history.

## Deploy parameters

| Parameter | Default | Description |
|---|---|---|
| `namespace` | `default` | Namespace of objects that do not set one, and of the history. |
| `wait_timeout` | `5m` | How long to wait for workloads to roll out. |
| `prune` | `false` | `true`: after the deploy, delete objects of the previous release that the new manifests no longer contain. Namespaces are never deleted. |
| `keep_releases` | `5` | Releases kept in the history (at least 2). |
| `require_image_digest` | `false` | `true`: reject workloads whose images are not pinned to a digest (`image@sha256:…`). |
| `var.NAME` | | Value of `${NAME}` in the manifests. |

## How it works

For each cluster, in sequence:

1. it reads the manifests, substitutes the variables and checks every object against the cluster's discovery (existing apiVersion and kind). Objects without a namespace get `namespace`;
2. it adds the labels `app.kubernetes.io/managed-by: manent`, `manent.io/application`, `manent.io/environment` and the annotations `manent.io/version` and `manent.io/deployment`;
3. it applies the Namespaces first, then saves the release in the history, then applies the other objects in dependency order (CRDs, permissions, ConfigMaps and Secrets, Services, workloads). Server-side apply uses the `manent` field manager and is idempotent;
4. it waits for Deployments, StatefulSets and DaemonSets to roll out. A Deployment past its `progressDeadlineSeconds` fails immediately, without waiting for `wait_timeout`;
5. with `prune`, it deletes the objects removed from the manifests;
6. it updates the live release and removes releases beyond `keep_releases` from the history.

To stop a deploy that does not start without waiting for the timeout, set `progressDeadlineSeconds` on the Deployments.

## History and rollback

The history is made of ConfigMaps in the namespace:

- `manent-<app>-<env>`: the live release;
- `manent-<app>-<env>-<version>`: the objects applied by each release, compressed (about 900 KiB at most per release).

The **rollback** re-applies on each cluster the saved objects of the previous release, waits for them to roll out and, with `prune`, deletes the objects created only by the failed release. The previous artifact does not need to be downloaded again. The live release (`plugin` locator) is read from the history; clusters with different releases stop the deploy at the checkpoint.

The `kubernetes` artifact type allows rollback (`rollback.artifact_types.kubernetes.allowed: true` in the defaults).

## Dry-run

In [dry-run](../guida/dry-run.md), for each cluster: version and credentials, then every object is applied with `dryRun=All`. The cluster validates it with schema, admission webhooks and RBAC, but saves nothing. Objects in a namespace that the manifests themselves would create cannot be checked and are counted in `objects_skipped`.

## Result

```json
{
  "plugin": "kubernetes",
  "targets": [
    {
      "host": "k8s-eu1.example.com",
      "status": "deployed",
      "details": {"namespace": "shop-prod", "objects": "4", "release": "manent-shop-prod-1-4-0-1a2b3c4d", "previous": "1.3.0", "pruned": "ConfigMap/shop-prod/shop-legacy"}
    }
  ]
}
```
