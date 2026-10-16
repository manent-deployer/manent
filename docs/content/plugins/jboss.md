# jboss

The `jboss` plugin releases Java EE / Jakarta EE applications (`war`, `ear`) to JBoss EAP and WildFly through the **HTTP management API**, on standalone servers or on a domain controller. It needs no SSH and no `jboss-cli` on the worker: it uploads the archive to the content repository and switches versions with management operations.

Supported versions: JBoss EAP 7.4 and later, WildFly 23 and later (the `suspend-timeout` parameter of `suspend`).

## How versions are switched

Every version becomes a separate deployment, `<application>-<version>.<ext>` (for example `shop-1.4.0.war`), with the same **runtime-name** (default `<application>.<ext>`, so the context root does not change). For each target, in sequence:

1. it reads the deployments; if the requested version is already active with the same content it touches nothing, so retries and recoveries do not restart the application;
2. it uploads the archive (`/management/add-content`) and adds the deployment, **disabled**;
3. with `suspend: true` it suspends the server: new requests get `503`, and requests in progress have `suspend_timeout` to finish;
4. it switches versions with `replace-deployment` (in domain mode for every server group) in a single **composite** operation: if the new version fails to start the server rolls back the whole operation and the previous version stays active;
5. with `restart: true` it restarts the JVM, with `reload: true` it reloads the server, and in both cases it waits until it is `running` again. Otherwise, if it had suspended it, it resumes the server;
6. it removes the oldest versions beyond `keep_releases`, never the active one nor the previous one.

If the switch fails after a suspend, the plugin resumes the server before returning the error, so the previous version goes back into service.

!!! note "Same version, different content"
    If a deployment with the same name but different content is already active (an artifact rebuilt with the same version), the deploy fails: use a new version. A disabled deployment with different content is replaced.

## Configuration

```yaml title="manent.yml"
plugins:
  jboss:
    artifact_types: [war, ear]          # default
    ca_file: /etc/manent/jboss-ca.pem   # CA of the management certificate, if private
    insecure_skip_tls_verify: false     # true only in development
    request_timeout: 10m                # one call, upload included
```

## Deploy parameters

| Parameter | Default | Description |
|---|---|---|
| `username` | | Management user (`add-user.sh`, `ManagementRealm`). |
| `password_secret` | | Name of the secret with the management password. |
| `tls` | `true` | `https` to the management interface (port `9993`); `false` for `http` (port `9990`). The target `port` overrides the port. |
| `server_groups` | | Domain mode only: comma-separated server groups the application is deployed to. |
| `runtime_name` | `<application>.<ext>` | Runtime name shared by all versions (for example `ROOT.war`). |
| `suspend` | `false` | Suspends the server (in domain mode the server groups) before the switch. |
| `suspend_timeout` | `60s` | How long requests in progress have to finish during the suspend. |
| `reload` | `false` | Reloads the server after the switch, without restarting the JVM. |
| `restart` | `false` | Restarts the JVM after the switch. Alternative to `reload`. |
| `keep_releases` | `3` | Versions kept in the content repository, the active one included (at least 2, to keep the previous one for the rollback). |
| `wait_timeout` | `5m` | How long to wait for the server after `reload` or `restart`. |

The boolean parameters accept `true` and `false`. The authentication follows the server challenge: digest (MD5 or SHA-256, the default of the management realm) or basic. The plugin never follows redirects, so credentials only go to the target host.

`artifact.version` is required and becomes part of the deployment name. The extension comes from the downloaded file or from `artifact.type` (`.war`, `.ear`, `.jar`, `.rar`, `.sar`).

```yaml title="envconfig/shop/prod.yml"
plugin: jboss
targets:
  - {host: eap1.example.com, labels: {region: eu-1, az: eu-1a}}
  - {host: eap2.example.com, labels: {region: eu-1, az: eu-1b}}
params:
  username: manent
  password_secret: jboss-mgmt
  suspend: "true"
  suspend_timeout: 90s
secrets:
  - {name: jboss-mgmt, ref: "vault:secret/data/shop/prod#jboss_password"}
rollback:
  enabled: true
```

In the [descriptor policy](../guida/configurazione-ambiente.md) only allow the parameters a team may change, for example `jboss: [suspend, suspend_timeout, reload, restart, wait_timeout]`: user, secrets and server groups belong to the environment configuration.

### Standalone and domain mode

The plugin reads `launch-type` from the target:

- **standalone:** every target is a server. Targets are released one at a time; with a [wave rollout](../guida/ondate.md) and `maxUnavailable`, and a [load balancer pool](../guida/svuotamento.md) that drains each batch, users never see a server being switched.
- **domain:** the target is the **domain controller** and `server_groups` is required. The deployment is added to the domain content repository and assigned to the groups; suspend, resume, reload and restart apply to all the servers of the groups (`suspend-servers`, `reload-servers`, `restart-servers` with `blocking`). Use one target per domain.

### `restart` in standalone mode

In standalone mode the restart is `shutdown(restart=true)`: the server exits with the code that asks `standalone.sh` to start it again. It works when the server is started by `standalone.sh` (directly or by the systemd service of the distribution); a server started in other ways does not come back, and the deploy fails after `wait_timeout`.

## Rollback

- **Release in service** (`plugin` locator): the active deployment with the application runtime-name, on the first target. A deployment not created by manent (released by hand) is recognized by name, so it can still be the target of a rollback.
- **Rollback:** on every target it reactivates the previous deployment, still present in the content repository, with the same composite operation and the same `suspend`, `reload` and `restart`. It uploads nothing, so it does not depend on the artifact source.

The `war` and `ear` artifact types allow the rollback (`rollback.artifact_types.war.allowed: true` in the defaults).

## Dry-run

In [dry-run](../guida/dry-run.md), for every target: authentication, mode, server groups, `server-state` (`running` in standalone mode) and the active version. It changes nothing.

## Result

```json
{
  "plugin": "jboss",
  "targets": [
    {
      "host": "eap1.example.com",
      "status": "deployed",
      "details": {
        "mode": "standalone",
        "deployment": "shop-1.4.0.war",
        "runtime_name": "shop.war",
        "previous": "shop-1.3.0.war",
        "previous_version": "1.3.0",
        "suspended": "true"
      }
    }
  ]
}
```
