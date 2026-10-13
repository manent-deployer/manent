# cloudrun

The `cloudrun` plugin releases a container image to a Google Cloud Run service. It changes the image in the service template, which creates a new revision, and sends all traffic to the latest ready revision. The live release is the revision that receives the traffic: the rollback moves the traffic back to the previous revision, without recreating it. It talks directly to the Cloud Run Admin API v2: the worker does not need `gcloud`.

The service (network, service account, scaling, environment variables, secrets, IAM) belongs to whoever manages the infrastructure (Terraform): manent changes only the image. A service that does not exist yet is an error.

## The artifact is an image

The artifact is an image in a registry, given as an `oci://` source, as for the [container](container.md) plugin: the digest becomes the artifact's sha256 and the revision always runs `repository@sha256:<digest>`.

```yaml
artifact:
  type: cloudrun
  name: shop
  version: "1.4.0"
  source: oci://europe-docker.pkg.dev/acme/shop/shop:1.4.0
```

To read the manifest from Artifact Registry, the `artifacts` step can use `basic` credentials with user `_json_key` and the JSON key of a service account with the Artifact Registry Reader role as password:

```yaml title="manent.yml"
artifacts:
  sources:
    - prefix: oci://europe-docker.pkg.dev/acme/
      auth: {type: basic, username: _json_key, secret_ref: "vault:secret/data/manent/gcp#registry_key"}
```

## Configuration

```yaml title="manent.yml"
plugins:
  cloudrun:
    artifact_types: [cloudrun]                 # default
    # endpoint: https://run.googleapis.com     # default
```

## Targets and credentials

Each target is a **region** (`europe-west1`), with a service of the same name in the project. Several regions make a multi-region release; the `region` label lets [wave rollouts](../guida/ondate.md) release one region at a time.

Without `credentials_secret` the worker uses the [Application Default Credentials](https://cloud.google.com/docs/authentication/application-default-credentials): `GOOGLE_APPLICATION_CREDENTIALS`, workload identity on GKE, the metadata server on Compute Engine and Cloud Run. This is the recommended choice. With `credentials_secret` the secret holds the JSON key of a service account; other credential types are rejected, because they would make manent call URLs chosen by whoever writes the secret.

Define `project` and `credentials_secret` in the [environment configuration](../guida/configurazione-ambiente.md) and leave them out of the plugin's parameters in `descriptor_policy`.

The service account needs the Cloud Run Developer role (`roles/run.developer`) on the service and Service Account User (`roles/iam.serviceAccountUser`) on the service's runtime service account.

```yaml title="envconfig/shop/prod.yml"
plugin: cloudrun
targets:
  - {host: europe-west1, labels: {region: europe-west1}}
  - {host: us-central1, labels: {region: us-central1}}
params:
  project: acme-prod
  service: shop
rollback:
  enabled: true
```

## Deploy parameters

| Parameter | Default | Description |
|---|---|---|
| `project` | (required) | Google Cloud project ID. |
| `service` | application name | Cloud Run service: lower case, digits and `-`. |
| `container` | the only container, or the one named like the application | Container whose image changes. Sidecars keep theirs. |
| `credentials_secret` | Application Default Credentials | Secret with a service account's JSON key. |
| `wait_timeout` | `10m` | How long to wait for the new revision and the traffic change. |

`artifact.version` is required: it becomes the revision's `manent.io/version` annotation.

## How it works

For each region, in sequence:

1. it reads the service;
2. if the template already has this image, this deploy's annotations and all traffic on the latest revision, it skips to the check: retries and recovery do not create new revisions;
3. it changes the image of the chosen container, adds the annotations `manent.io/version` and `manent.io/deployment` to the template, removes any fixed revision name (Cloud Run generates one) and sets all traffic on the latest revision. The service is rewritten with its `etag`: a concurrent change is rejected instead of overwritten;
4. it waits for the operation to end. A revision that does not start (failed health check, missing image) fails the operation and the deploy, with Cloud Run's message: traffic stays on the previous ready revision, so the service keeps answering;
5. it checks that the latest created revision is the latest ready one.

The switch is immediate when the revision is ready. For a gradual canary, Cloud Run supports traffic split between revisions, which this plugin does not manage yet.

## Rollback

- **Live release** (`plugin` locator): the `manent.io/version` annotation of the revision that receives all the traffic. A revision without the annotation has no release; traffic split between several revisions, or different versions across regions, stop the deploy at the checkpoint.
- **Rollback:** on each region it sends all traffic to the revision of the previous release, saved at the checkpoint. Without that detail (a manual rollback) it looks for the most recent revision with that version. It is immediate: the revision already exists and is not recreated. The traffic stays pinned to that revision until the next deploy, which puts it back on the latest revision.

The `cloudrun` artifact type allows rollback (`rollback.artifact_types.cloudrun.allowed: true` in the defaults).

## Dry-run

In [dry-run](../guida/dry-run.md), for each region: credentials, service and container, then the update is sent with `validateOnly`. Cloud Run checks it with the permissions of the service account, but changes nothing.

## Result

```json
{
  "plugin": "cloudrun",
  "targets": [
    {
      "host": "europe-west1",
      "status": "deployed",
      "details": {"region": "europe-west1", "service": "shop", "previous": "1.3.0", "revision": "shop-00042-xuv", "url": "https://shop-abc123-ew.a.run.app"}
    }
  ]
}
```
