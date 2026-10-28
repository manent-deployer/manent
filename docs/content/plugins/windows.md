# windows

The `windows` plugin deploys **web applications on IIS**, **Windows services**, **scheduled tasks** and **MSI packages** to Windows hosts. It connects with **WinRM** (default) or with the Windows **OpenSSH** server and runs PowerShell scripts: no agent is needed on the target.

Supported versions: Windows Server 2016 and later with Windows PowerShell 5.1 (`shell: powershell`, default) or PowerShell 7 (`shell: pwsh`). IIS requires the `WebAdministration` module (installed with the Web Server role); scheduled tasks the built-in `ScheduledTasks` module. The `iis-arr` pool requires Application Request Routing 3.0 on the ARR server, the `nlb` pool the NLB tools (`RSAT-NLB`) on the management host.

## How the version changes

The artifact is a `.zip` archive, or a `.msi` package with `mode: msi`. Every version is extracted into `<releases_dir>\<version>` (a package is kept there as `package.msi`). The release in service is:

- the **`physicalPath`** of the IIS application;
- the **`binPath`** of the service;
- the **action** of the scheduled task;
- for MSI, the product installed from that directory.

The switch is therefore a configuration change (or an installation), and the rollback goes back to a directory already there. For each target (one after the other, or [`parallel`](../guida/ondate.md#parallel-targets) at a time):

1. reads the release in service, creates `releases_dir` if missing and checks it is writable;
2. if the version is not already there with the same sha256, copies the artifact, checks its sha256 on the target, extracts it (or keeps the package) into a temporary directory and moves it to `<version>`. A retry or a recovery does not copy a release already there;
3. puts the release in service:
    - **IIS** (`mode: iis`): with `stop_app_pool: true` stops the application pool and waits for `Stopped`, sets the `physicalPath` of the application (`Set-WebConfigurationProperty` on `applicationHost.config`), then starts the application pool (or recycles it, if it was not stopped) and waits for `Started`;
    - **service** (`mode: service`): stops the service, sets `binPath` to the executable of the release with `sc.exe config`, starts it and waits for `Running` within `timeout`. A service that does not start fails the deploy;
    - **scheduled task** (`mode: task`): if an instance is running, waits for it to end within `timeout`, or stops it with `stop_task: true`. Then sets the action of the task to the executable of the release (`Set-ScheduledTask`, with the arguments and the release as working directory). With `run_now: true` it starts the task;
    - **MSI** (`mode: msi`): installs `package.msi` with `msiexec /i … /qn /norestart`, a verbose log in the release directory and the `msi_properties`. The upgrade of the previous version is the package's own (MajorUpgrade). The installed product is found by `product_name` in `Win32_InstalledWin32Program`, which, unlike `Win32_Product`, does not start a repair of the installed packages. Its product code and version are recorded with the release. Exit code 3010 (restart required) is a success with a warning: manent never restarts the host. Packages usually refuse a downgrade, so going back to a release installed before uninstalls the current product first; `uninstall_previous: true` always does;
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
| `mode` | `iis` | `iis`, `service`, `task` or `msi`. |
| `releases_dir` | | Directory of the releases on the target, for example `C:\apps\shop\releases` (required). |
| `site` | | `iis`: site name (for example `Default Web Site`). |
| `app` | `/` | `iis`: application path; `/` is the site itself. |
| `app_pool` | | `iis`: application pool to restart after the switch. |
| `stop_app_pool` | `false` | `iis`: stops the application pool during the switch (requests get `503`), instead of recycling it afterwards. |
| `service` | | `service`: service name (it must exist). |
| `binary` | | `service` and `task`: executable relative to the release, for example `bin\Shop.Worker.exe`. |
| `arguments` | | `service` and `task`: arguments of the executable. |
| `task` | | `task`: name of the scheduled task (it must exist). |
| `task_path` | `\` | `task`: folder of the task, for example `\Shop\`. |
| `stop_task` | `false` | `task`: stops a running instance instead of waiting for it to end. |
| `run_now` | `false` | `task`: starts the task after the switch. |
| `task_user`, `task_password_secret` | | `task`: account of a task that runs with a stored password; `Set-ScheduledTask` needs it again. Tasks running as SYSTEM, a service account or a gMSA do not. |
| `product_name` | | `msi`: name of the installed product, as in Programs and Features (required). |
| `msi_properties` | | `msi`: public properties, `NAME=value` separated by spaces (values without spaces or quotes). |
| `uninstall_previous` | `false` | `msi`: uninstalls the installed product before every installation (packages without MajorUpgrade). |
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

```yaml title="envconfig/shop/shop/prod.yml"
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

```yaml title="envconfig/shop-export/prod.yml"
plugin: windows
targets:
  - {host: batch1.contoso.local, user: 'CONTOSO\svc-deploy'}
params:
  mode: task
  task: Export
  task_path: \Shop\
  binary: bin\Shop.Export.exe
  arguments: --all
  releases_dir: D:\apps\shop-export\releases
  password_secret: svc-deploy
```

```yaml title="envconfig/shop-agent/prod.yml"
plugin: windows
targets:
  - {host: app1.contoso.local, user: 'CONTOSO\svc-deploy'}
params:
  mode: msi
  product_name: Shop Agent
  msi_properties: PORT=7070 ENVIRONMENT=prod
  releases_dir: D:\packages\shop-agent
  password_secret: svc-deploy
```

The descriptor uses the `windows` type (with `mode: msi` the source is the `.msi`):

```yaml
artifact:
  type: windows
  name: shop
  version: "1.4.0"
  source: https://nexus.example.com/repository/releases/shop/1.4.0/shop-1.4.0.zip
```

## Rollback and real state

- **Rollback:** the checkpoint reads the release in service from the `physicalPath`, the `binPath`, the action of the task or the installed product. The compensation puts that directory back in service, and `keep_releases` always keeps it. For MSI it uninstalls the product and reinstalls the kept package. The `windows` type allows rollback in the manent defaults.
- **MSI:** the release in service is the one recorded at installation, as long as its product is still installed with the same version. A product removed or replaced by hand is no release of manent, and the reconciliation reports the target.
- **Release in service:** a path outside `releases_dir` (for example the initial `C:\inetpub\wwwroot` folder) is not a manent release. The [reconciliation](../guida/governance.md#stato-reale) reads every target and reports those moved by hand to another directory.
- **Dry-run:** connects to every target, checks PowerShell, `releases_dir`, site or service and reports the release in service, without changing anything.

## Draining with ARR and NLB { #svuotamento-con-arr-e-nlb }

Two [traffic pools](../guida/svuotamento.md) take the hosts out of the load balancer during a [wave rollout](../guida/ondate.md), one batch at a time. They run PowerShell on a **management host**, with the same transports and parameters as the plugin:

- **`iis-arr`:** a web farm of IIS **Application Request Routing**. Draining sets the ARR state of the server to *Drain*: requests in progress and sticky sessions continue, new ones go to the other servers. manent waits for the current requests of the server to reach zero. Restoring sets it back to *Available*, and the server is healthy when ARR sees it healthy. It uses `Microsoft.Web.Administration` on the ARR server.
- **`nlb`:** a **Network Load Balancing** cluster. Draining is `Stop-NlbClusterNode -Drain`, which waits for the connections up to `drain_timeout` and then stops the node. Restoring is `Start-NlbClusterNode`; the node is healthy when it is *Converged*.

Members are the server addresses of the farm (`iis-arr`) or the node names (`nlb`); by default the host of the target (`traffic.pool.member`).

```yaml title="envconfig/shop/shop/prod.yml"
rollout:
  waves:
    - {name: all, select: "region=eu-1", by: az}
  maxUnavailable: "1"
traffic:
  pool:
    provider: iis-arr          # or nlb
    min_healthy: "1"
    settings:
      host: arr1.contoso.local # management host: the ARR server, or a cluster node
      user: CONTOSO\svc-deploy
      farm: shop-farm          # iis-arr
      password_secret: svc-deploy
      # transport, shell, winrm_*, ssh_key_secret, known_hosts, winrm_ca_file: as the plugin
      # drain_timeout: 10m     # nlb
```

## Preparing the target

- **WinRM:** `winrm quickconfig -transport:https` with a server certificate; NTLM is the default in a domain. With `basic` you need `winrm set winrm/config/service/auth @{Basic="true"}`, over HTTPS only.
- **OpenSSH:** the optional Windows feature *OpenSSH Server*, with the manent public key in the `authorized_keys` of the account. The default shell can stay `cmd.exe`.
- **Permissions:** the account must be able to write to `releases_dir` and, depending on the mode:
    - change the IIS configuration;
    - stop, start and configure the service;
    - change the scheduled task;
    - install packages (local administrator).

  For the pools, the account must administer ARR, or be an NLB cluster administrator.
