// Package servercli implements the manent-server CLI: API, workers and platform operations on
// the system database. Users use the client (cmd/manent), which talks only to the API.
package servercli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/user"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jessevdk/go-flags"

	"github.com/manent-deployer/manent/internal/api"
	"github.com/manent-deployer/manent/internal/app"
	"github.com/manent-deployer/manent/internal/artifact"
	"github.com/manent-deployer/manent/internal/audit"
	"github.com/manent-deployer/manent/internal/auth"
	"github.com/manent-deployer/manent/internal/cliutil"
	"github.com/manent-deployer/manent/internal/config"
	"github.com/manent-deployer/manent/internal/descriptor"
	"github.com/manent-deployer/manent/internal/engine"
	"github.com/manent-deployer/manent/internal/pipeline"
	"github.com/manent-deployer/manent/internal/rollout"
	"github.com/manent-deployer/manent/internal/tenant"
)

// GlobalOptions are the options shared by all commands.
type GlobalOptions struct {
	Config string `short:"c" long:"config" env:"MANENT_CONFIG" description:"YAML configuration file"`
}

// Run runs the CLI with the given arguments.
func Run(args []string) error {
	var global GlobalOptions
	p := flags.NewParser(&global, flags.Default)
	p.Name = "manent-server"
	p.LongDescription = "manent-server: API, workers and platform operations of manent"

	add := func(name, short string, cmd any) {
		if _, err := p.AddCommand(name, short, short, cmd); err != nil {
			panic(err)
		}
	}
	g := &global
	add("serve", "Starts the API and, in the same process, a worker (unless --no-worker)", &ServeCmd{global: g})
	add("api", "Starts only the API: enqueues deploys without running them (as serve --no-worker)", &ServeCmd{global: g, NoWorker: true})
	add("worker", "Starts a worker that runs the deploys from the queue", &WorkerCmd{global: g})
	add("validate", "Checks configuration and descriptor locally, without database", &ValidateCmd{global: g})
	add("version", "Shows the manent-server version", &cliutil.VersionCmd{})
	if err := cliutil.AddGroup(p, "admin", "Platform operations on the system database",
		"Platform operations: workers, recovery of lost nodes and of upgrades, history retention", append([]cliutil.Command{
			{Name: "executors", Short: "Lists the registered workers and their pending workflows", Cmd: &ExecutorsCmd{global: g}},
			{Name: "recover", Short: "Resumes the workflows of a lost worker or of a previous version", Cmd: &RecoverCmd{global: g}},
			{Name: "prune", Short: "Deletes the workflow history older than the retention", Cmd: &PruneCmd{global: g}},
			{Name: "specs", Short: "Lists waiting deploys and schedules that contain revoked custom step specifications", Cmd: &SpecsCmd{global: g}},
			{Name: "rename-artifact", Short: "Renames an artifact in the history of an application (inventory, rollbacks and promotions follow the new name)", Cmd: &RenameArtifactCmd{global: g}},
			{Name: "move-application", Short: "Moves the history of an application from one tenant to another (also from untenanted)", Cmd: &MoveApplicationCmd{global: g}},
		}, adminPolicyCommands(g)...)); err != nil {
		return err
	}
	if err := cliutil.AddGroup(p, "policy", "Checks governance policies (Rego) locally",
		"Compiles, tests and evaluates a Rego policy directory without server and without database", policyCommands(g)); err != nil {
		return err
	}
	if err := cliutil.AddGroup(p, "audit", "Tools for the audit file", "Tools for the audit file", []cliutil.Command{
		{Name: "verify", Short: "Verifies the hash chain of the audit file and of its rotated files", Cmd: &AuditVerifyCmd{global: g}},
	}); err != nil {
		return err
	}
	return cliutil.Parse(p, args)
}

// ValidateCmd validates the descriptor without starting DBOS and shows the effective deploy,
// that is the descriptor merged with the environment configuration (e.g. read from Consul).
// Unlike "deploy --dry-run" it touches no secrets, artifact or targets.
type ValidateCmd struct {
	global  *GlobalOptions
	File    string `short:"f" long:"file" required:"true" description:"YAML deploy descriptor"`
	Offline bool   `long:"offline" description:"do not read the environment configuration"`
}

