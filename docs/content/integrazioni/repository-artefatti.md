# Artifact repositories

The worker downloads every artifact from its `artifact.source` and verifies its sha256. `artifacts.sources` in `manent.yml` says which sources are allowed and which credentials go to each one: the descriptor carries only the URL, never a password.

!!! abstract "What you need"
    A read-only account on each repository · its credentials in the secret store · the corporate CA, if the repositories use one

## 1. Allow only your repositories

```yaml title="manent.yml"
artifacts:
  timeout: 30m
  allow_unlisted: false          # anything else is refused at submission (422) and on download
  sources:
    - prefix: https://nexus.example.com/repository/releases/
      auth: {type: basic, username: manent, secret_ref: "vault:secret/data/manent/nexus#password"}
```

- The **longest matching prefix** wins, so a narrower prefix can carry different credentials.
- Credentials are read at every download and are never forwarded on a redirect to another host.
- With `allow_unlisted: true` (the default, development only) a descriptor could make the worker download any URL reachable from its network, or copy a local file of the worker to the targets.

## 2. Repository by repository

=== "Sonatype Nexus"

    Create a user with the `nx-repository-view-*-*-read` privilege on the release repositories (or a dedicated role), then:

    ```yaml
    - prefix: https://nexus.example.com/repository/releases/
      auth: {type: basic, username: manent, secret_ref: "vault:secret/data/manent/nexus#password"}
    ```

=== "JFrog Artifactory"

    Create an access token with read permission on the repositories. Send it as a bearer token, or as an API key header on older installations:

    ```yaml
    - prefix: https://acme.jfrog.io/artifactory/releases-local/
      auth: {type: bearer, secret_ref: "vault:secret/data/manent/artifactory#token"}
    # - prefix: https://artifactory.example.com/artifactory/
    #   auth: {type: header, header: X-JFrog-Art-Api, secret_ref: "vault:secret/data/manent/artifactory#api_key"}
    ```

=== "GitLab or GitHub packages"

    A deploy token (GitLab) or a fine-grained token with package read access (GitHub):

    ```yaml
    - prefix: https://gitlab.example.com/api/v4/projects/42/packages/generic/
      auth: {type: header, header: DEPLOY-TOKEN, secret_ref: "vault:secret/data/manent/gitlab#deploy_token"}
    ```

=== "Container registries (OCI)"

    For `oci://` artifacts manent reads only the image manifest and uses its digest as the sha256; the image itself is pulled by the target (Docker, Kubernetes, ECS…). `basic` credentials are also used to obtain a registry token when the registry requires one (Harbor, GitLab, GHCR, Docker Hub).

    ```yaml
    - prefix: oci://registry.example.com/shop/
      auth: {type: basic, username: robot$manent, secret_ref: "vault:secret/data/manent/harbor#token"}
    # - prefix: oci+http://registry.lab:5000/     # plain-HTTP registry, labs only
    ```

=== "Local or shared directories"

    ```yaml
    - prefix: file:///srv/artifacts/
    ```

    Paths are normalized to `file://` plus the cleaned absolute path, so `../` cannot escape the prefix.

## 3. Private CAs

```yaml
- prefix: https://nexus.internal/repository/releases/
  ca_file: /etc/ssl/certs/corp-ca.pem
  auth: {type: basic, username: manent, secret_ref: "vault:secret/data/manent/nexus#password"}
```

## 4. Pin what you release

Publish the sha256 from CI and pass it to manent, so that the `qa` step refuses any other file, even one with the same name and version:

```sh
manent deploy -f deploy/test.yml --version "$TAG" \
  --source "https://nexus.example.com/repository/releases/shop/shop-$TAG.tgz" \
  --sha256 "$(sha256sum dist/shop-$TAG.tgz | cut -d' ' -f1)"
```

Promotions pin it automatically: production receives exactly the bytes that succeeded in the previous environment. For images, refer to them by digest (`oci://registry.example.com/shop/api@sha256:…`) or let manent pin the tag to the digest it read.

## Check it

```sh
manent validate -f deploy/test.yml     # a source outside artifacts.sources is refused here
manent validate -f deploy/test.yml --check-artifact   # the artifact exists and the credentials work
manent deploy -f deploy/test.yml --dry-run
```

`--check-artifact` asks the API server to find the artifact without downloading it (`HEAD`, or the manifest for an image, whose digest is compared with `artifact.sha256`). The `artifacts` step of the dry-run downloads the artifact with the configured credentials and reports `sha256` and `size`.

Reference: [Secrets and artifact sources](../operazioni/segreti-artefatti.md).
