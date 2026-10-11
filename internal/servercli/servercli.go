// Package servercli implementa la CLI di manent-server: API, worker e operazioni di
// piattaforma sul database di sistema. Gli utenti usano il client (cmd/manent), che parla solo
// con l'API.
package servercli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/user"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jessevdk/go-flags"

	"github.com/joeg-ita/manent/internal/api"
	"github.com/joeg-ita/manent/internal/app"
	"github.com/joeg-ita/manent/internal/artifact"
	"github.com/joeg-ita/manent/internal/audit"
	"github.com/joeg-ita/manent/internal/auth"
	"github.com/joeg-ita/manent/internal/cliutil"
	"github.com/joeg-ita/manent/internal/config"
	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/engine"
	"github.com/joeg-ita/manent/internal/pipeline"
	"github.com/joeg-ita/manent/internal/rollout"
)

// GlobalOptions sono le opzioni comuni a tutti i comandi.
type GlobalOptions struct {
	Config string `short:"c" long:"config" env:"MANENT_CONFIG" description:"file di configurazione YAML"`
}

// Run esegue la CLI con gli argomenti indicati.
func Run(args []string) error {
	var global GlobalOptions
	p := flags.NewParser(&global, flags.Default)
	p.Name = "manent-server"
	p.LongDescription = "manent-server: API, worker e operazioni di piattaforma di manent"

	add := func(name, short string, cmd any) {
		if _, err := p.AddCommand(name, short, short, cmd); err != nil {
			panic(err)
		}
	}
	g := &global
	add("serve", "Avvia l'API e, nello stesso processo, un worker (salvo --no-worker)", &ServeCmd{global: g})
	add("api", "Avvia solo l'API: accoda i deploy senza eseguirli (come serve --no-worker)", &ServeCmd{global: g, NoWorker: true})
	add("worker", "Avvia un worker che esegue i deploy dalla coda", &WorkerCmd{global: g})
	add("validate", "Verifica configurazione e descrittore in locale, senza database", &ValidateCmd{global: g})
	add("version", "Mostra la versione di manent-server", &cliutil.VersionCmd{})
	if err := cliutil.AddGroup(p, "admin", "Operazioni di piattaforma sul database di sistema",
		"Operazioni di piattaforma: worker, recovery dei nodi persi e degli aggiornamenti, retention dello storico", []cliutil.Command{
			{Name: "executors", Short: "Elenca i worker registrati e i loro workflow in sospeso", Cmd: &ExecutorsCmd{global: g}},
			{Name: "recover", Short: "Riprende i workflow di un worker perso o di una versione precedente", Cmd: &RecoverCmd{global: g}},
			{Name: "prune", Short: "Elimina lo storico dei workflow più vecchio della retention", Cmd: &PruneCmd{global: g}},
		}); err != nil {
		return err
	}
	if err := cliutil.AddGroup(p, "audit", "Strumenti per il file di audit", "Strumenti per il file di audit", []cliutil.Command{
		{Name: "verify", Short: "Verifica la catena di hash del file di audit e dei suoi file ruotati", Cmd: &AuditVerifyCmd{global: g}},
	}); err != nil {
		return err
	}
	return cliutil.Parse(p, args)
}

// ValidateCmd valida il descrittore senza avviare DBOS e mostra il deploy effettivo,
// cioè il descrittore fuso con la configurazione di ambiente (es. letta da Consul).
// A differenza di "deploy --dry-run" non contatta segreti, artefatto e target.
type ValidateCmd struct {
	global  *GlobalOptions
	File    string `short:"f" long:"file" required:"true" description:"descrittore di deploy YAML"`
	Offline bool   `long:"offline" description:"non leggere la configurazione di ambiente"`
}

