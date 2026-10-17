# windows

Il plugin `windows` rilascia su host Windows **applicazioni web su IIS** e **servizi Windows**. Si collega con **WinRM** (default) o con il server **OpenSSH** di Windows ed esegue script PowerShell: sul target non serve un agente.

Versioni supportate: Windows Server 2016 e successivi con Windows PowerShell 5.1 (`shell: powershell`, default) o PowerShell 7 (`shell: pwsh`). Per IIS serve il modulo `WebAdministration` (installato con il ruolo Web Server).

## Come cambia la versione

L'artefatto è un archivio `.zip`. Ogni versione viene estratta in `<releases_dir>\<versione>`; la release in esercizio è il **`physicalPath`** dell'applicazione IIS o il **`binPath`** del servizio. Il cambio è quindi una modifica di configurazione, e il rollback torna a una directory già presente. Per ogni target, in sequenza:

1. legge la release in esercizio, crea `releases_dir` se manca e ne verifica la scrittura;
2. se la versione non è già estratta con lo stesso sha256, copia l'archivio, ne verifica lo sha256 sul target, lo estrae in una directory temporanea e la sposta in `<versione>`. Un retry o una recovery non ricopiano una release già presente;
3. mette in esercizio la release:
    - **IIS** (`mode: iis`): con `stop_app_pool: true` ferma l'application pool e attende `Stopped`, imposta il `physicalPath` dell'applicazione (`Set-WebConfigurationProperty` su `applicationHost.config`), poi avvia l'application pool (o lo ricicla, se non era fermo) e attende `Started`;
    - **servizio** (`mode: service`): ferma il servizio, imposta `binPath` sull'eseguibile della release con `sc.exe config`, lo avvia e attende `Running` entro `timeout`. Un servizio che non parte fa fallire il deploy;
4. esegue `post_script`, se c'è, con `MANENT_VERSION` e `MANENT_RELEASE_DIR` nell'ambiente;
5. rimuove le release oltre `keep_releases`, mai quella in esercizio né la precedente.

La copia passa sullo stdin del processo PowerShell come testo base64, quindi funziona allo stesso modo su WinRM e su SSH e non dipende da SMB né da SFTP. La riga di comando resta sotto il limite di `cmd.exe` qualunque sia lo script: lo script stesso arriva sullo stdin.

## Configurazione

```yaml title="manent.yml"
plugins:
  windows:
    artifact_types: [windows]                  # default
    winrm_ca_file: /etc/manent/winrm-ca.pem     # CA dei certificati WinRM HTTPS, se privata
    known_hosts: /etc/manent/known_hosts        # transport ssh, come remote-copy
```

## Parametri del deploy

| Parametro | Default | Descrizione |
|---|---|---|
| `mode` | `iis` | `iis` o `service`. |
| `releases_dir` | | Directory delle release sul target, ad esempio `C:\apps\shop\releases` (obbligatoria). |
| `site` | | `iis`: nome del sito (ad esempio `Default Web Site`). |
| `app` | `/` | `iis`: percorso dell'applicazione; `/` è il sito stesso. |
| `app_pool` | | `iis`: application pool da riavviare dopo il cambio. |
| `stop_app_pool` | `false` | `iis`: ferma l'application pool durante il cambio (le richieste ricevono `503`), invece di riciclarlo dopo. |
| `service` | | `service`: nome del servizio (che deve esistere). |
| `binary` | | `service`: eseguibile relativo alla release, ad esempio `bin\Shop.Worker.exe`. |
| `arguments` | | `service`: argomenti dell'eseguibile nel `binPath`. |
| `timeout` | `60s` | Attesa dell'application pool o del servizio. |
| `keep_releases` | `5` | Release conservate sul target (almeno 2). |
| `post_script` | | PowerShell eseguito dopo il cambio. |
| `transport` | `winrm` | `winrm` o `ssh` (OpenSSH di Windows). |
| `shell` | `powershell` | `powershell` (Windows PowerShell 5.1) o `pwsh` (PowerShell 7). |
| `winrm_https` | `true` | HTTPS (porta `5986`); `false` per HTTP (porta `5985`). La `port` del target sostituisce la porta. |
| `winrm_auth` | `ntlm` | `ntlm` o `basic` (solo con HTTPS: la password viaggerebbe in chiaro). |
| `winrm_insecure_skip_verify` | `false` | Non verifica il certificato WinRM: solo in sviluppo. |
| `password_secret` | | Nome del segreto con la password: obbligatorio con WinRM, facoltativo con SSH. |
| `ssh_key_secret` | | Nome del segreto con la chiave privata SSH. |

Il `user` del target è l'account di servizio (ad esempio `CONTOSO\svc-deploy`), che deve poter amministrare IIS o il servizio. `artifact.version` è obbligatoria e diventa il nome della directory della release.

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

Il descrittore usa il tipo `windows`:

```yaml
artifact:
  type: windows
  name: shop
  version: "1.4.0"
  source: https://nexus.example.com/repository/releases/shop/1.4.0/shop-1.4.0.zip
```

## Rollback e stato reale

- **Rollback:** il checkpoint legge la release in esercizio dal `physicalPath` o dal `binPath`. La compensazione rimette in esercizio quella directory, che `keep_releases` conserva sempre. Il tipo `windows` ammette il rollback nei default di manent.
- **Release in esercizio:** un percorso fuori da `releases_dir` (ad esempio la cartella iniziale `C:\inetpub\wwwroot`) non è una release di manent. La [riconciliazione](../guida/governance.md#stato-reale) legge ogni target e segnala chi è stato spostato a mano su un'altra directory.
- **Dry-run:** si collega a ogni target, verifica PowerShell, `releases_dir`, sito o servizio e riporta la release in esercizio, senza cambiare nulla.

## Preparare il target

- **WinRM:** `winrm quickconfig -transport:https` con un certificato del server; NTLM è il default di un dominio. Con `basic` serve `winrm set winrm/config/service/auth @{Basic="true"}`, solo su HTTPS.
- **OpenSSH:** la funzionalità opzionale *OpenSSH Server* di Windows, con la chiave pubblica di manent in `authorized_keys` dell'account. La shell di default può restare `cmd.exe`.
- **Permessi:** l'account deve poter scrivere in `releases_dir`, modificare la configurazione di IIS e fermare, avviare e configurare il servizio.
