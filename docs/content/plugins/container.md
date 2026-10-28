# container

The `container` plugin releases a container image to servers and VMs over SSH, with Docker or Podman. On each target it pulls the image, replaces the application's container and waits until it is ready. It uses the same SSH connection as [remote-copy](remote-copy.md), so the targets need no agent.

## The artifact is an image

The artifact is an image in a registry, given as an `oci://` source:

```yaml
artifact:
  type: container
  name: shop
  version: "1.4.0"
  source: oci://registry.example.com/team/shop:1.4.0
```

The `artifacts` step does not download the layers: it reads the manifest from the registry and computes its digest, which becomes the **artifact's sha256**. From then on the plugin always starts `registry.example.com/team/shop@sha256:<digest>`, never the tag. This means:

- a deploy resumed after a crash, a promotion or a manual rollback release exactly the image that was verified;
- if someone moves the tag in the meantime, the `qa` check blocks the promotion: `sha256 atteso <digest>, calcolato <other digest>`;
- in the descriptor you can pin `artifact.sha256` to the digest (without `sha256:`) or use `oci://registry/team/shop@sha256:<digest>` directly.

OCI and Docker v2 manifests are accepted, multi-architecture indexes included. Registries that require a token (Docker Hub, GHCR, Harbor, GitLab) are handled automatically. Credentials are configured as for the other [artifact sources](../operazioni/segreti-artefatti.md):

```yaml title="manent.yml"
artifacts:
  allow_unlisted: false
  sources:
    - prefix: oci://registry.example.com/team/
      auth: {type: basic, username: manent, secret_ref: "vault:secret/data/manent/registry#password"}
```

`oci+http://` is a plain-HTTP registry, for development and labs only.

## Configuration

```yaml title="manent.yml"
plugins:
  container:
    known_hosts: ~/.ssh/known_hosts      # default
    insecure_ignore_host_key: false      # true only in development
    artifact_types: [container]          # default
```

## Deploy parameters

| Parameter | Required | Description |
|---|---|---|
| `ssh_key_secret` | one of the two | Name of the secret with the PEM private key. |
| `password_secret` | one of the two | Name of the secret with the SSH password. |
| `runtime` | no | `docker` (default) or `podman`. |
| `sudo` | no | `true` to run the runtime with `sudo -n`. Normally the user is in the `docker` group. |
| `container_name` | no | Name of the container. Default: the application name. |
| `run_args` | no | Extra `run` arguments: ports, volumes, network, `--env-file`, limits. For example `-p 8080:8080 --env-file /etc/shop.env`. |
| `command` | no | Arguments passed to the container after the image. |
| `registry_username` | no | User for the registry login on the target. |
| `registry_password_secret` | with `registry_username` | Secret with the registry password. It is passed on the stdin of `login`, never on the command line. |
| `wait_timeout` | no | How long to wait for startup (default `60s`). |
| `stable_for` | no | Without a `HEALTHCHECK` in the image: how long the container must run without restarts to be considered ready (default `5s`). |

`artifact.version` is required and becomes the container's `manent.version` label. Every target must have a `user`.

!!! warning "`run_args` and `command` are commands"
    They reach the target's shell as written. Define them in the [environment configuration](../guida/configurazione-ambiente.md) and exclude them from the `descriptor_policy`, like the remote-copy commands.

```yaml title="envconfig/shop/shop/prod.yml"
plugin: container
targets:
  - {host: vm1.example.com, user: deploy, labels: {region: eu-1, az: eu-1a}}
  - {host: vm2.example.com, user: deploy, labels: {region: eu-1, az: eu-1b}}
params:
  ssh_key_secret: deploy-key
  run_args: "-p 8080:8080 --env-file /etc/shop/prod.env"
  wait_timeout: 90s
secrets:
  - {name: deploy-key, ref: "vault:secret/data/shop/prod#ssh_key"}
rollback:
  enabled: true
```

## How it works

For each target (one after the other, or [`parallel`](../guida/ondate.md#parallel-targets) at a time):

1. it reads the running container (`container inspect`): version, image and state;
2. if the container already has the same image and version and is ready, it leaves it alone. Retries and recovery do not restart the application;
3. it logs in to the registry, if configured;
4. it pulls the image by digest;
5. it removes the previous container and starts the new one with `--restart unless-stopped`, the labels `manent.application`, `manent.environment`, `manent.version` and `manent.deployment`, `run_args` and `command`;
6. it waits until the container is ready: with a `HEALTHCHECK` in the image, until it is `healthy`; without one, until it has been running for `stable_for` without restarts. A container that exits or is `unhealthy` fails the deploy, and the last lines of its log go to the deploy log.

After the first target that fails no other target starts; the ones already running finish. Between removing the old container and starting the new one the application on that target does not answer: to avoid downtime, use a [wave rollout](../guida/ondate.md) with `maxUnavailable` and a load balancer that skips targets that are not ready.

## Rollback

- **Live release** (`plugin` locator): version and image read from the container's labels. Targets without a container are ignored; different releases across targets stop the deploy at the checkpoint.
- **Rollback:** on each target it restarts the container with the previous image, by digest, with the same wait as the deploy. It carries on if a target fails. Previous images stay cached on the target, so the rollback does not depend on the registry; if they are missing they are pulled again.

The `container` artifact type allows rollback (`rollback.artifact_types.container.allowed: true` in the defaults).

## Dry-run

In [dry-run](../guida/dry-run.md), for each target: SSH connection, runtime `version` with the user's permissions, and the running container. It does not pull and does not change any container.

## Result

```json
{
  "plugin": "container",
  "targets": [
    {
      "host": "vm1.example.com",
      "status": "deployed",
      "details": {
        "container": "shop",
        "image": "registry.example.com/team/shop@sha256:4f1c…",
        "previous_version": "1.3.0",
        "previous_image": "registry.example.com/team/shop@sha256:9ab2…",
        "state": "running healthy"
      }
    }
  ]
}
```
