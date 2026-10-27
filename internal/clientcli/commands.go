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

	"github.com/manent-deployer/manent/internal/cliutil"
	"github.com/manent-deployer/manent/internal/descriptor"
	"github.com/manent-deployer/manent/internal/service"
)

// DeployCmd enqueues a deploy and, by default, waits for it to end.
// Scheduled deploys (schedule in the descriptor) are not waited for.
type DeployCmd struct {
	global  *GlobalOptions
	File    string        `short:"f" long:"file" required:"true" description:"YAML deploy descriptor"`
	NoWait  bool          `long:"no-wait" description:"do not wait for the deploy to end"`
	Timeout time.Duration `long:"timeout" description:"maximum time to wait, e.g. 30m (0 = no limit); the deploy goes on anyway"`
	DryRun  bool          `long:"dry-run" description:"simulates the deploy on a worker: checks configuration, secrets, artifact and targets without changing them"`
	After   []string      `long:"after" description:"ID of a deploy that must succeed before this one (repeatable); added to the descriptor's after"`
	Import  []string      `long:"import" value-name:"NAME=ID" description:"reads the exports of deploy ID as import NAME of the descriptor (repeatable); adds ID to after"`
	ArtifactOverrides
	Outputs string `long:"outputs" description:"file to append the outcome to as key=value lines (deployment_id, state, ...): $GITHUB_OUTPUT or a GitLab dotenv report"`
	Summary string `long:"summary" description:"file to append the deploy summary to, in Markdown ($GITHUB_STEP_SUMMARY)"`
	CI      bool   `long:"ci" description:"detect GitHub Actions (outcomes in $GITHUB_OUTPUT, summary in $GITHUB_STEP_SUMMARY) or GitLab CI (outcomes in manent.env, or $MANENT_DOTENV)"`
	outputOptions
}

// ArtifactOverrides replace fields of the descriptor from the command line: in CI the
// descriptor stays in the repository and the build gives the version.
type ArtifactOverrides struct {
	ID      string `long:"id" description:"deploy ID (idempotency key), instead of the descriptor's"`
	Version string `long:"version" description:"artifact version, instead of the descriptor's"`
	Source  string `long:"source" description:"artifact source, instead of the descriptor's"`
	SHA256  string `long:"sha256" description:"expected sha256 of the artifact"`
	// ConfigRef sets config.ref: in CI the commit of the configuration repository tested with
	// the artifact.
	ConfigRef string `long:"config-ref" description:"revision of the environment configuration (ref or SHA, git provider), within descriptor_policy.config_ref"`
	// Tenant sets the tenant field: it chooses among the tenants that list the application.
	Tenant string `short:"t" long:"tenant" description:"tenant of the application, required if several tenants have an application with this name"`
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
	if o.ConfigRef != "" {
		d.Config = &descriptor.ConfigSource{Ref: o.ConfigRef}
	}
	if o.Tenant != "" {
		d.Tenant = o.Tenant
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
		ferr := follow(ctx, svc, sub, c.NoWait, c.Timeout, c.human())
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
// rollback and governance rules. Unlike "deploy --dry-run" it touches no secrets or targets;
// with --check-artifact the API server also looks for the artifact at its source.
type ValidateCmd struct {
	global        *GlobalOptions
	File          string `short:"f" long:"file" required:"true" description:"YAML deploy descriptor"`
	CheckArtifact bool   `long:"check-artifact" description:"also check from the API server that the artifact exists at its source, without downloading it (requires the dry-run permission)"`
}

// Execute implements flags.Commander.
func (c *ValidateCmd) Execute([]string) error {
	d, err := descriptor.LoadYAMLFile(c.File)
	if err != nil {
		return err
	}
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		v, err := svc.Validate(ctx, d, service.ValidateOptions{CheckArtifact: c.CheckArtifact})
		if err != nil {
			return err
		}
		for _, w := range v.Warnings {
			fmt.Fprintln(os.Stderr, "warning:", w)
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
	outputOptions
}

// Execute implements flags.Commander.
func (c *StatusCmd) Execute([]string) error {
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		st, err := svc.Status(ctx, c.Args.ID)
		if err != nil {
			return err
		}
		if c.human() {
			return printStatus(os.Stdout, st, time.Now())
		}
		return cliutil.PrintJSON(st)
	})
}

// ListCmd lists deploys, with filters on schedule, application, environment and state.
type ListCmd struct {
	global      *GlobalOptions
	Schedule    string `short:"s" long:"schedule" description:"only the deploys generated by this schedule"`
	Application string `short:"a" long:"application" description:"only the deploys of this application"`
	Environment string `short:"e" long:"environment" description:"only the deploys in this environment"`
	Tenant      string `short:"t" long:"tenant" description:"only the deploys of the applications of this tenant"`
	Artifact    string `short:"A" long:"artifact" description:"only the deploys of this artifact (artifact.name)"`
	Selector    string `long:"selector" description:"only the applications chosen by the tag selector (e.g. criticality=high, tenant=payments)"`
	State       string `long:"state" description:"only the deploys in this state (e.g. SUCCESS, ERROR, AWAITING_APPROVAL)"`
	Limit       int    `short:"n" long:"limit" default:"20" description:"maximum number of deploys"`
	outputOptions
}

// Execute implements flags.Commander.
func (c *ListCmd) Execute([]string) error {
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		deps, err := svc.List(ctx, service.ListFilter{Schedule: c.Schedule, Application: c.Application, Environment: c.Environment, Tenant: c.Tenant, Artifact: c.Artifact, Selector: c.Selector, State: c.State, Limit: c.Limit})
		if err != nil {
			return err
		}
		if c.human() {
			return printDeploys(os.Stdout, deps, time.Now())
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
		fmt.Println("deploy cancelled:", c.Args.ID)
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
		Name string `positional-arg-name:"name" required:"true"`
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
	Comment string `short:"m" long:"comment" description:"comment recorded with the decision"`
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
		msg := "approval recorded:"
		if !c.approve {
			msg = "rejection recorded:"
		}
		fmt.Println(msg, c.Args.ID, "(manent status for the state)")
		return nil
	})
}

