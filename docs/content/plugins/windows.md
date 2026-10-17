# windows

The `windows` plugin deploys **web applications on IIS** and **Windows services** to Windows hosts. It connects with **WinRM** (default) or with the Windows **OpenSSH** server and runs PowerShell scripts: no agent is needed on the target.

Supported versions: Windows Server 2016 and later with Windows PowerShell 5.1 (`shell: powershell`, default) or PowerShell 7 (`shell: pwsh`). IIS requires the `WebAdministration` module (installed with the Web Server role).

## How the version changes

The artifact is a `.zip` archive. Every version is extracted into `<releases_dir>\<version>`; the release in service is the **`physicalPath`** of the IIS application or the **`binPath`** of the service. The switch is therefore a configuration change, and the rollback goes back to a directory already there. For each target, in sequence:

1. reads the release in service, creates `releases_dir` if missing and checks it is writable;
2. if the version is not already extracted with the same sha256, copies the archive, checks its sha256 on the target, extracts it into a temporary directory and moves it to `<version>`. A retry or a recovery does not copy a release already there;
3. puts the release in service:
    - **IIS** (`mode: iis`): with `stop_app_pool: true` stops the application pool and waits for `Stopped`, sets the `physicalPath` of the application (`Set-WebConfigurationProperty` on `applicationHost.config`), then starts the application pool (or recycles it, if it was not stopped) and waits for `Started`;
    - **service** (`mode: service`): stops the service, sets `binPath` to the executable of the release with `sc.exe config`, starts it and waits for `Running` within `timeout`. A service that does not start fails the deploy;
4. runs `post_script`, if any, with `MANENT_VERSION` and `MANENT_RELEASE_DIR` in the environment;
5. removes the releases beyond `keep_releases`, never the one in service nor the previous one.

The copy goes on the stdin of the PowerShell process as base64 text, so it works the same over WinRM and SSH and depends neither on SMB nor on SFTP. The command line stays under the `cmd.exe` limit whatever the script: the script itself arrives on stdin.

## Configuration

```yaml title="manent.yml"
plugins:
  windows:
    artifact_types: [windows]                  # default
    winrm_ca_file: /etc/manent/winrm-ca.pem     # CA of the WinRM HTTPS certificates, if private
    known_hosts: /etc/manent/known_hosts        # ssh transport, as remote-copy
```

## Deploy parameters

| Parameter | Default | Description |
|---|---|---|
| `mode` | `iis` | `iis` or `service`. |
| `releases_dir` | | Directory of the releases on the target, for example `C:\apps\shop\releases` (required). |
| `site` | | `iis`: site name (for example `Default Web Site`). |
| `app` | `/` | `iis`: application path; `/` is the site itself. |
| `app_pool` | | `iis`: application pool to restart after the switch. |
| `stop_app_pool` | `false` | `iis`: stops the application pool during the switch (requests get `503`), instead of recycling it afterwards. |
| `service` | | `service`: service name (it must exist). |
| `binary` | | `service`: executable relative to the release, for example `bin\Shop.Worker.exe`. |
| `arguments` | | `service`: arguments of the executable in the `binPath`. |
| `timeout` | `60s` | Wait for the application pool or the service. |
| `keep_releases` | `5` | Releases kept on the target (at least 2). |
| `post_script` | | PowerShell run after the switch. |
| `transport` | `winrm` | `winrm` or `ssh` (Windows OpenSSH). |
| `shell` | `powershell` | `powershell` (Windows PowerShell 5.1) or `pwsh` (PowerShell 7). |
| `winrm_https` | `true` | HTTPS (port `5986`); `false` for HTTP (port `5985`). The target `port` replaces the port. |
| `winrm_auth` | `ntlm` | `ntlm` or `basic` (only with HTTPS: the password would travel in clear). |
| `winrm_insecure_skip_verify` | `false` | Does not verify the WinRM certificate: development only. |
| `password_secret` | | Name of the secret with the password: required with WinRM, optional with SSH. |
| `ssh_key_secret` | | Name of the secret with the SSH private key. |

The target `user` is the service account (for example `CONTOSO\svc-deploy`), which must be able to administer IIS or the service. `artifact.version` is required and becomes the name of the release directory.

```yaml title="envconfig/shop/prod.yml"
plugin: windows
targets:
  - {host: web1.contoso.local, user: 'CONTOSO\svc-deploy', labels: {region: eu-1, az: eu-1a}}
  - {host: web2.contoso.local, user: 'CONTOSO\svc-deploy', labels: {region: eu-1, az: eu-1b}}
params:
  mode: iis
  site: Default Web Site
  app: /shop
  app_pool: shop
  releases_dir: D:\apps\shop\releases
  password_secret: svc-deploy
secrets:
  - {name: svc-deploy, ref: "vault:secret/data/deploy/svc-deploy#password"}
```

```yaml title="envconfig/shop-worker/prod.yml"
plugin: windows
targets:
  - {host: app1.contoso.local, user: svc-deploy}
params:
  mode: service
  transport: ssh
  ssh_key_secret: deploy-key
  service: ShopWorker
  binary: bin\Shop.Worker.exe
  arguments: --environment Production
  releases_dir: D:\apps\shop-worker\releases
```

The descriptor uses the `windows` type:

```yaml
artifact:
  type: windows
  name: shop
  version: "1.4.0"
  source: https://nexus.example.com/repository/releases/shop/1.4.0/shop-1.4.0.zip
```

## Rollback and real state

- **Rollback:** the checkpoint reads the release in service from the `physicalPath` or the `binPath`. The compensation puts that directory back in service, and `keep_releases` always keeps it. The `windows` type allows rollback in the manent defaults.
- **Release in service:** a path outside `releases_dir` (for example the initial `C:\inetpub\wwwroot` folder) is not a manent release. The [reconciliation](../guida/governance.md#stato-reale) reads every target and reports those moved by hand to another directory.
- **Dry-run:** connects to every target, checks PowerShell, `releases_dir`, site or service and reports the release in service, without changing anything.

## Preparing the target

- **WinRM:** `winrm quickconfig -transport:https` with a server certificate; NTLM is the default in a domain. With `basic` you need `winrm set winrm/config/service/auth @{Basic="true"}`, over HTTPS only.
- **OpenSSH:** the optional Windows feature *OpenSSH Server*, with the manent public key in the `authorized_keys` of the account. The default shell can stay `cmd.exe`.
- **Permissions:** the account must be able to write to `releases_dir`, change the IIS configuration and stop, start and configure the service.