// Execute implements flags.Commander.
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
	reg, _, err := app.NewSteps(plugins, &artifact.Store{}, sp, locators, log)
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
	out := map[string]any{"valid": true, "plan": plan.Public(), "rollback": eff.Rollback}
	g, err := localGovernance(cfg, log, sp, d, nil)
	if err != nil {
		return err
	}
	out["governance"] = g
	if !c.Offline {
		resolver, err := app.NewResolver(cfg, sp)
		if err != nil {
			return err
		}
		// The tenant is part of the configuration key: resolved as the engine does.
		owner, err := tenant.Resolve(context.Background(), app.NewTenantCache(cfg, log), d.Application, d.Tenant, cfg.Tenants.Untenanted != config.UntenantedReject)
		if err != nil {
			return err
		}
		d.Tenant = owner.Name
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
		if g, err = localGovernance(cfg, log, sp, d, &eff); err != nil {
			return err
		}
		out["governance"] = g
		out["config"] = meta
		out["deploy"] = eff.Deploy
		out["secrets"] = names
		out["rollback"] = eff.Rollback
		if eff.HasImports() {
			// The exports of other deploys are read from the database when the deploy starts.
			out["imports"] = map[string]any{"imports": eff.Imports, "targetsFrom": eff.Deploy.TargetsFrom, "paramsFrom": eff.Deploy.ParamsFrom,
				"note": "resolved when the deploy starts: 'manent validate' against the server reads them"}
		}
		if eff.Rollout != nil && len(eff.Deploy.Targets) > 0 {
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

// ServeCmd starts the HTTP server and stops it cleanly on SIGINT/SIGTERM.
// By default it is also a worker; with --no-worker it only exposes the APIs and leaves
// execution to the workers.
type ServeCmd struct {
	global   *GlobalOptions
	Listen   string `short:"l" long:"listen" description:"listen address (overrides the configuration)"`
	NoWorker bool   `long:"no-worker" description:"API only: enqueues deploys without running them"`
}

// Execute implements flags.Commander.
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
		return fmt.Errorf("authentication: %w", err)
	}
	a.WarnInsecure()
	gin.SetMode(gin.ReleaseMode)
	srv := &http.Server{Addr: addr, Handler: api.NewRouterWith(a.Service, authn, a.Logger, a.APIOptions()), ReadHeaderTimeout: 10 * time.Second}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	a.Logger.Info("API server started", "addr", addr)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	a.Logger.Info("stopping API server")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// WorkerCmd starts a worker without API: it dequeues and runs deploys until it receives
// SIGINT/SIGTERM.
type WorkerCmd struct {
	global *GlobalOptions
}

// Execute implements flags.Commander.
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
	// In reverse order: drain and shutdown, then the observability server (which answers 503 on
	// /readyz during the drain), finally the last spans.
	defer stopTelemetry()
	defer startOps(a)()
	defer a.Close()
	a.WarnInsecure()
	ctx, stop := cliutil.SignalContext()
	defer stop()
	<-ctx.Done()
	a.Logger.Info("stopping worker")
	return nil
}

// cliContext returns the context of CLI commands, with the identity of the operating system
// user: RBAC permissions apply to the CLI too and the audit records who acted.
func cliContext() context.Context {
	return auth.WithPrincipal(context.Background(), auth.CLIPrincipal())
}

// newClient creates the application in client mode: it enqueues and queries without running deploys.
func newClient(g *GlobalOptions) (*app.App, error) {
	cfg, err := config.Load(g.Config)
	if err != nil {
		return nil, err
	}
	return app.NewClient(cfg)
}

// newWorker creates the application in worker mode: it runs deploys and recovers its own
// interrupted workflows.
func newWorker(g *GlobalOptions) (*app.App, error) {
	cfg, err := config.Load(g.Config)
	if err != nil {
		return nil, err
	}
	return app.NewWorker(cfg)
}

