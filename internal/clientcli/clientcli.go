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

	"github.com/joeg-ita/manent/internal/apiclient"
	"github.com/joeg-ita/manent/internal/cliutil"
	"github.com/joeg-ita/manent/internal/service"
)

// DefaultServer is the server used when none is configured: the local "manent-server serve".
const DefaultServer = "http://127.0.0.1:8080"

// GlobalOptions are the options shared by all commands. They take precedence over environment
// variables, which in turn take precedence over the client configuration file. The token is
// read from MANENT_TOKEN or from a file, never from an option, which would stay in the shell history.
type GlobalOptions struct {
	Config    string `short:"c" long:"config" env:"MANENT_CLIENT_CONFIG" description:"file di configurazione del client (default ~/.config/manent/client.yml)"`
	Server    string `long:"server" env:"MANENT_SERVER" description:"URL del server manent"`
	TokenFile string `long:"token-file" env:"MANENT_TOKEN_FILE" description:"file con il token (in alternativa la variabile MANENT_TOKEN)"`
	CAFile    string `long:"ca-file" env:"MANENT_CA_FILE" description:"certificato della CA del server, se non è tra quelle di sistema"`
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
	p.LongDescription = "manent: client del server di deploy as code (manent-server)"

	add := func(name, short string, cmd any) {
		if _, err := p.AddCommand(name, short, short, cmd); err != nil {
			panic(err)
		}
	}
	g := &global
	add("deploy", "Esegue, pianifica o schedula un deploy da descrittore YAML", &DeployCmd{global: g})
	add("validate", "Verifica un descrittore sul server e mostra il piano e il deploy effettivo", &ValidateCmd{global: g})
	add("status", "Mostra lo stato di un deploy", &StatusCmd{global: g})
	add("list", "Elenca i deploy più recenti", &ListCmd{global: g})
	add("cancel", "Annulla un deploy in attesa o in esecuzione", &CancelCmd{global: g})
	add("logs", "Mostra il log di un deploy", &LogsCmd{global: g})
	add("steps", "Mostra la timeline degli step di un deploy", &StepsCmd{global: g})
	add("approve", "Approva un deploy in attesa di approvazione", &DecideCmd{global: g, approve: true})
	add("reject", "Respinge un deploy in attesa di approvazione", &DecideCmd{global: g})
	add("promote", "Rilascia in un ambiente l'artefatto già rilasciato in un altro", &PromoteCmd{global: g})
	add("rollback", "Torna a una versione rilasciata in precedenza", &RollbackCmd{global: g})
	add("inventory", "Mostra la release in esercizio per applicazione e ambiente", &InventoryCmd{global: g})
	add("approvals", "Elenca i deploy in attesa che puoi approvare", &ApprovalsCmd{global: g})
	add("activity", "Mostra i deploy in esecuzione, in attesa di approvazione, in coda e pianificati", &ActivityCmd{global: g})
	add("workers", "Elenca i worker con stato, heartbeat e deploy in esecuzione (permesso platform.read)", &WorkersCmd{global: g})
	add("whoami", "Mostra la tua identità sul server e i tuoi permessi", &WhoamiCmd{global: g})
	add("freezes", "Elenca le finestre di freeze attive e future", &FreezesCmd{global: g})
	add("dora", "Calcola le metriche DORA dallo storico dei deploy", &DORACmd{global: g})
	add("version", "Mostra la versione del client", &cliutil.VersionCmd{})
	if err := cliutil.AddGroup(p, "schedule", "Gestisce le schedule dei deploy ricorrenti",
		"Gestisce le schedule dei deploy ricorrenti (create con schedule.cron nel descrittore)", []cliutil.Command{
			{Name: "list", Short: "Elenca le schedule", Cmd: &ScheduleListCmd{global: g}},
			{Name: "show", Short: "Mostra una schedule", Cmd: &ScheduleCmd{global: g, op: opShow}},
			{Name: "pause", Short: "Sospende una schedule", Cmd: &ScheduleCmd{global: g, op: opPause}},
			{Name: "resume", Short: "Riattiva una schedule sospesa", Cmd: &ScheduleCmd{global: g, op: opResume}},
			{Name: "delete", Short: "Elimina una schedule", Cmd: &ScheduleCmd{global: g, op: opDelete}},
		}); err != nil {
		return err
	}
	if err := cliutil.AddGroup(p, "release", "Gestisce le release composte (più deploy con dipendenze)",
		"Gestisce le release composte: più deploy con dipendenze tra loro, in parallelo dove possibile", []cliutil.Command{
			{Name: "submit", Short: "Accoda una release da un file YAML e ne segue l'avanzamento", Cmd: &ReleaseSubmitCmd{global: g}},
			{Name: "status", Short: "Mostra lo stato di una release", Cmd: &ReleaseStatusCmd{global: g}},
			{Name: "list", Short: "Elenca le release più recenti", Cmd: &ReleaseListCmd{global: g}},
			{Name: "cancel", Short: "Annulla i deploy non conclusi di una release", Cmd: &ReleaseCancelCmd{global: g}},
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
		return cfg, fmt.Errorf("%s: configurazione del client non valida: %w", path, err)
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
			return nil, fmt.Errorf("lettura del token: %w", err)
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
		fmt.Fprintln(os.Stderr, "deploy in attesa di approvazione: manent approve", sub.ID)
		return cliutil.PrintJSON(sub)
	}
	if noWait || sub.ID == "" || sub.ScheduledAt != nil {
		return cliutil.PrintJSON(sub)
	}
	if sub.State == service.StateWaitingDependency {
		fmt.Fprintln(os.Stderr, "deploy in attesa delle dipendenze:", sub.ID)
	}
	kind := "deploy"
	if sub.DryRun {
		kind = "dry-run"
	}
	fmt.Fprintln(os.Stderr, kind, "accodato:", sub.ID, "(manent logs -f", sub.ID+" per seguirlo)")
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	res, err := svc.Wait(ctx, sub.ID)
	if err != nil {
		return fmt.Errorf("%s %s fallito: %w", kind, sub.ID, err)
	}
	return cliutil.PrintJSON(res)
}
