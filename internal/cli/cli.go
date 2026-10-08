// Package cli implementa i comandi da riga di comando (go-flags) con descrittori YAML.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jessevdk/go-flags"

	"github.com/joeg-ita/go-dart/internal/api"
	"github.com/joeg-ita/go-dart/internal/app"
	"github.com/joeg-ita/go-dart/internal/config"
	"github.com/joeg-ita/go-dart/internal/descriptor"
	"github.com/joeg-ita/go-dart/internal/pipeline"
)

// GlobalOptions sono le opzioni comuni a tutti i comandi.
type GlobalOptions struct {
	Config string `short:"c" long:"config" env:"GODART_CONFIG" description:"file di configurazione YAML"`
}

// Run esegue la CLI con gli argomenti indicati.
func Run(args []string) error {
	var global GlobalOptions
	p := flags.NewParser(&global, flags.Default)
	p.LongDescription = "go-dart: deploy as code basato su workflow durevoli DBOS"

	add := func(name, short string, cmd any) {
		if _, err := p.AddCommand(name, short, short, cmd); err != nil {
			panic(err)
		}
	}
	add("deploy", "Esegue un deploy da descrittore YAML", &DeployCmd{global: &global})
	add("validate", "Valida un descrittore YAML e mostra il piano", &ValidateCmd{global: &global})
	add("status", "Mostra lo stato di un deploy", &StatusCmd{global: &global})
	add("serve", "Avvia il web server API", &ServeCmd{global: &global})

	_, err := p.ParseArgs(args)
	var ferr *flags.Error
	if errors.As(err, &ferr) && ferr.Type == flags.ErrHelp {
		return nil
	}
	return err
}

// DeployCmd accoda un deploy e, di default, ne attende la conclusione.
type DeployCmd struct {
	global *GlobalOptions
	File   string `short:"f" long:"file" required:"true" description:"descrittore di deploy YAML"`
	NoWait bool   `long:"no-wait" description:"non attendere la conclusione del deploy"`
}

// Execute implementa flags.Commander.
func (c *DeployCmd) Execute([]string) error {
	d, err := descriptor.LoadYAMLFile(c.File)
	if err != nil {
		return err
	}
	a, err := newApp(c.global)
	if err != nil {
		return err
	}
	defer a.Close()

	ctx := context.Background()
	id, err := a.Engine.Submit(ctx, d)
	if err != nil {
		return err
	}
	fmt.Println("deploy accodato:", id)
	if c.NoWait {
		return nil
	}
	res, err := a.Engine.Wait(ctx, id)
	if err != nil {
		return fmt.Errorf("deploy %s fallito: %w", id, err)
	}
	return printJSON(res)
}

// ValidateCmd valida il descrittore senza avviare DBOS.
type ValidateCmd struct {
	global *GlobalOptions
	File   string `short:"f" long:"file" required:"true" description:"descrittore di deploy YAML"`
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
	reg, err := app.NewSteps(cfg, plugins, log)
	if err != nil {
		return err
	}
	plan, err := pipeline.ConfigPlanner{Steps: cfg.Pipeline.Steps, Registry: reg}.Plan(d)
	if err != nil {
		return err
	}
	return printJSON(map[string]any{"valid": true, "plan": plan})
}

// StatusCmd mostra lo stato di un deploy.
type StatusCmd struct {
	global *GlobalOptions
	Args   struct {
		ID string `positional-arg-name:"id" required:"true"`
	} `positional-args:"yes"`
}

// Execute implementa flags.Commander.
func (c *StatusCmd) Execute([]string) error {
	a, err := newApp(c.global)
	if err != nil {
		return err
	}
	defer a.Close()
	st, err := a.Engine.Status(context.Background(), c.Args.ID)
	if err != nil {
		return err
	}
	return printJSON(st)
}

// ServeCmd avvia il server HTTP e lo arresta in modo pulito su SIGINT/SIGTERM.
type ServeCmd struct {
	global *GlobalOptions
	Listen string `short:"l" long:"listen" description:"indirizzo di ascolto (sovrascrive la configurazione)"`
}

// Execute implementa flags.Commander.
func (c *ServeCmd) Execute([]string) error {
	a, err := newApp(c.global)
	if err != nil {
		return err
	}
	defer a.Close()

	addr := a.Config.HTTP.Listen
	if c.Listen != "" {
		addr = c.Listen
	}
	gin.SetMode(gin.ReleaseMode)
	srv := &http.Server{Addr: addr, Handler: api.NewRouter(a.Engine, a.Logger), ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
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

func newApp(g *GlobalOptions) (*app.App, error) {
	cfg, err := config.Load(g.Config)
	if err != nil {
		return nil, err
	}
	return app.New(cfg)
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