// AuditVerifyCmd verifies the hash chain of the audit file. Without arguments it verifies the
// configuration's audit.file and its rotated files.
type AuditVerifyCmd struct {
	global *GlobalOptions
	Args   struct {
		Files []string `positional-arg-name:"file"`
	} `positional-args:"yes"`
}

// Execute implements flags.Commander.
func (c *AuditVerifyCmd) Execute([]string) error {
	cfg, err := config.Load(c.global.Config)
	if err != nil {
		return err
	}
	files := c.Args.Files
	switch {
	case len(files) == 1:
		// A single file: its rotated files are verified too, in chronological order.
		if files, err = audit.Files(files[0]); err != nil {
			return err
		}
	case len(files) == 0 && cfg.Audit.File != "":
		if files, err = audit.Files(cfg.Audit.File); err != nil {
			return err
		}
	case len(files) == 0:
		return errors.New("provide the audit file (or audit.file in the configuration)")
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
		return fmt.Errorf("invalid audit chain: %w", err)
	}
	fmt.Fprintln(os.Stderr, "audit chain intact")
	return cliutil.PrintJSON(rep)
}

// withAdmin runs fn with the platform operations. They work on the system database, so they
// are not available with --server.
func withAdmin(g *GlobalOptions, fn func(context.Context, *app.App, *engine.Admin) error) error {
	a, err := newClient(g)
	if err != nil {
		return err
	}
	defer a.Close()
	return fn(cliContext(), a, a.Admin())
}

// adminActor identifies the operator in the audit and in the worker registry.
func adminActor() string {
	name := "unknown"
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	return "cli:" + name
}

// ExecutorsCmd lists the registered workers and those with pending workflows.
type ExecutorsCmd struct {
	global *GlobalOptions
}

// Execute implements flags.Commander.
func (c *ExecutorsCmd) Execute([]string) error {
	return withAdmin(c.global, func(ctx context.Context, a *app.App, adm *engine.Admin) error {
		es, err := adm.Executors(ctx, a.Config.Recovery.StaleAfter)
		if err != nil {
			return err
		}
		return cliutil.PrintJSON(map[string]any{"version": a.Config.App.Version, "executors": es})
	})
}

// RecoverCmd resumes the workflows of a lost worker or of a version no longer run.
type RecoverCmd struct {
	global      *GlobalOptions
	Executor    string `long:"executor" description:"resumes the pending workflows of this lost worker"`
	FromVersion string `long:"from-version" description:"resumes the pending or queued workflows of this version (after an upgrade)"`
	ToVersion   string `long:"to-version" description:"version to resume them with (default app.version)"`
	FromStart   bool   `long:"from-start" description:"rerun the workflows from the start instead of from the last completed step"`
	Force       bool   `long:"force" description:"goes on even if the worker is running, unknown or already recovered"`
	DryRun      bool   `long:"dry-run" description:"shows what would be done without changing anything"`
}

// Execute implements flags.Commander.
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

// RenameArtifactCmd renames an artifact in the history of an application.
type RenameArtifactCmd struct {
	global      *GlobalOptions
	Tenant      string `short:"t" long:"tenant" description:"tenant of the application (default untenanted)"`
	Application string `short:"a" long:"application" required:"true" description:"application"`
	From        string `long:"from" required:"true" description:"current name of the artifact (artifact.name)"`
	To          string `long:"to" required:"true" description:"new name of the artifact"`
	DryRun      bool   `long:"dry-run" description:"count the workflows without changing them"`
}

// Execute implements flags.Commander.
func (c *RenameArtifactCmd) Execute([]string) error {
	return withAdmin(c.global, func(ctx context.Context, _ *app.App, adm *engine.Admin) error {
		rep, err := adm.RenameArtifact(ctx, engine.RenameArtifactOptions{Tenant: c.Tenant, Application: c.Application, From: c.From, To: c.To, DryRun: c.DryRun, By: adminActor()})
		if err != nil {
			return err
		}
		return cliutil.PrintJSON(rep)
	})
}

