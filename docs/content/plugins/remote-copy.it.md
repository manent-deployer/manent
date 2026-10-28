# remote-copy

Il plugin `remote-copy` copia l'artefatto su server remoti via SSH/SFTP, ne verifica l'integrità e può eseguire un comando dopo la copia. È compilato dentro manent ed è disponibile anche come binario esterno (`bin/manent-plugin-remote-copy`, con `make plugins`).

## Configurazione

```yaml title="manent.yml"
plugins:
  remote-copy:
    known_hosts: ~/.ssh/known_hosts      # default
    insecure_ignore_host_key: false      # true solo in sviluppo
    artifact_types: [application]        # default
```

| Chiave | Descrizione |
|---|---|
| `known_hosts` | File con le host key ammesse. Default `~/.ssh/known_hosts`. |
| `insecure_ignore_host_key` | Disattiva la verifica della host key. Solo per sviluppo. |
| `artifact_types` | Tipi di artefatto gestiti. Default `[application]`. |

## Parametri del deploy

Si indicano in `deploy.params`, nel descrittore o nella configurazione di ambiente.

| Parametro | Obbligatorio | Descrizione |
|---|---|---|
| `dest_dir` | sì | Directory remota di destinazione. Viene creata se non esiste. |
| `ssh_key_secret` | uno dei due | Nome del segreto con la chiave privata PEM. |
| `password_secret` | uno dei due | Nome del segreto con la password. |
| `post_command` | no | Comando eseguito su ogni target dopo la copia. |
| `verify_checksum` | no | `false` disattiva la verifica sha256 remota. |
| `layout` | no | `flat` (default): il file viene copiato in `dest_dir`. `releases`: una directory per versione e il symlink `current`; necessario per il [rollback](#rollback). |
| `keep_releases` | no | Con `layout: releases`, quante release conservare, compresa quella attiva (default 5, minimo 2). |
| `prepare_command` | no | Con `layout: releases`, comando eseguito nella nuova release **prima** dello switch (es. estrazione dell'archivio). |
| `rollback_command` | no | Comando eseguito su ogni target dopo il rollback. Default: `post_command`. |

Ogni target deve avere `user`; la porta di default è 22.

```yaml
secrets:
  - name: deploy-key
    ref: env:DEPLOY_SSH_KEY
deploy:
  plugin: remote-copy
  targets:
    - host: 192.168.1.10
      user: deploy
  params:
    dest_dir: /opt/myapp/releases
    ssh_key_secret: deploy-key
    post_command: "tar -xzf /opt/myapp/releases/myapp-1.2.3.tar.gz -C /opt/myapp/current"
```

## Funzionamento

Per ogni target (uno dopo l'altro, o [`parallel`](../guida/ondate.md#target-in-parallelo) alla volta):

1. apre la connessione SSH, con la chiave o la password lette dai segreti al momento della connessione;
2. crea `dest_dir` (`mkdir -p`);
3. carica l'artefatto su `<dest_dir>/<nome file>.part` e lo rinomina in modo atomico nel nome finale. Una copia interrotta non lascia mai un file parziale con il nome definitivo;
4. se l'artefatto ha uno sha256 e `verify_checksum` non è `false`, esegue `sha256sum` sul file remoto e lo confronta;
5. esegue `post_command`, se indicato.

Dopo il primo target che fallisce nessun altro target parte; quelli già avviati finiscono. Rieseguirlo sovrascrive il file remoto con lo stesso contenuto, quindi è idempotente. Anche `post_command` deve esserlo, perché un retry lo esegue di nuovo.

## Layout `releases`

Con `layout: releases` ogni versione ha la propria directory e il symlink `current` indica quella in esercizio:

```
/opt/myapp/
├── current -> releases/1.5.0
└── releases/
    ├── 1.3.0/
    ├── 1.4.0/
    │   └── myapp-1.4.0.tar.gz
    └── 1.5.0/
        └── myapp-1.5.0.tar.gz
```

Per ogni target:

1. legge la release corrente (`readlink current`);
2. carica l'artefatto in `releases/<artifact.version>/`, con la stessa copia atomica e verifica sha256 del layout `flat`;
3. esegue `prepare_command`, se indicato;
4. sposta `current` sulla nuova release con uno scambio atomico del symlink (`ln -sfn` su un link temporaneo e `mv -T`, che richiede GNU coreutils);
5. esegue `post_command`;
6. elimina le release più vecchie oltre `keep_releases`, senza toccare mai quella appena attivata e quella precedente. Un errore della pulizia viene registrato ma non fa fallire il deploy.

`artifact.version` è obbligatoria e diventa il nome della directory: sono ammessi lettere, cifre, `.`, `_`, `+` e `-`.

I comandi (`prepare_command`, `post_command`, `rollback_command`) ricevono queste variabili:

| Variabile | Valore |
|---|---|
| `MANENT_RELEASE_DIR` | Directory della release, ad esempio `/opt/myapp/releases/1.5.0`. |
| `MANENT_ARTIFACT` | Percorso dell'artefatto caricato. Vuota in `rollback_command`. |
| `MANENT_VERSION` | Versione della release. |
| `MANENT_CURRENT` | Percorso del symlink `current`. |

```yaml
deploy:
  plugin: remote-copy
  params:
    dest_dir: /opt/myapp
    ssh_key_secret: deploy-key
    layout: releases
    keep_releases: "5"
    prepare_command: 'tar -xzf "$MANENT_ARTIFACT" -C "$MANENT_RELEASE_DIR"'
    post_command: "sudo systemctl restart myapp"
```

!!! warning "Passaggio dal layout `flat`"
    Se `dest_dir/current` esiste già come directory reale, lo switch del symlink fallisce. Prima del primo deploy con `layout: releases` va rinominata o rimossa.

## Rollback

`remote-copy` supporta il [rollback](../guida/rollback.md) solo con `layout: releases`.

- **Release in esercizio** (locator `plugin`): la release puntata da `current`. I target senza `current`, ad esempio appena aggiunti, vengono ignorati. Se i target puntano a release diverse il deploy si ferma nel checkpoint, perché non sarebbe chiaro a quale tornare.
- **Rollback:** per ogni target verifica che `releases/<versione precedente>` esista, sposta `current` su di essa ed esegue `rollback_command` (o `post_command`). Prosegue anche se un target fallisce e riporta gli errori alla fine. Il target è `rolled_back` oppure `failed`. Un target dove la release precedente non esiste più fallisce.

Con il locator di un sistema aziendale, la versione indicata deve essere ancora presente sui target: `keep_releases` va dimensionato di conseguenza.

## Dry-run

In [dry-run](../guida/dry-run.md), per ogni target:

- si connette con le stesse credenziali;
- verifica, senza creare nulla, che `dest_dir` esista e sia scrivibile oppure che sia creabile nella prima directory esistente del percorso;
- se la verifica del checksum è attiva, controlla che `sha256sum` sia disponibile;
- **non** copia l'artefatto e **non** esegue `post_command` né `prepare_command`;
- con `layout: releases` riporta in `current` la release in esercizio.

Il dry-run controlla tutti i target e riporta lo stato di ciascuno (`ready` o `failed`).

## Risultato

```json
{
  "plugin": "remote-copy",
  "targets": [
    {
      "host": "192.168.1.10",
      "status": "deployed",
      "details": {"remote_path": "/opt/myapp/releases/myapp-1.2.3.tar.gz", "checksum": "verified", "post_command": "ok"},
      "duration_ms": 4210
    }
  ]
}
```

Lo `status` di un target è `deployed`, `failed` oppure `skipped` quando non è mai partito perché un altro target del lotto è fallito; `duration_ms` è il tempo speso su di esso.
