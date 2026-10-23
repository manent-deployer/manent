# Scrivere un plugin

Il deploy vero e proprio è delegato ai plugin. Un plugin dipende solo dal package pubblico `github.com/joeg-ita/manent/pkg/plugin` e può essere:

- **interno**: compilato dentro manent e registrato in `internal/app`;
- **esterno**: un binario separato che manent avvia e con cui comunica via gRPC. Vedi [Plugin esterni](esterni.md).

Il codice del plugin è lo stesso nei due casi: cambia solo come viene distribuito.

## Il contratto

```go
type Deployer interface {
	// Name restituisce il nome univoco del plugin (es. "remote-copy").
	Name() string
	// Supports indica se il plugin gestisce il tipo di artefatto indicato.
	Supports(artifactType string) bool
	// Validate verifica la richiesta prima del deploy, senza effetti collaterali.
	Validate(req *plugin.Request) error
	// Deploy esegue il deploy. Deve essere idempotente.
	Deploy(ctx context.Context, req *plugin.Request) (*plugin.Result, error)
}

// Factory costruisce un Deployer dalla sua configurazione in manent.yml.
type Factory func(cfg map[string]any, logger *slog.Logger) (plugin.Deployer, error)
```

### La richiesta

```go
type Request struct {
	DeploymentID string
	Tenant       string            // untenanted per le applicazioni che nessun tenant elenca
	Application  string
	Environment  string
	Artifact     Artifact          // Type, Name, Version, LocalPath, SHA256
	Targets      []Target          // Host, Port, User
	Params       map[string]string // deploy.params del descrittore effettivo
	Secrets      SecretResolver
}
```

`Tenant`, `Application`, `Artifact.Name` ed `Environment` identificano la release: un plugin che tiene stato per release (storico, marcatori sui target) deve usarli tutti e quattro come chiave, perché due [tenant](../guida/tenant.md) possono avere applicazioni con lo stesso nome e ogni artefatto di un'applicazione è una release a sé. `Tenant` vale `untenanted` per le applicazioni che nessun tenant elenca. `Artifact.LocalPath` è il percorso dell'artefatto già scaricato e verificato sul worker. `Params` sono i parametri del plugin, dopo la fusione con la [configurazione di ambiente](../guida/configurazione-ambiente.md).

### Il risultato

```go
type Result struct {
	Plugin  string
	Targets []TargetResult // Host, Status, Details
	Outputs map[string]string
}
```

`Status` vale `plugin.StatusDeployed` o `plugin.StatusFailed` (in dry-run `plugin.StatusReady`, nel rollback `plugin.StatusRolledBack`). `Details` raccoglie informazioni utili per ogni target, ad esempio il percorso remoto. Il risultato viene serializzato in JSON e salvato nell'output dello step `deploy`.

