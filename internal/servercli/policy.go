package servercli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/joeg-ita/manent/internal/app"
	"github.com/joeg-ita/manent/internal/audit"
	"github.com/joeg-ita/manent/internal/auth"
	"github.com/joeg-ita/manent/internal/cliutil"
	"github.com/joeg-ita/manent/internal/config"
	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/engine"
	"github.com/joeg-ita/manent/internal/governance"
	"github.com/joeg-ita/manent/internal/policy"
	"github.com/joeg-ita/manent/internal/secrets"
	"github.com/joeg-ita/manent/internal/service"
)

// localGovernance evaluates the governance policy of the configuration on a descriptor
// without the server: rules of manent.yml, or Rego policies from a file, bundle or Consul
// source. A policy in the store needs the server (manent validate).
func localGovernance(cfg *config.Config, log *slog.Logger, sp secrets.Provider, d, eff *descriptor.Descriptor) (map[string]any, error) {
	gov, err := app.NewGovernance(cfg, sp, log)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	out := map[string]any{"engine": gov.Engine()}
	if err := gov.Refresh(ctx); err != nil {
		out["error"] = err.Error()
		return out, nil
	}
	p := auth.CLIPrincipal()
	in := engine.DescriptorPolicyInput(governance.PhaseValidate, d, eff, engine.KindDeploy, p.String(), p.Groups, time.Now())
	dec, err := gov.Evaluate(ctx, in)
	if err != nil {
		out["error"] = err.Error()
		return out, nil
	}
	out["revision"], out["rule"], out["requires"] = dec.Revision, dec.Rule, dec.Requires
	out["approvals"] = dec.Approvals.RequiredOrZero()
	if dec.Approvals != nil && dec.Approvals.Approvers != nil {
		out["approvers"] = dec.Approvals.Approvers
	}
	if dec.Change != nil {
		out["change"] = dec.Change
	}
	if len(dec.Deny) > 0 {
		out["deny"] = dec.Deny
	}
	if len(dec.Notes) > 0 {
		out["notes"] = dec.Notes
	}
	if dec.Freeze != nil {
		out["freeze"] = dec.Freeze
	}
	return out, nil
}

// policyCommands are the local commands on a directory of policies.
func policyCommands(g *GlobalOptions) []cliutil.Command {
	return []cliutil.Command{
		{Name: "test", Short: "Compila una directory di policy ed esegue i suoi test, senza server", Cmd: &PolicyTestCmd{global: g}},
		{Name: "eval", Short: "Valuta una directory di policy su un descrittore, senza server", Cmd: &PolicyEvalCmd{global: g}},
		{Name: "export-rbac", Short: "Scrive i binding di rbac come dati di una revisione e un modulo authz.rego di partenza", Cmd: &PolicyExportRBACCmd{global: g}},
	}
}

// adminPolicyCommands are the break-glass commands on the database.
func adminPolicyCommands(g *GlobalOptions) []cliutil.Command {
	return []cliutil.Command{
		{Name: "policy-activate", Short: "Attiva una revisione della policy senza approvazione (break-glass, registrato nell'audit)", Cmd: &AdminPolicyActivateCmd{global: g}},
		{Name: "freeze-delete", Short: "Elimina un freeze creato a runtime (break-glass, registrato nell'audit)", Cmd: &AdminFreezeDeleteCmd{global: g}},
	}
}

// loadOptionalConfig reads the configuration if given (rules of manent.yml as data), the
// defaults otherwise.
func loadOptionalConfig(g *GlobalOptions) (*config.Config, error) {
	if g.Config == "" {
		return config.Default(), nil
	}
	return config.Load(g.Config)
}

// compileDir compiles a directory of policies with the options of the configuration.
func compileDir(ctx context.Context, cfg *config.Config, dir string) (*policy.Compiled, []service.PolicyTest, error) {
	static, err := app.NewStaticGovernance(cfg)
	if err != nil {
		return nil, nil, err
	}
	files, _, _, err := (&policy.FileSource{Dir: dir}).Fetch(ctx)
	if err != nil {
		return nil, nil, err
	}
	o := cfg.Governance.OPA
	return policy.Compile(ctx, files, "local", policy.CompileOptions{Query: o.Query, Rules: static.Rules, EvalTimeout: o.EvalTimeout, Authz: app.AuthzCompileOptions(cfg)})
}

// PolicyTestCmd compiles a directory and runs its tests.
type PolicyTestCmd struct {
	global *GlobalOptions
	Dir    string `short:"d" long:"dir" required:"true" description:"directory con i moduli Rego e i dati"`
}

// Execute implements flags.Commander.
func (c *PolicyTestCmd) Execute([]string) error {
	cfg, err := loadOptionalConfig(c.global)
	if err != nil {
		return err
	}
	comp, tests, err := compileDir(context.Background(), cfg, c.Dir)
	out := map[string]any{"tests": tests}
	if err != nil {
		out["error"] = err.Error()
		_ = cliutil.PrintJSON(out)
		return errors.New("the policy is not valid")
	}
	out["sha256"], out["valid"] = comp.SHA256, true
	return cliutil.PrintJSON(out)
}

// PolicyExportRBACCmd writes rbac.roles and rbac.bindings as data (manent/rbac/data.json) and
// a starting authz.rego, to move the bindings into a policy revision.
type PolicyExportRBACCmd struct {
	global *GlobalOptions
	Dir    string `short:"d" long:"dir" required:"true" description:"directory della revisione"`
	Force  bool   `long:"force" description:"sovrascrive i file esistenti"`
}

