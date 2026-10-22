# Repository Git

La [configurazione di ambiente](../guida/configurazione-ambiente.md) può stare in un repository Git. manent legge ogni file a un commit e ne registra lo SHA sul deploy. Una release si ricostruisce così esattamente da `sha256` dell'artefatto più `repository@sha:path`. Pull request, CODEOWNERS e commit firmati governano ogni modifica della configurazione.

!!! abstract "Cosa serve"
    Un server Git raggiungibile da API e worker (HTTPS, SSH o `git://`) · un token o una deploy key in sola lettura · se servono, le chiavi che firmano i commit

## 1. Struttura del repository

| Percorso | Contenuto |
|---|---|
| `<tenant>/<applicazione>/<artefatto>/<ambiente>.yml` | Configurazione di ambiente, stesso formato degli altri provider. |

Il modello si può cambiare (`envconfig.git.path`). Non arriva mai dalla richiesta: un descrittore non può puntare al file di un'altra applicazione.

!!! danger "Riferimenti, mai valori"
    La configurazione viene salvata nello storico del deploy, come con Consul. Deve contenere solo riferimenti ai segreti (`vault:…`, `file:…`), mai password o chiavi.

## 2. Configurare manent

```yaml title="manent.yml"
envconfig:
  provider: git
  required: true
  git:
    repository: https://git.example.com/platform/envconfig.git
    ref: refs/heads/main                      # ref di default
    path: "{tenant}/{application}/{artifact}/{environment}.yml"
    username: x-access-token                  # HTTPS
    password_ref: vault:secret/manent/git#token
    # ssh_key_ref: file:/run/secrets/git-key  # ssh:// al posto di HTTPS
    # known_hosts: /etc/manent/known_hosts    # obbligatorio con ssh_key_ref
    # ca_file: /etc/manent/git-ca.pem
    allowed_refs: [refs/tags/env-*]           # altri ref che un descrittore può indicare
    require_signed: true
    signers:
      ssh_allowed_signers: /etc/manent/allowed_signers
      # openpgp_keyring: /etc/manent/git-keys.asc
    max_age: 0                                # età massima di un commit (0 = nessuna)
    cache_dir: /var/lib/manent/git            # default <workdir>/.git-cache
    ref_ttl: 30s                              # per quanto si riusa la risoluzione di un ref
```

| Chiave | Significato |
|---|---|
| `repository` | URL (`https://`, `ssh://`, `git://`, `file://`) o percorso locale. Solo da `manent.yml`. |
| `ref` | Il ref letto quando il descrittore non ne indica uno. Sempre ammesso. |
| `allowed_refs` | Altri ref (pattern con `*`) che un descrittore può indicare. Uno SHA richiesto deve essere raggiungibile dal ref di default o da uno di questi. |
| `password_ref`, `ssh_key_ref` | Riferimenti ai segreti, risolti a ogni fetch e tenuti solo in memoria. |
| `require_signed`, `signers` | I commit senza una firma OpenPGP o SSH valida (namespace `git`) di queste chiavi sono rifiutati. I file vengono riletti a ogni verifica: una chiave ruotata non richiede un riavvio. |
| `max_age` | Rifiuta i commit più vecchi di così. Il rollback manuale ne è esente: torna a una revisione registrata. |
| `cache_dir` | Copia bare locale, una per repository, su ogni worker. Non serve uno storage condiviso. |
| `ref_ttl` | Entro questo tempo un ref si risolve senza un nuovo fetch. Uno SHA richiesto che manca in locale fa sempre un fetch. |

manent legge Git con una libreria inclusa nel binario: l'immagine del server non ha bisogno dell'eseguibile `git`.

## 3. Quale revisione usa un deploy

La revisione si risolve nello step `config`, all'avvio del deploy. Un deploy pianificato legge il ref al momento dell'esecuzione, come con gli altri provider. Da lì in poi retry e recovery riusano lo SHA registrato, anche se il branch va avanti.

| Deploy | Revisione |
|---|---|
| Deploy | Il ref di default, oppure `config.ref` del descrittore se `descriptor_policy.config_ref` lo ammette. |
| Promozione | Il ref di default dell'ambiente di destinazione; `--config-ref` (o `config_ref` nell'API) ne sceglie un altro. |
| Rollback manuale | Lo SHA registrato dal deploy a cui si torna, qualunque cosa dica la policy. |
| Riconciliazione | Lo SHA della release in esercizio. |

L'output dello step `config` riporta `source` (`git:<repository>//<percorso>`), `revision` (lo SHA) e `ref`. Lo SHA è anche in `Status.config_revision`, nell'inventario, nella pagina del deploy dell'interfaccia web e negli esiti della CI (`config_revision`).

## 4. Far scegliere la revisione al descrittore

Di default il descrittore non può scegliere (`config_ref: none`). Per consentirlo:

```yaml title="manent.yml"
descriptor_policy:
  config_ref: sha        # none | ref (solo ref) | sha (ref e SHA di commit)
```

```yaml title="deploy.yml"
config:
  ref: refs/tags/env-42  # o lo SHA di un commit
```

```sh
manent deploy -f deploy.yml --config-ref 4f9c2e1a7b3d...     # es. dalla CI, il commit testato con l'artefatto
manent promote -a shop --from test --to prod --config-ref refs/tags/env-42
```

Un ref fuori da `ref` e `allowed_refs`, o uno SHA non raggiungibile da essi, è rifiutato all'invio (422), prima dell'accodamento. Lo stesso vale per un file assente alla revisione richiesta, una firma non valida e un commit più vecchio di `max_age`. Un server irraggiungibile è un errore temporaneo: il worker riprova.

## 5. Sicurezza

| Rischio | Mitigazione |
|---|---|
| Una revisione vecchia scelta per saltare un vincolo aggiunto dopo | `config_ref: none` di default; raggiungibilità dai ref ammessi; `max_age` e firme facoltativi. |
| Il file di un'altra applicazione | Il percorso arriva solo dal modello. |
| Un branch non protetto | Si leggono solo `ref` e `allowed_refs`. |
| Un repository arbitrario (SSRF) | Il repository arriva solo da `manent.yml`. |
| Credenziali esposte | Riferimenti ai segreti, mai su disco né negli output degli step. |
| Un branch ammesso cancellato | Il fetch rimuove i ref locali spariti dal server: i suoi commit smettono di essere raggiungibili. |