// MoveApplicationCmd moves the history of an application from one tenant to another.
type MoveApplicationCmd struct {
	global      *GlobalOptions
	Application string `short:"a" long:"application" required:"true" description:"application"`
	From        string `long:"from" required:"true" description:"current tenant (untenanted for the applications without tenant)"`
	To          string `long:"to" required:"true" description:"new tenant"`
	DryRun      bool   `long:"dry-run" description:"count the workflows without changing them"`
}

// Execute implements flags.Commander.
func (c *MoveApplicationCmd) Execute([]string) error {
	return withAdmin(c.global, func(ctx context.Context, _ *app.App, adm *engine.Admin) error {
		rep, err := adm.MoveApplication(ctx, engine.MoveApplicationOptions{Application: c.Application, From: c.From, To: c.To, DryRun: c.DryRun, By: adminActor()})
		if err != nil {
			return err
		}
		return cliutil.PrintJSON(rep)
	})
}

// PruneCmd deletes the history older than retention.max_age (or --max-age).
type PruneCmd struct {
	global       *GlobalOptions
	MaxAge       time.Duration `long:"max-age" description:"age beyond which the ended workflows are deleted (default retention.max_age)"`
	KeepReleases int           `long:"keep-releases" description:"successful deploys kept per application and environment (default retention.keep_releases)"`
	DryRun       bool          `long:"dry-run" description:"count the workflows without deleting them"`
}

// Execute implements flags.Commander.
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
			return errors.New("provide --max-age or configure retention.max_age")
		}
		rep, err := adm.Prune(ctx, o)
		if err != nil {
			return err
		}
		return cliutil.PrintJSON(rep)
	})
}

// SpecsCmd lists the unfinished workflows and schedules containing revoked custom step specs
// (pipeline.custom.revoked_specs or --hash).
type SpecsCmd struct {
	global *GlobalOptions
	Hash   []string `long:"hash" description:"sha256 of the specification to search for, instead of pipeline.custom.revoked_specs (repeatable)"`
	All    bool     `long:"all" description:"lists all the specifications captured in the plans, revoked or not"`
}

// Execute implements flags.Commander.
func (c *SpecsCmd) Execute([]string) error {
	return withAdmin(c.global, func(ctx context.Context, a *app.App, adm *engine.Admin) error {
		var hashes []string
		for _, h := range c.Hash {
			h = strings.ToLower(strings.TrimSpace(h))
			if len(h) != 64 || strings.Trim(h, "0123456789abcdef") != "" {
				return fmt.Errorf("--hash %q: expected a hex sha256 (64 characters)", h)
			}
			hashes = append(hashes, h)
		}
		if len(hashes) == 0 {
			hashes = a.Config.Pipeline.Custom.RevokedSpecs
		}
		if len(hashes) == 0 && !c.All {
			return errors.New("no revoked specification in pipeline.custom.revoked_specs: provide --hash or --all")
		}
		rep, err := adm.Specs(ctx, engine.SpecsOptions{Hashes: hashes, All: c.All})
		if err != nil {
			return err
		}
		return cliutil.PrintJSON(rep)
	})
}

// startOps starts, if configured, the observability.listen server (/metrics, /healthz,
// /readyz). It returns the function that stops it.
func startOps(a *app.App) func() {
	addr := a.Config.Observability.Listen
	if addr == "" {
		return func() {}
	}
	srv := &http.Server{Addr: addr, Handler: a.OpsHandler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.Logger.Error("observability server", "addr", addr, "error", err)
		}
	}()
	a.Logger.Info("observability server started", "addr", addr)
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
}

// startTelemetry configures tracing and state metrics of a long-running process.
// It returns the function that exports the last spans.
func startTelemetry(a *app.App) (func(), error) {
	shutdown, err := app.SetupTracing(context.Background(), a.Config, a.Config.DBOS.ExecutorID)
	if err != nil {
		return nil, fmt.Errorf("tracing: %w", err)
	}
	if err := a.EnableStateMetrics(); err != nil {
		return nil, fmt.Errorf("metrics: %w", err)
	}
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdown(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "tracing:", err)
		}
	}, nil
}
