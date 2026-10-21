package clientcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/joeg-ita/manent/internal/cliutil"
	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/service"
)

// DeployCmd enqueues a deploy and, by default, waits for it to end.
// Scheduled deploys (schedule in the descriptor) are not waited for.
type DeployCmd struct {
	global  *GlobalOptions
	File    string        `short:"f" long:"file" required:"true" description:"descrittore di deploy YAML"`
	NoWait  bool          `long:"no-wait" description:"non attendere la conclusione del deploy"`
	Timeout time.Duration `long:"timeout" description:"tempo massimo di attesa, es. 30m (0 = nessun limite); il deploy prosegue comunque"`
	DryRun  bool          `long:"dry-run" description:"simula il deploy su un worker: verifica configurazione, segreti, artefatto e target senza modificarli"`
	After   []string      `long:"after" description:"ID di un deploy che deve riuscire prima di questo (ripetibile); si aggiunge ad after del descrittore"`
	Import  []string      `long:"import" value-name:"NOME=ID" description:"legge gli export del deploy ID come import NOME del descrittore (ripetibile); aggiunge ID ad after"`
	ArtifactOverrides
	Outputs string `long:"outputs" description:"file a cui aggiungere gli esiti come righe chiave=valore (deployment_id, state, ...): $GITHUB_OUTPUT o un report dotenv di GitLab"`
	Summary string `long:"summary" description:"file a cui aggiungere il riepilogo del deploy in Markdown ($GITHUB_STEP_SUMMARY)"`
	CI      bool   `long:"ci" description:"riconosce GitHub Actions (esiti in $GITHUB_OUTPUT, riepilogo in $GITHUB_STEP_SUMMARY) o GitLab CI (esiti in manent.env, o $MANENT_DOTENV)"`
}

// ArtifactOverrides replace fields of the descriptor from the command line: in CI the
// descriptor stays in the repository and the build gives the version.
type ArtifactOverrides struct {
	ID      string `long:"id" description:"ID del deploy (chiave di idempotenza), al posto di quello del descrittore"`
	Version string `long:"version" description:"versione dell'artefatto, al posto di quella del descrittore"`
	Source  string `long:"source" description:"sorgente dell'artefatto, al posto di quella del descrittore"`
	SHA256  string `long:"sha256" description:"sha256 atteso dell'artefatto"`
}

// apply writes the overrides into the descriptor and validates it again.
func (o ArtifactOverrides) apply(d *descriptor.Descriptor) bool {
	if o == (ArtifactOverrides{}) {
		return false
	}
	if o.ID != "" {
		d.ID = o.ID
	}
	if o.Version != "" {
		d.Artifact.Version = o.Version
	}
	if o.Source != "" {
		d.Artifact.Source = o.Source
	}
	if o.SHA256 != "" {
		d.Artifact.SHA256 = o.SHA256
	}
	return true
}

// Execute implements flags.Commander.
func (c *DeployCmd) Execute([]string) error {
	data, err := os.ReadFile(c.File)
	if err != nil {
		return err
	}
	// Validated after the options: --import can declare the imports the descriptor uses.
	d, err := descriptor.DecodeYAML(data)
	if err != nil {
		return err
	}
	if err := applyImports(d, c.Import); err != nil {
		return err
	}
	c.ArtifactOverrides.apply(d)
	d.DryRun = d.DryRun || c.DryRun
	for _, id := range c.After {
		if !slices.Contains(d.After, id) {
			d.After = append(d.After, id)
		}
	}
	if err := d.Validate(); err != nil {
		return err
	}
	rep, err := c.report()
	if err != nil {
		return err
	}
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		sub, err := svc.Submit(ctx, d)
		if err != nil {
			return err
		}
		ferr := follow(ctx, svc, sub, c.NoWait, c.Timeout)
		// Also on failure: the pipeline reads the state of the deploy it started.
		return errors.Join(ferr, rep.write(ctx, svc, d, sub))
	})
}

// applyImports binds the imports of the descriptor to given deploys ("name=id"): the import
// becomes a deployment import and the deploy is added to the dependencies, so it has succeeded
// when this one starts. The CI knows the ID of the producer only at run time.
func applyImports(d *descriptor.Descriptor, specs []string) error {
	for _, spec := range specs {
		name, id, ok := strings.Cut(spec, "=")
		if !ok || name == "" || id == "" {
			return fmt.Errorf("--import %q: expected NAME=ID", spec)
		}
		imports := maps.Clone(d.Imports)
		if imports == nil {
			imports = map[string]descriptor.Import{}
		}
		imports[name] = descriptor.Import{Deployment: id}
		d.Imports = imports
		if !slices.Contains(d.After, id) {
			d.After = append(d.After, id)
		}
	}
	return nil
}