`Outputs` sono valori prodotti dal deploy che altri deploy possono importare, per esempio gli host delle VM create da un plugin di provisioning (una lista JSON) o l'indirizzo di un database. Sono salvati nell'output dello step `deploy` come `out.<chiave>` e diventano export solo se il descrittore o la configurazione di ambiente li dichiarano (`exports: {vms: {from: deploy.vms, format: targets}}`), verificati con un formato e un pattern: vedi [Export e import](../guida/catene.md#export-import). Chiavi: lettere, cifre, `_`, `-`, `.` (al più 64 chiavi, valori fino a 64 KiB). Come il resto del risultato sono persistiti: non metterci mai segreti. I plugin esterni li restituiscono nel campo `outputs` di `DeployResponse`.

## Regole

!!! danger "Idempotenza"
    `Deploy` può essere rieseguito: dai retry dello step (`max_retries`) e dalla recovery dopo un crash del worker. Eseguirlo due volte deve produrre lo stesso stato finale. Ad esempio, scrivere su un file temporaneo e poi rinominarlo, oppure verificare lo stato attuale prima di modificarlo.

- **Segreti:** si leggono con `req.Secrets.Secret(ctx, nome)` solo quando servono. Il nome è quello logico della sezione `secrets`, tipicamente passato tramite un parametro (es. `ssh_key_secret: deploy-key`). I segreti non vanno mai inseriti nel `Result` né nei log.
- **Validate:** controlla parametri e target senza contattare i target. Viene chiamato prima di ogni `Deploy` e `DryRun`.
- **Contesto:** `ctx` viene annullato quando il deploy viene annullato o il worker si arresta. Le operazioni lunghe devono rispettarlo.
- **Log:** si usa il logger ricevuto dalla Factory. I log arrivano al log applicativo e al SIEM.
- **Concorrenza:** la stessa istanza può servire più deploy contemporaneamente, su ambienti diversi. Il plugin deve essere sicuro per l'uso concorrente.

## Dry-run

Un plugin può implementare anche l'interfaccia opzionale `plugin.DryRunner`:

```go
type DryRunner interface {
	DryRun(ctx context.Context, req *plugin.Request) (*plugin.Result, error)
}
```

In un [dry-run](../guida/dry-run.md), lo step `deploy` chiama `DryRun` invece di `Deploy`. `DryRun` deve verificare che il deploy sia eseguibile (raggiungibilità dei target, credenziali, permessi) **senza modificare nulla**, e riportare `plugin.StatusReady` per i target pronti. Conviene controllare tutti i target invece di fermarsi al primo errore, così l'esito è completo.

Un plugin che non implementa `DryRunner`, o che restituisce `plugin.ErrDryRunUnsupported`, in dry-run viene solo validato.

## Rollback

Per supportare il [rollback](../guida/rollback.md) un plugin implementa due interfacce opzionali:

```go
// Rollbacker riporta i target alla release indicata. Deve essere idempotente.
type Rollbacker interface {
	Rollback(ctx context.Context, req *plugin.Request, previous plugin.Release) (*plugin.Result, error)
}

// ReleaseInspector legge dai target la release in esercizio, senza modificarli.
// Restituisce nil se non ce n'è nessuna. Serve al locator "plugin".
type ReleaseInspector interface {
	CurrentRelease(ctx context.Context, req *plugin.Request) (*plugin.Release, error)
}
```

- **`Rollback`** riceve la stessa richiesta del deploy (stessi target, parametri e segreti) e la release registrata prima del deploy. Va applicato a **tutti** i target, anche a quelli che il deploy fallito non aveva ancora toccato: per questo deve essere idempotente. Conviene proseguire anche se un target fallisce e riportare gli errori alla fine, così si ripristina il maggior numero di target.
- **`CurrentRelease`** viene chiamato nel checkpoint, prima del deploy. Se i target non concordano sulla release in esercizio è meglio restituire un errore che sceglierne una a caso.
- Se il plugin implementa `Rollbacker` ma non `ReleaseInspector`, il rollback funziona con un locator diverso da `plugin`, ad esempio un sistema aziendale dei rilasci.

Senza `Rollbacker`, un deploy con il rollback attivo si ferma nel checkpoint, prima di toccare i target.

## Registrare un plugin interno

In `internal/app/app.go`, funzione `NewPlugins`:

```go
if err := reg.Register(mioplugin.Name, mioplugin.Factory); err != nil {
	return nil, err
}
```

Poi lo si attiva in `manent.yml`, nella sezione `plugins`, con la sua configurazione:

```yaml
plugins:
  mio-plugin:
    opzione: valore
```

Solo i plugin presenti in `plugins` sono attivi. Quale usare per un deploy lo decide la policy: `deploy.plugin` se indicato, altrimenti il primo plugin attivo, in ordine alfabetico, che supporta `artifact.type`.

## Testare un plugin

Il plugin di riferimento, `plugins/remotecopy`, nasconde la connessione SSH dietro le interfacce `Dialer` e `Session`. Così i test sostituiscono la rete con un'implementazione finta e verificano comandi e upload senza un server reale. È lo schema consigliato per ogni plugin che parla con sistemi esterni.