// Execute implementa flags.Commander.
func (c *ValidateCmd) Execute([]string) error {
	d, err := descriptor.LoadYAMLFile(c.File)
	if err != nil {
		return err
	}
	cfg, err := config.Load(c.global.Config)
	if err != nil {
		return err
	}
	log, _, err := app.NewLogger(cfg)
	if err != nil {
		return err
	}
	plugins, err := app.NewPlugins(cfg, log)
	if err != nil {
		return err
	}
	sp, err := app.NewSecretProvider(cfg)
	if err != nil {
		return err
	}
	locators := app.NewLocators(cfg, log)
	reg, err := app.NewSteps(plugins, &artifact.Store{}, sp, locators, log)
	if err != nil {
		return err
	}
	plan, err := pipeline.ConfigPlanner{Steps: cfg.Pipeline.Steps, Registry: reg}.Plan(d)
	if err != nil {
		return err
	}
	policy := app.NewCallbackPolicy(cfg)
	for _, cb := range d.Callbacks {
		if err := policy.Check(cb.URL); err != nil {
			return err
		}
	}
	rbPolicy := app.NewRollbackPolicy(cfg, locators)
	eff := *d
	if eff, err = rbPolicy.Apply(eff); err != nil {
		return err
	}
	out := map[string]any{"valid": true, "plan": plan, "rollback": eff.Rollback}
	gov, err := app.NewGovernance(cfg)
	if err != nil {
		return err
	}
	rule := gov.Rule(d.Application, d.Environment)
	g := map[string]any{"rule": rule.Name, "requires": rule.Requires, "approvals": rule.Approvals.Required}
	if w := gov.Frozen(d.Application, d.Environment, time.Now()); w != nil {
		g["freeze"] = w
	}
	out["governance"] = g
	if !c.Offline {
		resolver, err := app.NewResolver(cfg)
		if err != nil {
			return err
		}
		eff, meta, err := resolver.Resolve(context.Background(), *d)
		if err != nil {
			return err
		}
		if eff, err = rbPolicy.Apply(eff); err != nil {
			return err
		}
		names := make([]string, len(eff.Secrets))
		for i, s := range eff.Secrets {
			names[i] = s.Name
		}
		out["config"] = meta
		out["deploy"] = eff.Deploy
		out["secrets"] = names
		out["rollback"] = eff.Rollback
		if eff.Rollout != nil {
			p, err := rollout.Build(eff.Deploy.Targets, eff.Rollout)
			if err != nil {
				return fmt.Errorf("rollout: %w", err)
			}
			stages := make([]map[string]any, len(p.Stages))
			for i, st := range p.Stages {
				passes := map[string][]string{}
				for _, ps := range st.Passes {
					passes[ps.Name] = ps.Hosts()
				}
				stages[i] = map[string]any{"name": st.Name, "bake": st.Bake.String(), "passes": passes}
			}
			out["rollout"] = map[string]any{"on_failure": eff.Rollout.OnFailureOrDefault(), "stages": stages}
		}
	}
	return cliutil.PrintJSON(out)
}

// ServeCmd avvia il server HTTP e lo arresta in modo pulito su SIGINT/SIGTERM.
// Di default è anche un worker; con --no-worker espone solo le API e lascia l'esecuzione ai worker.
type ServeCmd struct {
	global   *GlobalOptions
	Listen   string `short:"l" long:"listen" description:"indirizzo di ascolto (sovrascrive la configurazione)"`
	NoWorker bool   `long:"no-worker" description:"solo API: accoda i deploy senza eseguirli"`
}

