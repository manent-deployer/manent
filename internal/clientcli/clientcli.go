// Package clientcli implements the manent client CLI (cmd/manent): it reads YAML descriptors and
// talks only to the server's HTTP API, so authentication, permissions and audit always apply.
// It must not import engine, DBOS, plugins or database drivers: deps_test.go checks it.
package clientcli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jessevdk/go-flags"
	"gopkg.in/yaml.v3"

	"github.com/manent-deployer/manent/internal/apiclient"
	"github.com/manent-deployer/manent/internal/cliutil"
	"github.com/manent-deployer/manent/internal/service"
)

// DefaultServer is the server used when none is configured: the local "manent-server serve".
const DefaultServer = "http://127.0.0.1:8080"

// GlobalOptions are the options shared by all commands. They take precedence over environment
// variables, which in turn take precedence over the client configuration file. The token is
// read from MANENT_TOKEN or from a file, never from an option, which would stay in the shell history.
type GlobalOptions struct {
	Config    string `short:"c" long:"config" env:"MANENT_CLIENT_CONFIG" description:"client configuration file (default ~/.config/manent/client.yml)"`
	Server    string `long:"server" env:"MANENT_SERVER" description:"URL of the manent server"`
	TokenFile string `long:"token-file" env:"MANENT_TOKEN_FILE" description:"file with the token (or the MANENT_TOKEN variable)"`
	CAFile    string `long:"ca-file" env:"MANENT_CA_FILE" description:"CA certificate of the server, if not among the system ones"`
}

// ClientConfig is the client configuration file.
type ClientConfig struct {
	Server    string        `yaml:"server"`
	TokenFile string        `yaml:"token_file"`
	CAFile    string        `yaml:"ca_file"`
	Timeout   time.Duration `yaml:"timeout"`
}

// Run runs the CLI with the given arguments.
func Run(args []string) error {
	var global GlobalOptions
	p := flags.NewParser(&global, flags.Default)
	p.Name = "manent"
	p.LongDescription = "manent: client of the deploy as code server (manent-server)"

	add := func(name, short string, cmd any) {
		if _, err := p.AddCommand(name, short, short, cmd); err != nil {
			panic(err)
		}
	}
	g := &global
	add("deploy", "Runs, schedules or plans a deploy from a YAML descriptor", &DeployCmd{global: g})
	add("validate", "Checks a descriptor on the server and shows the plan and the effective deploy", &ValidateCmd{global: g})
	add("status", "Shows the state of a deploy", &StatusCmd{global: g})
	add("list", "Lists the most recent deploys", &ListCmd{global: g})
	add("cancel", "Cancels a waiting or running deploy", &CancelCmd{global: g})
	add("logs", "Shows the log of a deploy", &LogsCmd{global: g})
	add("steps", "Shows the timeline of the steps of a deploy", &StepsCmd{global: g})
	add("approve", "Approves a deploy awaiting approval", &DecideCmd{global: g, approve: true})
	add("reject", "Rejects a deploy awaiting approval", &DecideCmd{global: g})
	add("promote", "Releases to an environment the artifact already released to another", &PromoteCmd{global: g})
	add("rollback", "Goes back to a previously released version", &RollbackCmd{global: g})
	add("inventory", "Shows the release in service per application and environment", &InventoryCmd{global: g})
	add("approvals", "Lists the waiting deploys you can approve", &ApprovalsCmd{global: g})
	add("activity", "Shows the deploys running, awaiting approval, queued and scheduled", &ActivityCmd{global: g})
	add("workers", "Lists the workers with state, heartbeat, resources, queues and running deploys (platform.read permission)", &WorkersCmd{global: g})
	add("queues", "Lists the queues with limits, queued, running and scheduled deploys and listening workers (platform.read permission)", &QueuesCmd{global: g})
	add("whoami", "Shows your identity on the server and your permissions", &WhoamiCmd{global: g})
	add("freezes", "Lists the active and future freeze windows", &FreezesCmd{global: g})
	add("dora", "Computes the DORA metrics from the deploy history", &DORACmd{global: g})
	add("version", "Shows the client version", &cliutil.VersionCmd{})
	if err := cliutil.AddGroup(p, "schedule", "Manages the schedules of recurring deploys",
		"Manages the schedules of recurring deploys (created with schedule.cron in the descriptor)", []cliutil.Command{
			{Name: "list", Short: "Lists the schedules", Cmd: &ScheduleListCmd{global: g}},
			{Name: "show", Short: "Shows a schedule", Cmd: &ScheduleCmd{global: g, op: opShow}},
			{Name: "pause", Short: "Pauses a schedule", Cmd: &ScheduleCmd{global: g, op: opPause}},
			{Name: "resume", Short: "Resumes a paused schedule", Cmd: &ScheduleCmd{global: g, op: opResume}},
			{Name: "delete", Short: "Deletes a schedule", Cmd: &ScheduleCmd{global: g, op: opDelete}},
		}); err != nil {
		return err
	}
	if err := cliutil.AddGroup(p, "policy", "Manages the revisions of the governance policy (Rego)",
		"Manages the revisions of the governance policy evaluated by OPA: draft upload, tests, impact, approval and rollback", policyCommands(g)); err != nil {
		return err
	}
	if err := cliutil.AddGroup(p, "tenants", "Lists the tenants of the registry",
		"Lists the tenants of the registry, with tags and owned applications", []cliutil.Command{
			{Name: "list", Short: "Lists the tenants, filtered by owned application and selector", Cmd: &TenantsListCmd{global: g}},
			{Name: "show", Short: "Shows a tenant with the applications it owns and their tags", Cmd: &TenantShowCmd{global: g}},
		}); err != nil {
		return err
	}
	if err := cliutil.AddGroup(p, "freeze", "Creates and deletes freeze windows at runtime",
		"Creates and deletes freeze windows without changing manent.yml or restarting (e.g. an emergency freeze)", freezeCommands(g)); err != nil {
		return err
	}
	if err := cliutil.AddGroup(p, "env", "Checks environment configurations",
		"Checks on the server a candidate environment configuration, before applying it", []cliutil.Command{
			{Name: "validate", Short: "Checks an environment configuration as a deploy would use it", Cmd: &EnvValidateCmd{global: g}},
		}); err != nil {
		return err
	}
	if err := cliutil.AddGroup(p, "release", "Manages composite releases (several deploys with dependencies)",
		"Manages composite releases: several deploys with dependencies between them, in parallel where possible", []cliutil.Command{
			{Name: "submit", Short: "Enqueues a release from a YAML file and follows its progress", Cmd: &ReleaseSubmitCmd{global: g}},
			{Name: "status", Short: "Shows the state of a release", Cmd: &ReleaseStatusCmd{global: g}},
			{Name: "list", Short: "Lists the most recent releases", Cmd: &ReleaseListCmd{global: g}},
			{Name: "cancel", Short: "Cancels the deploys of a release not yet ended", Cmd: &ReleaseCancelCmd{global: g}},
		}); err != nil {
		return err
	}
	return cliutil.Parse(p, args)
}

