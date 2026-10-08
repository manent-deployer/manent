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
	"github.com/joeg-ita/go-dart/internal/engine"
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
	add("deploy", "Esegue, pianifica o schedula un deploy da descrittore YAML", &DeployCmd{global: &global})
	add("validate", "Valida un descrittore YAML e mostra il piano", &ValidateCmd{global: &global})
	add("status", "Mostra lo stato di un deploy", &StatusCmd{global: &global})
	add("list", "Elenca i deploy più recenti", &ListCmd{global: &global})
	add("cancel", "Annulla un deploy in attesa o in esecuzione", &CancelCmd{global: &global})
	add("serve", "Avvia il web server API", &ServeCmd{global: &global})

	sched, err := p.AddCommand("schedule", "Gestisce le schedule dei deploy ricorrenti",
		"Gestisce le schedule dei deploy ricorrenti (create con schedule.cron nel descrittore)", &struct{}{})
	if err != nil {
		return err
	}
	sched.SubcommandsOptional = false
	for _, sc := range []struct {
		name, short string
		cmd         any
	}{
		{"list", "Elenca le schedule", &ScheduleListCmd{global: &global}},
		{"show", "Mostra una schedule", &ScheduleCmd{global: &global, op: opShow}},
		{"pause", "Sospende una schedule", &ScheduleCmd{global: &global, op: opPause}},
		{"resume", "Riattiva una schedule sospesa", &ScheduleCmd{global: &global, op: opResume}},
		{"delete", "Elimina una schedule", &ScheduleCmd{global: &global, op: opDelete}},
	} {
		if _, err := sched.AddCommand(sc.name, sc.short, sc.short, sc.cmd); err != nil {
			return err
		}
	}

	_, err = p.ParseArgs(args)
	var ferr *flags.Error
	if errors.As(err, &ferr) && ferr.Type == flags.ErrHelp {
		return nil
	}
	return err
}

// DeployCmd accoda un deploy e, di default, ne attende la conclusione.
// I deploy pianificati (schedule nel descrittore) non vengono attesi.
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
	sub, err := a.Engine.Submit(ctx, d)
	if err != nil {
		return err
	}
	if c.NoWait || sub.ID == "" || sub.ScheduledAt != nil {
		return printJSON(sub)
	}
	fmt.Println("deploy accodato:", sub.ID)
	res, err := a.Engine.Wait(ctx, sub.ID)
	if err != nil {
		return fmt.Errorf("deploy %s fallito: %w", sub.ID, err)
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
	return withApp(c.global, func(a *app.App) error {
		st, err := a.Engine.Status(context.Background(), c.Args.ID)
		if err != nil {
			return err
		}
		return printJSON(st)
	})
}

// ListCmd elenca i deploy, opzionalmente solo quelli generati da una schedule.
type ListCmd struct {
	global   *GlobalOptions
	Schedule string `short:"s" long:"schedule" description:"solo i deploy generati da questa schedule"`
	Limit    int    `short:"n" long:"limit" default:"20" description:"numero massimo di deploy"`
}

// Execute implementa flags.Commander.
func (c *ListCmd) Execute([]string) error {
	return withApp(c.global, func(a *app.App) error {
		deps, err := a.Engine.List(context.Background(), engine.ListFilter{Schedule: c.Schedule, Limit: c.Limit})
		if err != nil {
			return err
		}
		return printJSON(deps)
	})
}

// CancelCmd annulla un deploy.
type CancelCmd struct {
	global *GlobalOptions
	Args   struct {
		ID string `positional-arg-name:"id" required:"true"`
	} `positional-args:"yes"`
}

// Execute implementa flags.Commander.
func (c *CancelCmd) Execute([]string) error {
	return withApp(c.global, func(a *app.App) error {
		if err := a.Engine.Cancel(context.Background(), c.Args.ID); err != nil {
			return err
		}
		fmt.Println("deploy annullato:", c.Args.ID)
		return nil
	})
}

// ScheduleListCmd elenca le schedule ricorrenti.
type ScheduleListCmd struct {
	global *GlobalOptions
}

// Execute implementa flags.Commander.
func (c *ScheduleListCmd) Execute([]string) error {
	return withApp(c.global, func(a *app.App) error {
		ss, err := a.Engine.ListSchedules(context.Background())
		if err != nil {
			return err
		}
		return printJSON(ss)
	})
}

// Operazioni disponibili su una singola schedule.
const (
	opShow = iota
	opPause
	opResume
	opDelete
)

// ScheduleCmd esegue un'operazione su una schedule indicata per nome.
type ScheduleCmd struct {
	global *GlobalOptions
	op     int
	Args   struct {
		Name string `positional-arg-name:"nome" required:"true"`
	} `positional-args:"yes"`
}

// Execute implementa flags.Commander.
func (c *ScheduleCmd) Execute([]string) error {
	return withApp(c.global, func(a *app.App) error {
		ctx, name := context.Background(), c.Args.Name
		switch c.op {
		case opPause:
			return a.Engine.PauseSchedule(ctx, name)
		case opResume:
			return a.Engine.ResumeSchedule(ctx, name)
		case opDelete:
			return a.Engine.DeleteSchedule(ctx, name)
		default:
			s, err := a.Engine.GetSchedule(ctx, name)
			if err != nil {
				return err
			}
			return printJSON(s)
		}
	})
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

// withApp avvia l'applicazione, esegue fn e la arresta.
func withApp(g *GlobalOptions, fn func(*app.App) error) error {
	a, err := newApp(g)
	if err != nil {
		return err
	}
	defer a.Close()
	return fn(a)
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