// redeployOptions are the options shared by promote and rollback.
type redeployOptions struct {
	ID             string        `long:"id" description:"ID of the new deploy (idempotency key)"`
	DryRun         bool          `long:"dry-run" description:"simulates the deploy"`
	FreezeOverride string        `long:"freeze-override" description:"reason to override an active freeze (requires the deployment.freeze-override permission)"`
	NoWait         bool          `long:"no-wait" description:"do not wait for the deploy to end"`
	Timeout        time.Duration `long:"timeout" description:"maximum time to wait (0 = no limit)"`
	outputOptions
}

func (o redeployOptions) redeploy() service.Redeploy {
	return service.Redeploy{ID: o.ID, DryRun: o.DryRun, FreezeOverride: o.FreezeOverride}
}

// PromoteCmd releases to an environment the artifact already released to another one.
type PromoteCmd struct {
	global      *GlobalOptions
	Application string `short:"a" long:"application" required:"true" description:"application"`
	From        string `long:"from" required:"true" description:"source environment"`
	To          string `long:"to" required:"true" description:"destination environment"`
	Artifact    string `short:"A" long:"artifact" description:"artifact to promote (required if the source environment has more than one)"`
	Version     string `long:"version" description:"version to promote (default: the one in service in the source environment)"`
	Tenant      string `short:"t" long:"tenant" description:"tenant of the application, required if several tenants have an application with this name"`
	DeployFile  string `long:"deploy-file" description:"YAML file with the deploy section (policy, plugin, targets, params) for the destination environment"`
	ConfigRef   string `long:"config-ref" description:"revision of the destination environment's configuration (ref or SHA, git provider); default: the default ref"`
	redeployOptions
}

// Execute implements flags.Commander.
func (c *PromoteCmd) Execute([]string) error {
	r := service.PromoteRequest{Tenant: c.Tenant, Application: c.Application, Artifact: c.Artifact, From: c.From, To: c.To, Version: c.Version, ConfigRef: c.ConfigRef, Redeploy: c.redeploy()}
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
		return follow(ctx, svc, sub, c.NoWait, c.Timeout, c.human())
	})
}

// RollbackCmd goes back to a version previously released in the environment.
type RollbackCmd struct {
	global      *GlobalOptions
	Tenant      string `short:"t" long:"tenant" description:"tenant of the application, required if several tenants have an application with this name"`
	Application string `short:"a" long:"application" required:"true" description:"application"`
	Environment string `short:"e" long:"environment" required:"true" description:"environment"`
	Artifact    string `short:"A" long:"artifact" description:"artifact to roll back (required if the environment has more than one)"`
	Version     string `long:"to" description:"version to go back to (default: the one before the version in service)"`
	redeployOptions
}

// Execute implements flags.Commander.
func (c *RollbackCmd) Execute([]string) error {
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		sub, err := svc.Rollback(ctx, service.RollbackRequest{Tenant: c.Tenant, Application: c.Application, Artifact: c.Artifact, Environment: c.Environment, Version: c.Version,
			Redeploy: c.redeploy()})
		if err != nil {
			return err
		}
		return follow(ctx, svc, sub, c.NoWait, c.Timeout, c.human())
	})
}

// InventoryCmd shows the release in service per application and environment.
type InventoryCmd struct {
	global      *GlobalOptions
	Application string `short:"a" long:"application" description:"only this application"`
	Environment string `short:"e" long:"environment" description:"only this environment"`
	Tenant      string `short:"t" long:"tenant" description:"only this tenant"`
	Artifact    string `short:"A" long:"artifact" description:"only this artifact (artifact.name)"`
	Selector    string `long:"selector" description:"only the applications chosen by the tag selector (e.g. product=checkout)"`
	OutOfSync   bool   `long:"out-of-sync" description:"only the environments with targets changed outside manent (reconciliation)"`
	outputOptions
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
		if c.human() {
			return printInventory(os.Stdout, inv, time.Now())
		}
		return cliutil.PrintJSON(inv)
	})
}

// FreezesCmd lists the active and future freeze windows.
type FreezesCmd struct {
	global      *GlobalOptions
	Application string `short:"a" long:"application" description:"only the windows of this application"`
	Environment string `short:"e" long:"environment" description:"only the windows of this environment"`
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
	Follow   bool          `short:"f" long:"follow" description:"waits for new lines until the deploy ends"`
	JSON     bool          `long:"json" description:"one JSON line per entry"`
	Interval time.Duration `long:"interval" default:"2s" description:"read interval with --follow"`
	Level    string        `short:"l" long:"level" choice:"debug" choice:"info" choice:"warn" choice:"error" description:"only the lines at this level or above"`
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
				if levelRank(l.Level) >= levelRank(c.Level) {
					c.print(l)
				}
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
	if stdoutIsTerminal() {
		fmt.Println(humanLogLine(l))
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
	Application string `short:"a" long:"application" description:"only this application"`
	Environment string `short:"e" long:"environment" description:"only this environment"`
	Since       string `long:"since" default:"30d" description:"start of the period: days (30d), duration (720h) or RFC 3339 date"`
	Tenant      string `short:"t" long:"tenant" description:"only this tenant"`
	Artifact    string `short:"A" long:"artifact" description:"only this artifact (artifact.name)"`
	ByTenant    bool   `long:"by-tenant" description:"metrics per tenant too"`
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
