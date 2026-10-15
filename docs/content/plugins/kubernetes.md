# kubernetes

The `kubernetes` plugin releases manifests or Helm charts to one or more Kubernetes clusters. It uses server-side apply, waits for workloads to finish rolling out and keeps the release history in the cluster, which it uses to go back to the previous release. It talks directly to the cluster API: the worker does not need `kubectl`, and needs `helm` only to render charts.

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

For a Helm chart see [Helm charts](#chart-helm). For Kustomize configurations, release the output generated in CI (`kustomize build`): the artifact stays immutable and verifiable.

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
    helm:                                # charts only
      command: helm                      # default; included in the manent-server image
      # sha256: "<sha256 of the binary>" # checked before every render
      timeout: 2m                        # default
```

## Helm charts { #chart-helm }

The artifact can be a chart packaged with `helm package` (a `.tgz` with `<name>/Chart.yaml`), recognized automatically. The package is the artifact verified with the sha256 and promoted between environments; dependencies must be included (`helm package --dependency-update`).

```yaml
artifact:
  type: kubernetes
  name: shop
  version: "1.4.0"
  source: https://nexus.example.com/repository/charts/shop-1.4.0.tgz
  sha256: 8c1f…
```

The chart is only **rendered** with `helm template`, for each cluster, as Argo CD does: the generated objects follow the same path as manifests (labels, server-side apply, history, rollout wait, prune, dry-run). Helm installs nothing and keeps no state in the cluster: no `sh.helm.release.v1` secrets, and `helm list` does not show the release. The rollback re-applies the objects saved in the history and does not need Helm.

- **Cluster capabilities.** `--kube-version` and `--api-versions` come from the target cluster's discovery, so `.Capabilities.KubeVersion` and `.Capabilities.APIVersions.Has` answer as in a `helm install`.
- **Values**, in increasing order of precedence: the chart's `values.yaml`, the files in `helm_values_files` (inside the chart, for example `values-prod.yaml`), `helm_values` (YAML), the single `helm_value.<path>` entries. Like the other parameters, they come from the [environment configuration](../guida/configurazione-ambiente.md).
- **Hooks.** Hooks (`helm.sh/hook`) have no equivalent in server-side apply: by default they are not applied and are listed in the result (`helm_hooks_skipped`) and in the log. With `helm_hooks: apply` they are applied as normal objects, together with the others; a migration Job must then be idempotent or renamed at every version (for example with `{{ .Chart.AppVersion }}` in the name). Tests (`helm.sh/hook: test`) are never applied.
- **CRDs** in the `crds/` folder are included and applied first (`helm_skip_crds: true` to leave them out).
- **`lookup`** does not read the cluster during rendering and always returns an empty value.
- **Isolation.** Helm runs with an empty environment and a temporary home: it does not inherit the worker's variables and reads no `KUBECONFIG`, repositories or local credentials. With `helm.sha256` the binary is checked before every use.
- The `${...}` manifest variables do not apply to charts: use values.

```yaml title="envconfig/shop/prod.yml"
plugin: kubernetes
targets:
  - {host: k8s-eu1.example.com, labels: {region: eu-1}}
params:
  namespace: shop-prod
  token_secret: kube-token
  helm_values_files: values-prod.yaml
  helm_value.replicaCount: "4"
  helm_values: |
    ingress:
      enabled: true
      host: shop.example.com
```

An application already installed with `helm install` can move to manent: server-side apply adopts its objects. Then delete only Helm's state (the `sh.helm.release.v1.<release>.*` secrets in the namespace), not the release with `helm uninstall`, which would delete the objects.

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
| `helm_release` | application name | Helm release name (`.Release.Name`). |
| `helm_values_files` | | Values files inside the chart, comma separated, applied in order. |
| `helm_values` | | Values in YAML. |
| `helm_value.<path>` | | A single value, parsed as YAML: `helm_value.image.tag: "1.4.0"`, `helm_value.replicaCount: "3"`. |
| `helm_hooks` | `skip` | `apply`: apply the chart's hooks as normal objects. |
| `helm_skip_crds` | `false` | `true`: do not apply the CRDs in the `crds/` folder. |

## How it works

For each cluster, in sequence:

1. it reads the manifests and substitutes the variables, or renders the chart with the cluster's capabilities; then it checks every object against the cluster's discovery (existing apiVersion and kind). Objects without a namespace get `namespace`;
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

## Percentage rollouts { #rilasci-per-percentuale }

With a `strategy: traffic` rollout ([Percentage rollouts](../guida/traffico.md)) the plugin keeps two versions side by side in two **slots**, `a` and `b`, alternated at every release, and shifts traffic with the weights of a Gateway API `HTTPRoute` (or `GRPCRoute`).

- **Slot objects.** Deployments, StatefulSets, DaemonSets, Services, HorizontalPodAutoscalers and PodDisruptionBudgets of the manifests get the `-<slot>` suffix (`shop` → `shop-b`) and the `manent.io/track: <slot>` label, also in selectors and pods; `serviceName` of StatefulSets and `scaleTargetRef` of HPAs follow the new name. The other objects (ConfigMaps, Secrets, …) are shared by the two slots: a change applies to the stable version at once.
- **The route** belongs to the infrastructure: manent only changes the weights of the `<traffic_service>-a` and `<traffic_service>-b` `backendRefs` (it must have both) with a merge patch conditioned on the `resourceVersion` it read. The rule with a match on the testers' header (`rollout.traffic.testers.header`) leads only to the candidate.
- **History.** The ConfigMap of the release in service also records the slot and, after a promotion, the replaced release. The return scales the candidate down to zero; after `scaleDownDelay` the retirement scales the workloads of the replaced slot down to zero and deletes its HPAs.

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: shop, namespace: shop}
spec:
  parentRefs: [{name: public}]
  rules:
    - matches: [{headers: [{name: x-canary, value: always}]}]
      backendRefs: [{name: shop-a, port: 80}]
    - backendRefs:
        - {name: shop-a, port: 80, weight: 100}
        - {name: shop-b, port: 80, weight: 0}
```

| Parameter | Default | Description |
|---|---|---|
| `traffic_route` | | Name of the route that splits traffic. |
| `traffic_service` | | Base name of the slot Services (`<name>-a`, `<name>-b`). |
| `traffic_route_kind` | `HTTPRoute` | `HTTPRoute` or `GRPCRoute`. |
| `traffic_route_namespace` | `namespace` | Namespace of the route. |
| `traffic_router` | `gateway-api` | Router of the route. |

An application already released without slots (objects without suffix) moves to slots with an explicit operation: the first percentage rollout creates `shop-a` next to `shop` and receives all the traffic at once (there is no stable version in the slots); the route must be prepared with the `shop-a` and `shop-b` backends and the objects without suffix removed by hand after checking.

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