// ValidateCmd checks the descriptor on the server, as at submission but without starting the
// deploy, and shows plan, effective deploy (merged with the environment configuration),
// rollback and governance rules. Unlike "deploy --dry-run" it touches no secrets, artifact or targets.
type ValidateCmd struct {
	global *GlobalOptions
	File   string `short:"f" long:"file" required:"true" description:"descrittore di deploy YAML"`
}

// Execute implements flags.Commander.
func (c *ValidateCmd) Execute([]string) error {
	d, err := descriptor.LoadYAMLFile(c.File)
	if err != nil {
		return err
	}
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		v, err := svc.Validate(ctx, d)
		if err != nil {
			return err
		}
		for _, w := range v.Warnings {
			fmt.Fprintln(os.Stderr, "attenzione:", w)
		}
		return cliutil.PrintJSON(v)
	})
}

// idArgs is the positional argument with a deploy ID.
type idArgs struct {
	ID string `positional-arg-name:"id" required:"true"`
}

// StatusCmd shows the status of a deploy.
type StatusCmd struct {
	global *GlobalOptions
	Args   idArgs `positional-args:"yes"`
}

// Execute implements flags.Commander.
func (c *StatusCmd) Execute([]string) error {
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		st, err := svc.Status(ctx, c.Args.ID)
		if err != nil {
			return err
		}
		return cliutil.PrintJSON(st)
	})
}

// ListCmd lists deploys, with filters on schedule, application, environment and state.
type ListCmd struct {
	global      *GlobalOptions
	Schedule    string `short:"s" long:"schedule" description:"solo i deploy generati da questa schedule"`
	Application string `short:"a" long:"application" description:"solo i deploy di questa applicazione"`
	Environment string `short:"e" long:"environment" description:"solo i deploy in questo ambiente"`
	Tenant      string `short:"t" long:"tenant" description:"solo i deploy delle applicazioni di questo tenant"`
	Artifact    string `short:"A" long:"artifact" description:"solo i deploy di questo artefatto (artifact.name)"`
	Selector    string `long:"selector" description:"solo le applicazioni scelte dal selettore sui tag (es. criticality=high, tenant=payments)"`
	State       string `long:"state" description:"solo i deploy in questo stato (es. SUCCESS, ERROR, AWAITING_APPROVAL)"`
	Limit       int    `short:"n" long:"limit" default:"20" description:"numero massimo di deploy"`
}

// Execute implements flags.Commander.
func (c *ListCmd) Execute([]string) error {
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		deps, err := svc.List(ctx, service.ListFilter{Schedule: c.Schedule, Application: c.Application, Environment: c.Environment, Tenant: c.Tenant, Artifact: c.Artifact, Selector: c.Selector, State: c.State, Limit: c.Limit})
		if err != nil {
			return err
		}
		return cliutil.PrintJSON(deps)
	})
}

// CancelCmd cancels a deploy.
type CancelCmd struct {
	global *GlobalOptions
	Args   idArgs `positional-args:"yes"`
}

// Execute implements flags.Commander.
func (c *CancelCmd) Execute([]string) error {
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		if err := svc.Cancel(ctx, c.Args.ID); err != nil {
			return err
		}
		fmt.Println("deploy annullato:", c.Args.ID)
		return nil
	})
}

// ScheduleListCmd lists the recurring schedules.
type ScheduleListCmd struct {
	global *GlobalOptions
}

// Execute implements flags.Commander.
func (c *ScheduleListCmd) Execute([]string) error {
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		ss, err := svc.ListSchedules(ctx)
		if err != nil {
			return err
		}
		return cliutil.PrintJSON(ss)
	})
}

// Operations available on a single schedule.
const (
	opShow = iota
	opPause
	opResume
	opDelete
)

// ScheduleCmd runs an operation on a schedule given by name.
type ScheduleCmd struct {
	global *GlobalOptions
	op     int
	Args   struct {
		Name string `positional-arg-name:"nome" required:"true"`
	} `positional-args:"yes"`
}

// Execute implements flags.Commander.
func (c *ScheduleCmd) Execute([]string) error {
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		name := c.Args.Name
		switch c.op {
		case opPause:
			return svc.PauseSchedule(ctx, name)
		case opResume:
			return svc.ResumeSchedule(ctx, name)
		case opDelete:
			return svc.DeleteSchedule(ctx, name)
		default:
			s, err := svc.GetSchedule(ctx, name)
			if err != nil {
				return err
			}
			return cliutil.PrintJSON(s)
		}
	})
}

// DecideCmd approves or rejects a deploy awaiting approval.
type DecideCmd struct {
	global  *GlobalOptions
	approve bool
	Comment string `short:"m" long:"comment" description:"commento registrato con la decisione"`
	Args    idArgs `positional-args:"yes"`
}

