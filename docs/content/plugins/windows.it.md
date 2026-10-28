# windows

Il plugin `windows` rilascia su host Windows **applicazioni web su IIS**, **servizi Windows**, **attività pianificate** e **pacchetti MSI**. Si collega con **WinRM** (default) o con il server **OpenSSH** di Windows ed esegue script PowerShell: sul target non serve un agente.

Versioni supportate: Windows Server 2016 e successivi con Windows PowerShell 5.1 (`shell: powershell`, default) o PowerShell 7 (`shell: pwsh`). Per IIS serve il modulo `WebAdministration` (installato con il ruolo Web Server); per le attività pianificate il modulo `ScheduledTasks`, già presente. Il pool `iis-arr` richiede Application Request Routing 3.0 sul server ARR, il pool `nlb` gli strumenti NLB (`RSAT-NLB`) sull'host di gestione.

## Come cambia la versione

L'artefatto è un archivio `.zip`, o un pacchetto `.msi` con `mode: msi`. Ogni versione viene estratta in `<releases_dir>\<versione>` (un pacchetto vi resta come `package.msi`). La release in esercizio è:

- il **`physicalPath`** dell'applicazione IIS;
- il **`binPath`** del servizio;
- l'**azione** dell'attività pianificata;
- per MSI, il prodotto installato da quella directory.