// defaultConfigPath is the client configuration file: $XDG_CONFIG_HOME/manent/client.yml
// or ~/.config/manent/client.yml.
func defaultConfigPath() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "manent", "client.yml")
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".config", "manent", "client.yml")
	}
	return ""
}

// loadConfig reads the client configuration file: the given one, which must exist, or the
// default one, if it exists.
func loadConfig(path string) (ClientConfig, error) {
	var cfg ClientConfig
	explicit := path != ""
	if !explicit {
		path = defaultConfigPath()
	}
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) && !explicit {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("%s: invalid client configuration: %w", path, err)
	}
	cfg.TokenFile, cfg.CAFile = expandHome(cfg.TokenFile), expandHome(cfg.CAFile)
	return cfg, nil
}

// expandHome replaces a leading "~/" with the user's home.
func expandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, rest)
		}
	}
	return p
}

// newClient creates the API client: options and environment variables take precedence over
// the configuration file. The token comes from MANENT_TOKEN or the token file.
func newClient(g *GlobalOptions) (*apiclient.Client, error) {
	cfg, err := loadConfig(g.Config)
	if err != nil {
		return nil, err
	}
	server := firstNonEmpty(g.Server, cfg.Server, DefaultServer)
	token := os.Getenv("MANENT_TOKEN")
	if tf := firstNonEmpty(g.TokenFile, cfg.TokenFile); tf != "" && (token == "" || g.TokenFile != "") {
		b, err := os.ReadFile(tf)
		if err != nil {
			return nil, fmt.Errorf("reading the token: %w", err)
		}
		token = strings.TrimSpace(string(b))
	}
	return apiclient.NewWithOptions(apiclient.Options{
		Server: server, Token: token, CAFile: firstNonEmpty(g.CAFile, cfg.CAFile), Timeout: cfg.Timeout,
	})
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// withService runs fn with the API client.
func withService(g *GlobalOptions, fn func(context.Context, service.Service) error) error {
	c, err := newClient(g)
	if err != nil {
		return err
	}
	return fn(context.Background(), c)
}

// follow shows the outcome of the submission and, if requested, waits for the deploy to end.
// It does not wait for scheduled deploys nor for those awaiting approval.
func follow(ctx context.Context, svc service.Service, sub *service.Submission, noWait bool, timeout time.Duration) error {
	if sub.State == service.StateAwaitingApproval {
		fmt.Fprintln(os.Stderr, "deploy awaiting approval: manent approve", sub.ID)
		return cliutil.PrintJSON(sub)
	}
	if noWait || sub.ID == "" || sub.ScheduledAt != nil {
		return cliutil.PrintJSON(sub)
	}
	if sub.State == service.StateWaitingDependency {
		fmt.Fprintln(os.Stderr, "deploy waiting for its dependencies:", sub.ID)
	}
	kind := "deploy"
	if sub.DryRun {
		kind = "dry-run"
	}
	fmt.Fprintln(os.Stderr, kind, "enqueued:", sub.ID, "(manent logs -f", sub.ID+" to follow it)")
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	res, err := svc.Wait(ctx, sub.ID)
	if err != nil {
		return fmt.Errorf("%s %s failed: %w", kind, sub.ID, err)
	}
	return cliutil.PrintJSON(res)
}