// Execute implements flags.Commander.
func (c *DecideCmd) Execute([]string) error {
	if !c.approve && c.Comment == "" {
		return errors.New("give the reason for the rejection with --comment")
	}
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		if err := svc.Decide(ctx, c.Args.ID, service.DecisionRequest{Approve: c.approve, Comment: c.Comment}); err != nil {
			return err
		}
		// The decision is processed by the approval workflow: the status updates right after.
		msg := "approvazione registrata:"
		if !c.approve {
			msg = "rifiuto registrato:"
		}
		fmt.Println(msg, c.Args.ID, "(manent status per lo stato)")
		return nil
	})
}

// redeployOptions are the options shared by promote and rollback.
type redeployOptions struct {
	ID             string        `long:"id" description:"ID del nuovo deploy (chiave di idempotenza)"`
	DryRun         bool          `long:"dry-run" description:"simula il deploy"`
	FreezeOverride string        `long:"freeze-override" description:"motivo della deroga a un freeze attivo (richiede il permesso deployment.freeze-override)"`
	NoWait         bool          `long:"no-wait" description:"non attendere la conclusione del deploy"`
	Timeout        time.Duration `long:"timeout" description:"tempo massimo di attesa (0 = nessun limite)"`
}

func (o redeployOptions) redeploy() service.Redeploy {
	return service.Redeploy{ID: o.ID, DryRun: o.DryRun, FreezeOverride: o.FreezeOverride}
}

// PromoteCmd releases to an environment the artifact already released to another one.
type PromoteCmd struct {
	global      *GlobalOptions
	Application string `short:"a" long:"application" required:"true" description:"applicazione"`
	From        string `long:"from" required:"true" description:"ambiente di origine"`
	To          string `long:"to" required:"true" description:"ambiente di destinazione"`
	Artifact    string `short:"A" long:"artifact" description:"artefatto da promuovere (obbligatorio se l'ambiente di origine ne ha più di uno)"`
	Version     string `long:"version" description:"versione da promuovere (default: quella in esercizio nell'ambiente di origine)"`
	DeployFile  string `long:"deploy-file" description:"file YAML con la sezione deploy (policy, plugin, targets, params) per l'ambiente di destinazione"`
	redeployOptions
}

// Execute implements flags.Commander.
func (c *PromoteCmd) Execute([]string) error {
	r := service.PromoteRequest{Application: c.Application, Artifact: c.Artifact, From: c.From, To: c.To, Version: c.Version, Redeploy: c.redeploy()}
	if c.DeployFile != "" {
		data, err := os.ReadFile(c.DeployFile)
		if err != nil {
			return err
		}
		var dep descriptor.Deploy
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(&dep); err != nil {
			return fmt.Errorf("%s: invalid deploy section: %w", c.DeployFile, err)
		}
		r.Deploy = &dep
	}
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		sub, err := svc.Promote(ctx, r)
		if err != nil {
			return err
		}
		return follow(ctx, svc, sub, c.NoWait, c.Timeout)
	})
}

// RollbackCmd goes back to a version previously released in the environment.
type RollbackCmd struct {
	global      *GlobalOptions
	Application string `short:"a" long:"application" required:"true" description:"applicazione"`
	Environment string `short:"e" long:"environment" required:"true" description:"ambiente"`
	Artifact    string `short:"A" long:"artifact" description:"artefatto da riportare indietro (obbligatorio se l'ambiente ne ha più di uno)"`
	Version     string `long:"to" description:"versione a cui tornare (default: la precedente a quella in esercizio)"`
	redeployOptions
}

// Execute implements flags.Commander.
func (c *RollbackCmd) Execute([]string) error {
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		sub, err := svc.Rollback(ctx, service.RollbackRequest{Application: c.Application, Artifact: c.Artifact, Environment: c.Environment, Version: c.Version,
			Redeploy: c.redeploy()})
		if err != nil {
			return err
		}
		return follow(ctx, svc, sub, c.NoWait, c.Timeout)
	})
}

// InventoryCmd shows the release in service per application and environment.
type InventoryCmd struct {
	global      *GlobalOptions
	Application string `short:"a" long:"application" description:"solo questa applicazione"`
	Environment string `short:"e" long:"environment" description:"solo questo ambiente"`
	Tenant      string `short:"t" long:"tenant" description:"solo questo tenant"`
	Artifact    string `short:"A" long:"artifact" description:"solo questo artefatto (artifact.name)"`
	Selector    string `long:"selector" description:"solo le applicazioni scelte dal selettore sui tag (es. product=checkout)"`
	OutOfSync   bool   `long:"out-of-sync" description:"solo gli ambienti con target cambiati fuori da manent (riconciliazione)"`
}