Il cambio è quindi una modifica di configurazione (o un'installazione), e il rollback torna a una directory già presente. Per ogni target (uno dopo l'altro, o [`parallel`](../guida/ondate.md#target-in-parallelo) alla volta):

1. legge la release in esercizio, crea `releases_dir` se manca e ne verifica la scrittura;
2. se la versione non è già presente con lo stesso sha256, copia l'artefatto, ne verifica lo sha256 sul target, lo estrae (o conserva il pacchetto) in una directory temporanea e la sposta in `<versione>`. Un retry o una recovery non ricopiano una release già presente;
3. mette in esercizio la release:
    - **IIS** (`mode: iis`): con `stop_app_pool: true` ferma l'application pool e attende `Stopped`, imposta il `physicalPath` dell'applicazione (`Set-WebConfigurationProperty` su `applicationHost.config`), poi avvia l'application pool (o lo ricicla, se non era fermo) e attende `Started`;
    - **servizio** (`mode: service`): ferma il servizio, imposta `binPath` sull'eseguibile della release con `sc.exe config`, lo avvia e attende `Running` entro `timeout`. Un servizio che non parte fa fallire il deploy;
    - **attività pianificata** (`mode: task`): se un'istanza è in esecuzione, attende che finisca entro `timeout`, oppure la ferma con `stop_task: true`. Poi imposta l'azione dell'attività sull'eseguibile della release (`Set-ScheduledTask`, con gli argomenti e la release come cartella di lavoro). Con `run_now: true` avvia l'attività;
    - **MSI** (`mode: msi`): installa `package.msi` con `msiexec /i … /qn /norestart`, un log dettagliato nella directory della release e le `msi_properties`. L'aggiornamento della versione precedente è quello del pacchetto stesso (MajorUpgrade). Il prodotto installato si trova con `product_name` in `Win32_InstalledWin32Program`, che, diversamente da `Win32_Product`, non avvia una riparazione dei pacchetti installati. Product code e versione vengono registrati con la release. Il codice di uscita 3010 (riavvio richiesto) è un successo con un avviso: manent non riavvia mai l'host. I pacchetti di solito rifiutano un downgrade, quindi tornare a una release già installata disinstalla prima il prodotto attuale; `uninstall_previous: true` lo fa sempre;
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
| `mode` | `iis` | `iis`, `service`, `task` o `msi`. |
| `releases_dir` | | Directory delle release sul target, ad esempio `C:\apps\shop\releases` (obbligatoria). |
| `site` | | `iis`: nome del sito (ad esempio `Default Web Site`). |
| `app` | `/` | `iis`: percorso dell'applicazione; `/` è il sito stesso. |
| `app_pool` | | `iis`: application pool da riavviare dopo il cambio. |
| `stop_app_pool` | `false` | `iis`: ferma l'application pool durante il cambio (le richieste ricevono `503`), invece di riciclarlo dopo. |
| `service` | | `service`: nome del servizio (che deve esistere). |
| `binary` | | `service` e `task`: eseguibile relativo alla release, ad esempio `bin\Shop.Worker.exe`. |
| `arguments` | | `service` e `task`: argomenti dell'eseguibile. |
| `task` | | `task`: nome dell'attività pianificata (che deve esistere). |
| `task_path` | `\` | `task`: cartella dell'attività, ad esempio `\Shop\`. |
| `stop_task` | `false` | `task`: ferma un'istanza in esecuzione invece di attendere che finisca. |
| `run_now` | `false` | `task`: avvia l'attività dopo il cambio. |
| `task_user`, `task_password_secret` | | `task`: account di un'attività eseguita con una password memorizzata; `Set-ScheduledTask` la richiede di nuovo. Le attività eseguite come SYSTEM, account di servizio o gMSA non ne hanno bisogno. |
| `product_name` | | `msi`: nome del prodotto installato, come in Programmi e funzionalità (obbligatorio). |
| `msi_properties` | | `msi`: proprietà pubbliche, `NOME=valore` separate da spazi (valori senza spazi né apici). |
| `uninstall_previous` | `false` | `msi`: disinstalla il prodotto installato prima di ogni installazione (pacchetti senza MajorUpgrade). |
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

Il descrittore usa il tipo `windows` (con `mode: msi` la sorgente è il `.msi`):

```yaml
artifact:
  type: windows
  name: shop
  version: "1.4.0"
  source: https://nexus.example.com/repository/releases/shop/1.4.0/shop-1.4.0.zip
```

## Rollback e stato reale

- **Rollback:** il checkpoint legge la release in esercizio dal `physicalPath`, dal `binPath`, dall'azione dell'attività o dal prodotto installato. La compensazione rimette in esercizio quella directory, che `keep_releases` conserva sempre. Per MSI disinstalla il prodotto e reinstalla il pacchetto conservato. Il tipo `windows` ammette il rollback nei default di manent.
- **MSI:** la release in esercizio è quella registrata all'installazione, purché il suo prodotto sia ancora installato con la stessa versione. Un prodotto rimosso o sostituito a mano non è una release di manent, e la riconciliazione segnala il target.
- **Release in esercizio:** un percorso fuori da `releases_dir` (ad esempio la cartella iniziale `C:\inetpub\wwwroot`) non è una release di manent. La [riconciliazione](../guida/governance.md#stato-reale) legge ogni target e segnala chi è stato spostato a mano su un'altra directory.
- **Dry-run:** si collega a ogni target, verifica PowerShell, `releases_dir`, sito o servizio e riporta la release in esercizio, senza cambiare nulla.

## Svuotamento con ARR e NLB { #svuotamento-con-arr-e-nlb }

Due [pool di traffico](../guida/svuotamento.md) tolgono gli host dal bilanciatore durante un [rilascio a ondate](../guida/ondate.md), un lotto alla volta. Eseguono PowerShell su un **host di gestione**, con gli stessi transport e parametri del plugin:

- **`iis-arr`:** una web farm di IIS **Application Request Routing**. Lo svuotamento porta lo stato ARR del server a *Drain*: le richieste in corso e le sessioni sticky continuano, le nuove vanno agli altri server. manent attende che le richieste in corso del server arrivino a zero. Il rientro lo riporta ad *Available*, e il server è sano quando ARR lo vede sano. Usa `Microsoft.Web.Administration` sul server ARR.
- **`nlb`:** un cluster **Network Load Balancing**. Lo svuotamento è `Stop-NlbClusterNode -Drain`, che attende le connessioni fino a `drain_timeout` e poi ferma il nodo. Il rientro è `Start-NlbClusterNode`; il nodo è sano quando è *Converged*.

I membri sono gli indirizzi dei server della farm (`iis-arr`) o i nomi dei nodi (`nlb`); di default l'host del target (`traffic.pool.member`).

```yaml title="envconfig/shop/shop/prod.yml"
rollout:
  waves:
    - {name: all, select: "region=eu-1", by: az}
  maxUnavailable: "1"
traffic:
  pool:
    provider: iis-arr          # oppure nlb
    min_healthy: "1"
    settings:
      host: arr1.contoso.local # host di gestione: il server ARR, o un nodo del cluster
      user: CONTOSO\svc-deploy
      farm: shop-farm          # iis-arr
      password_secret: svc-deploy
      # transport, shell, winrm_*, ssh_key_secret, known_hosts, winrm_ca_file: come il plugin
      # drain_timeout: 10m     # nlb
```

## Preparare il target

- **WinRM:** `winrm quickconfig -transport:https` con un certificato del server; NTLM è il default di un dominio. Con `basic` serve `winrm set winrm/config/service/auth @{Basic="true"}`, solo su HTTPS.
- **OpenSSH:** la funzionalità opzionale *OpenSSH Server* di Windows, con la chiave pubblica di manent in `authorized_keys` dell'account. La shell di default può restare `cmd.exe`.
- **Permessi:** l'account deve poter scrivere in `releases_dir` e, a seconda della modalità:
    - modificare la configurazione di IIS;
    - fermare, avviare e configurare il servizio;
    - modificare l'attività pianificata;
    - installare pacchetti (amministratore locale).

  Per i pool, l'account deve amministrare ARR, o essere amministratore del cluster NLB.