// Execute implements flags.Commander.
func (c *PolicyExportRBACCmd) Execute([]string) error {
	if c.global.Config == "" {
		return errors.New("export-rbac needs the configuration (-c manent.yml)")
	}
	cfg, err := config.Load(c.global.Config)
	if err != nil {
		return err
	}
	files, err := policy.ExportRBAC(app.RBACConfig(cfg))
	if err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		p := filepath.Join(c.Dir, filepath.FromSlash(name))
		if _, err := os.Stat(p); err == nil && !c.Force {
			return fmt.Errorf("%s already exists (--force to overwrite it)", p)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		p := filepath.Join(c.Dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(files[name]), 0o644); err != nil {
			return err
		}
		fmt.Println("scritto", p)
	}
	fmt.Fprintf(os.Stderr, "%d binding esportati: verifica la revisione con \"manent-server policy test -d %s\" e caricala con \"manent policy push -d %s\"\n",
		len(cfg.RBAC.Bindings), c.Dir, c.Dir)
	return nil
}

// PolicyEvalCmd evaluates a directory of policies on a descriptor.
type PolicyEvalCmd struct {
	global *GlobalOptions
	Dir    string `short:"d" long:"dir" required:"true" description:"directory con i moduli Rego e i dati"`
	File   string `short:"f" long:"file" required:"true" description:"descrittore YAML"`
	Phase  string `long:"phase" default:"submit" choice:"submit" choice:"validate" choice:"start" choice:"stage" choice:"tick" description:"fase della valutazione"`
	Kind   string `long:"kind" default:"deploy" choice:"deploy" choice:"promotion" choice:"rollback" description:"tipo di deploy"`
	Region string `long:"region" description:"region (fase stage)"`
}

// Execute implements flags.Commander.
func (c *PolicyEvalCmd) Execute([]string) error {
	cfg, err := loadOptionalConfig(c.global)
	if err != nil {
		return err
	}
	d, err := descriptor.LoadYAMLFile(c.File)
	if err != nil {
		return err
	}
	ctx := context.Background()
	comp, _, err := compileDir(ctx, cfg, c.Dir)
	if err != nil {
		return err
	}
	static, err := app.NewStaticGovernance(cfg)
	if err != nil {
		return err
	}
	p := auth.CLIPrincipal()
	in := engine.DescriptorPolicyInput(c.Phase, d, d, c.Kind, p.String(), p.Groups, time.Now())
	in.Region = c.Region
	windows := append(static.Freezes, comp.Windows...)
	dec, err := governance.Decide(ctx, in, windows, comp.Evaluate)
	if err != nil {
		return err
	}
	in.Freezes.Active = governance.ActiveWindows(windows, in.Application, in.Environment, in.Region, in.Now)
	return cliutil.PrintJSON(service.PolicyEvaluation{Input: in, Decision: dec})
}

// AdminPolicyActivateCmd activates a revision without approval.
type AdminPolicyActivateCmd struct {
	global *GlobalOptions
	Reason string `long:"reason" required:"true" description:"motivo, registrato nell'audit"`
	Args   struct {
		Revision int64 `positional-arg-name:"revisione" required:"true"`
	} `positional-args:"yes"`
}

// Execute implements flags.Commander.
func (c *AdminPolicyActivateCmd) Execute([]string) error {
	return withAdmin(c.global, func(ctx context.Context, a *app.App, _ *engine.Admin) error {
		err := a.Governance().ForceActivate(ctx, c.Args.Revision, adminActor())
		recordBreakGlass(ctx, a, "policy.activate", fmt.Sprintf("revision %d (break-glass): %s", c.Args.Revision, c.Reason), err)
		if err != nil {
			return err
		}
		a.Logger.Warn("revisione della policy attivata in break-glass", "revision", c.Args.Revision, "by", adminActor(), "reason", c.Reason)
		return nil
	})
}

// AdminFreezeDeleteCmd deletes a runtime freeze window outside the permissions.
type AdminFreezeDeleteCmd struct {
	global *GlobalOptions
	Reason string `long:"reason" required:"true" description:"motivo, registrato nell'audit"`
	Args   struct {
		Name string `positional-arg-name:"nome" required:"true"`
	} `positional-args:"yes"`
}

// Execute implements flags.Commander.
func (c *AdminFreezeDeleteCmd) Execute([]string) error {
	return withAdmin(c.global, func(ctx context.Context, a *app.App, _ *engine.Admin) error {
		err := a.Governance().DeleteFreeze(ctx, c.Args.Name)
		recordBreakGlass(ctx, a, "freeze.delete", c.Args.Name+" (break-glass): "+c.Reason, err)
		if err != nil {
			return err
		}
		a.Logger.Warn("freeze eliminato in break-glass", "freeze", c.Args.Name, "by", adminActor(), "reason", c.Reason)
		return nil
	})
}

// recordBreakGlass records a break-glass operation in the audit.
func recordBreakGlass(ctx context.Context, a *app.App, action, comment string, err error) {
	rec := a.Audit()
	if rec == nil {
		return
	}
	e := audit.Entry{Actor: adminActor(), Action: action, Outcome: audit.OutcomeAllowed, Comment: comment}
	if err != nil {
		e.Outcome, e.Reason = audit.OutcomeError, err.Error()
	}
	if rerr := rec.Record(ctx, e); rerr != nil {
		a.Logger.Error("registrazione nell'audit fallita", "action", action, "error", rerr)
	}
}