// Execute implements flags.Commander.
func (c *InventoryCmd) Execute([]string) error {
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		inv, err := svc.Inventory(ctx, service.InventoryFilter{Application: c.Application, Environment: c.Environment, Tenant: c.Tenant, Artifact: c.Artifact, Selector: c.Selector})
		if err != nil {
			return err
		}
		if c.OutOfSync {
			inv = slices.DeleteFunc(inv, func(e service.InventoryEntry) bool { return !e.OutOfSync })
		}
		return cliutil.PrintJSON(inv)
	})
}

// FreezesCmd lists the active and future freeze windows.
type FreezesCmd struct {
	global      *GlobalOptions
	Application string `short:"a" long:"application" description:"solo le finestre di questa applicazione"`
	Environment string `short:"e" long:"environment" description:"solo le finestre di questo ambiente"`
}

// Execute implements flags.Commander.
func (c *FreezesCmd) Execute([]string) error {
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		ws, err := svc.Freezes(ctx, c.Application, c.Environment)
		if err != nil {
			return err
		}
		return cliutil.PrintJSON(ws)
	})
}

// WhoamiCmd shows the identity the client uses with the server and its permissions.
type WhoamiCmd struct {
	global *GlobalOptions
}

// Execute implements flags.Commander.
func (c *WhoamiCmd) Execute([]string) error {
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		id, err := svc.Me(ctx)
		if err != nil {
			return err
		}
		return cliutil.PrintJSON(id)
	})
}

// ApprovalsCmd lists the waiting deploys the user can approve.
type ApprovalsCmd struct {
	global *GlobalOptions
}

// Execute implements flags.Commander.
func (c *ApprovalsCmd) Execute([]string) error {
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		deps, err := svc.Approvals(ctx)
		if err != nil {
			return err
		}
		return cliutil.PrintJSON(deps)
	})
}

// StepsCmd shows the step timeline of a deploy.
type StepsCmd struct {
	global *GlobalOptions
	Args   idArgs `positional-args:"yes"`
}

// Execute implements flags.Commander.
func (c *StepsCmd) Execute([]string) error {
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		steps, err := svc.Steps(ctx, c.Args.ID)
		if err != nil {
			return err
		}
		return cliutil.PrintJSON(steps)
	})
}

// LogsCmd shows the log of a deploy and, with --follow, waits for new lines until the end.
type LogsCmd struct {
	global   *GlobalOptions
	Follow   bool          `short:"f" long:"follow" description:"attende le righe nuove fino alla conclusione del deploy"`
	JSON     bool          `long:"json" description:"una riga JSON per voce"`
	Interval time.Duration `long:"interval" default:"2s" description:"intervallo di lettura con --follow"`
	Args     idArgs        `positional-args:"yes"`
}

// Execute implements flags.Commander.
func (c *LogsCmd) Execute([]string) error {
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		ctx, stop := cliutil.SignalContext()
		defer stop()
		offset := 0
		for {
			p, err := svc.Logs(ctx, c.Args.ID, offset)
			if err != nil {
				return err
			}
			for _, l := range p.Lines {
				c.print(l)
			}
			offset = p.Next
			if !c.Follow || p.Done {
				return nil
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(c.Interval):
			}
		}
	})
}

func (c *LogsCmd) print(l service.LogLine) {
	if c.JSON {
		b, _ := json.Marshal(l)
		fmt.Println(string(b))
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %-5s", l.Time.Local().Format("2006-01-02 15:04:05"), l.Level)
	if l.Step != "" {
		fmt.Fprintf(&b, " [%s]", l.Step)
	}
	b.WriteString(" " + l.Message)
	for _, k := range slices.Sorted(maps.Keys(l.Attrs)) {
		fmt.Fprintf(&b, " %s=%s", k, l.Attrs[k])
	}
	fmt.Println(b.String())
}

// DORACmd shows the DORA metrics.
type DORACmd struct {
	global      *GlobalOptions
	Application string `short:"a" long:"application" description:"solo questa applicazione"`
	Environment string `short:"e" long:"environment" description:"solo questo ambiente"`
	Since       string `long:"since" default:"30d" description:"inizio del periodo: giorni (30d), durata (720h) o data RFC 3339"`
	Tenant      string `short:"t" long:"tenant" description:"solo questo tenant"`
	Artifact    string `short:"A" long:"artifact" description:"solo questo artefatto (artifact.name)"`
	ByTenant    bool   `long:"by-tenant" description:"metriche anche per tenant"`
}

// Execute implements flags.Commander.
func (c *DORACmd) Execute([]string) error {
	since, err := service.ParseSince(c.Since, time.Now())
	if err != nil {
		return err
	}
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		rep, err := svc.DORA(ctx, service.DORAFilter{Application: c.Application, Environment: c.Environment, Tenant: c.Tenant, Artifact: c.Artifact, ByTenant: c.ByTenant, Since: since})
		if err != nil {
			return err
		}
		return cliutil.PrintJSON(rep)
	})
}