// Execute implementa flags.Commander.
func (c *ServeCmd) Execute([]string) error {
	newApp := newWorker
	if c.NoWorker {
		newApp = newClient
	}
	a, err := newApp(c.global)
	if err != nil {
		return err
	}
	stopTelemetry, err := startTelemetry(a)
	if err != nil {
		a.Close()
		return err
	}
	defer stopTelemetry()
	defer startOps(a)()
	defer a.Close()

	addr := a.Config.HTTP.Listen
	if c.Listen != "" {
		addr = c.Listen
	}
	ctx, stop := cliutil.SignalContext()
	defer stop()
	authn, err := app.NewAuthenticator(ctx, a.Config, a.Logger)
	if err != nil {
		return fmt.Errorf("autenticazione: %w", err)
	}
	a.WarnInsecure()
	gin.SetMode(gin.ReleaseMode)
	srv := &http.Server{Addr: addr, Handler: api.NewRouterWith(a.Service, authn, a.Logger, a.APIOptions()), ReadHeaderTimeout: 10 * time.Second}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	a.Logger.Info("server API avviato", "addr", addr)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	a.Logger.Info("arresto server API")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// WorkerCmd avvia un worker senza API: preleva ed esegue i deploy dalla coda finché non riceve SIGINT/SIGTERM.
type WorkerCmd struct {
	global *GlobalOptions
}

// Execute implementa flags.Commander.
func (c *WorkerCmd) Execute([]string) error {
	a, err := newWorker(c.global)
	if err != nil {
		return err
	}
	stopTelemetry, err := startTelemetry(a)
	if err != nil {
		a.Close()
		return err
	}
	// In ordine inverso: drain e arresto, poi server di observability (che durante il drain
	// risponde 503 su /readyz), infine le ultime span.
	defer stopTelemetry()
	defer startOps(a)()
	defer a.Close()
	a.WarnInsecure()
	ctx, stop := cliutil.SignalContext()
	defer stop()
	<-ctx.Done()
	a.Logger.Info("arresto worker")
	return nil
}

// cliContext restituisce il context dei comandi CLI, con l'identità dell'utente del sistema
// operativo: i permessi RBAC si applicano anche alla CLI e l'audit registra chi ha agito.
func cliContext() context.Context {
	return auth.WithPrincipal(context.Background(), auth.CLIPrincipal())
}

// newClient crea l'applicazione in modalità client: accoda e interroga senza eseguire deploy.
func newClient(g *GlobalOptions) (*app.App, error) {
	cfg, err := config.Load(g.Config)
	if err != nil {
		return nil, err
	}
	return app.NewClient(cfg)
}

// newWorker crea l'applicazione in modalità worker: esegue i deploy e recupera i propri workflow interrotti.
func newWorker(g *GlobalOptions) (*app.App, error) {
	cfg, err := config.Load(g.Config)
	if err != nil {
		return nil, err
	}
	return app.NewWorker(cfg)
}

// AuditVerifyCmd verifica la catena di hash del file di audit. Senza argomenti verifica
// audit.file della configurazione e i suoi file ruotati.
type AuditVerifyCmd struct {
	global *GlobalOptions
	Args   struct {
		Files []string `positional-arg-name:"file"`
	} `positional-args:"yes"`
}

// Execute implementa flags.Commander.
func (c *AuditVerifyCmd) Execute([]string) error {
	cfg, err := config.Load(c.global.Config)
	if err != nil {
		return err
	}
	files := c.Args.Files
	switch {
	case len(files) == 1:
		// Un solo file: si verificano anche i suoi file ruotati, in ordine cronologico.
		if files, err = audit.Files(files[0]); err != nil {
			return err
		}
	case len(files) == 0 && cfg.Audit.File != "":
		if files, err = audit.Files(cfg.Audit.File); err != nil {
			return err
		}
	case len(files) == 0:
		return errors.New("indicare il file di audit (o audit.file in configurazione)")
	}
	sp, err := app.NewSecretProvider(cfg)
	if err != nil {
		return err
	}
	key, err := app.AuditKey(cfg, sp)
	if err != nil {
		return err
	}
	rep, err := audit.Verify(files, key)
	if err != nil {
		_ = cliutil.PrintJSON(rep)
		return fmt.Errorf("catena di audit non valida: %w", err)
	}
	fmt.Fprintln(os.Stderr, "catena di audit integra")
	return cliutil.PrintJSON(rep)
}

// withAdmin esegue fn con le operazioni di piattaforma. Lavorano sul database di sistema,
// quindi non sono disponibili con --server.
func withAdmin(g *GlobalOptions, fn func(context.Context, *app.App, *engine.Admin) error) error {
	a, err := newClient(g)
	if err != nil {
		return err
	}
	defer a.Close()
	return fn(cliContext(), a, a.Admin())
}

// adminActor identifica l'operatore nell'audit e nel registro dei worker.
func adminActor() string {
	name := "unknown"
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	return "cli:" + name
}

// ExecutorsCmd elenca i worker registrati e quelli con workflow in sospeso.
type ExecutorsCmd struct {
	global *GlobalOptions
}

// Execute implementa flags.Commander.
func (c *ExecutorsCmd) Execute([]string) error {
	return withAdmin(c.global, func(ctx context.Context, a *app.App, adm *engine.Admin) error {
		es, err := adm.Executors(ctx, a.Config.Recovery.StaleAfter)
		if err != nil {
			return err
		}
		return cliutil.PrintJSON(map[string]any{"version": a.Config.App.Version, "executors": es})
	})
}

// RecoverCmd riprende i workflow di un worker perso o di una versione non più eseguita.
type RecoverCmd struct {
	global      *GlobalOptions
	Executor    string `long:"executor" description:"riprende i workflow in sospeso di questo worker perso"`
	FromVersion string `long:"from-version" description:"riprende i workflow in sospeso o in coda di questa versione (dopo un aggiornamento)"`
	ToVersion   string `long:"to-version" description:"versione con cui riprenderli (default app.version)"`
	FromStart   bool   `long:"from-start" description:"riesegue i workflow dall'inizio invece che dall'ultimo step concluso"`
	Force       bool   `long:"force" description:"procede anche se il worker risulta attivo, è sconosciuto o è già stato recuperato"`
	DryRun      bool   `long:"dry-run" description:"mostra cosa verrebbe fatto senza modificare nulla"`
}

// Execute implementa flags.Commander.
func (c *RecoverCmd) Execute([]string) error {
	return withAdmin(c.global, func(ctx context.Context, a *app.App, adm *engine.Admin) error {
		rep, err := adm.Recover(ctx, engine.RecoverOptions{
			Executor: c.Executor, FromVersion: c.FromVersion, ToVersion: c.ToVersion, FromStart: c.FromStart,
			StaleAfter: a.Config.Recovery.StaleAfter, Force: c.Force, DryRun: c.DryRun, By: adminActor(),
		})
		if err != nil {
			return err
		}
		return cliutil.PrintJSON(rep)
	})
}

// PruneCmd elimina lo storico più vecchio di retention.max_age (o di --max-age).
type PruneCmd struct {
	global       *GlobalOptions
	MaxAge       time.Duration `long:"max-age" description:"età oltre la quale eliminare i workflow conclusi (default retention.max_age)"`
	KeepReleases int           `long:"keep-releases" description:"deploy riusciti conservati per applicazione e ambiente (default retention.keep_releases)"`
	DryRun       bool          `long:"dry-run" description:"conta i workflow senza eliminarli"`
}

// Execute implementa flags.Commander.
func (c *PruneCmd) Execute([]string) error {
	return withAdmin(c.global, func(ctx context.Context, a *app.App, adm *engine.Admin) error {
		o := engine.RetentionOptions{
			MaxAge: a.Config.Retention.MaxAge, KeepReleases: a.Config.Retention.KeepReleases, DryRun: c.DryRun, By: adminActor(),
		}
		if c.MaxAge > 0 {
			o.MaxAge = c.MaxAge
		}
		if c.KeepReleases > 0 {
			o.KeepReleases = c.KeepReleases
		}
		if o.MaxAge <= 0 {
			return errors.New("indicare --max-age o configurare retention.max_age")
		}
		rep, err := adm.Prune(ctx, o)
		if err != nil {
			return err
		}
		return cliutil.PrintJSON(rep)
	})
}

// startOps avvia, se configurato, il server di observability.listen (/metrics, /healthz,
// /readyz). Restituisce la funzione che lo arresta.
func startOps(a *app.App) func() {
	addr := a.Config.Observability.Listen
	if addr == "" {
		return func() {}
	}
	srv := &http.Server{Addr: addr, Handler: a.OpsHandler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.Logger.Error("server di observability", "addr", addr, "error", err)
		}
	}()
	a.Logger.Info("server di observability avviato", "addr", addr)
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
}

// startTelemetry configura il tracing e le metriche di stato di un processo di lunga durata.
// Restituisce la funzione che esporta le ultime span.
func startTelemetry(a *app.App) (func(), error) {
	shutdown, err := app.SetupTracing(context.Background(), a.Config, a.Config.DBOS.ExecutorID)
	if err != nil {
		return nil, fmt.Errorf("tracing: %w", err)
	}
	if err := a.EnableStateMetrics(); err != nil {
		return nil, fmt.Errorf("metriche: %w", err)
	}
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdown(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "tracing:", err)
		}
	}, nil
}
