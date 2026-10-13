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

// DeployCmd accoda un deploy e, di default, ne attende la conclusione.
// I deploy pianificati (schedule nel descrittore) non vengono attesi.
type DeployCmd struct {
	global  *GlobalOptions
	File    string        `short:"f" long:"file" required:"true" description:"descrittore di deploy YAML"`
	NoWait  bool          `long:"no-wait" description:"non attendere la conclusione del deploy"`
	Timeout time.Duration `long:"timeout" description:"tempo massimo di attesa, es. 30m (0 = nessun limite); il deploy prosegue comunque"`
	DryRun  bool          `long:"dry-run" description:"simula il deploy su un worker: verifica configurazione, segreti, artefatto e target senza modificarli"`
	After   []string      `long:"after" description:"ID di un deploy che deve riuscire prima di questo (ripetibile); si aggiunge ad after del descrittore"`
}

// Execute implementa flags.Commander.
func (c *DeployCmd) Execute([]string) error {
	d, err := descriptor.LoadYAMLFile(c.File)
	if err != nil {
		return err
	}
	if c.DryRun || len(c.After) > 0 {
		d.DryRun = d.DryRun || c.DryRun
		d.After = append(d.After, c.After...)
		if err := d.Validate(); err != nil {
			return err
		}
	}
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		sub, err := svc.Submit(ctx, d)
		if err != nil {
			return err
		}
		return follow(ctx, svc, sub, c.NoWait, c.Timeout)
	})
}

// ValidateCmd verifica il descrittore sul server, come all'accodamento ma senza avviare il
// deploy, e mostra piano, deploy effettivo (fuso con la configurazione di ambiente), rollback
// e regole di governance. A differenza di "deploy --dry-run" non contatta segreti, artefatto e target.
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

// idArgs è l'argomento posizionale con l'ID di un deploy.
type idArgs struct {
	ID string `positional-arg-name:"id" required:"true"`
}

// StatusCmd mostra lo stato di un deploy.
type StatusCmd struct {
	global *GlobalOptions
	Args   idArgs `positional-args:"yes"`
}

// Execute implementa flags.Commander.
func (c *StatusCmd) Execute([]string) error {
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		st, err := svc.Status(ctx, c.Args.ID)
		if err != nil {
			return err
		}
		return cliutil.PrintJSON(st)
	})
}

// ListCmd elenca i deploy, con filtri per schedule, applicazione, ambiente e stato.
type ListCmd struct {
	global      *GlobalOptions
	Schedule    string `short:"s" long:"schedule" description:"solo i deploy generati da questa schedule"`
	Application string `short:"a" long:"application" description:"solo i deploy di questa applicazione"`
	Environment string `short:"e" long:"environment" description:"solo i deploy in questo ambiente"`
	State       string `long:"state" description:"solo i deploy in questo stato (es. SUCCESS, ERROR, AWAITING_APPROVAL)"`
	Limit       int    `short:"n" long:"limit" default:"20" description:"numero massimo di deploy"`
}

// Execute implementa flags.Commander.
func (c *ListCmd) Execute([]string) error {
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		deps, err := svc.List(ctx, service.ListFilter{Schedule: c.Schedule, Application: c.Application, Environment: c.Environment, State: c.State, Limit: c.Limit})
		if err != nil {
			return err
		}
		return cliutil.PrintJSON(deps)
	})
}

// CancelCmd annulla un deploy.
type CancelCmd struct {
	global *GlobalOptions
	Args   idArgs `positional-args:"yes"`
}

// Execute implementa flags.Commander.
func (c *CancelCmd) Execute([]string) error {
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		if err := svc.Cancel(ctx, c.Args.ID); err != nil {
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
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		ss, err := svc.ListSchedules(ctx)
		if err != nil {
			return err
		}
		return cliutil.PrintJSON(ss)
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

// DecideCmd approva o respinge un deploy in attesa di approvazione.
type DecideCmd struct {
	global  *GlobalOptions
	approve bool
	Comment string `short:"m" long:"comment" description:"commento registrato con la decisione"`
	Args    idArgs `positional-args:"yes"`
}

// Execute implementa flags.Commander.
func (c *DecideCmd) Execute([]string) error {
	if !c.approve && c.Comment == "" {
		return errors.New("indicare il motivo del rifiuto con --comment")
	}
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		if err := svc.Decide(ctx, c.Args.ID, service.DecisionRequest{Approve: c.approve, Comment: c.Comment}); err != nil {
			return err
		}
		// La decisione è elaborata dal workflow di approvazione: lo stato si aggiorna subito dopo.
		msg := "approvazione registrata:"
		if !c.approve {
			msg = "rifiuto registrato:"
		}
		fmt.Println(msg, c.Args.ID, "(manent status per lo stato)")
		return nil
	})
}

// redeployOptions sono le opzioni comuni a promote e rollback.
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

// PromoteCmd rilascia in un ambiente l'artefatto già rilasciato in un altro.
type PromoteCmd struct {
	global      *GlobalOptions
	Application string `short:"a" long:"application" required:"true" description:"applicazione"`
	From        string `long:"from" required:"true" description:"ambiente di origine"`
	To          string `long:"to" required:"true" description:"ambiente di destinazione"`
	Version     string `long:"version" description:"versione da promuovere (default: quella in esercizio nell'ambiente di origine)"`
	DeployFile  string `long:"deploy-file" description:"file YAML con la sezione deploy (policy, plugin, targets, params) per l'ambiente di destinazione"`
	redeployOptions
}

// Execute implementa flags.Commander.
func (c *PromoteCmd) Execute([]string) error {
	r := service.PromoteRequest{Application: c.Application, From: c.From, To: c.To, Version: c.Version, Redeploy: c.redeploy()}
	if c.DeployFile != "" {
		data, err := os.ReadFile(c.DeployFile)
		if err != nil {
			return err
		}
		var dep descriptor.Deploy
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(&dep); err != nil {
			return fmt.Errorf("%s: sezione deploy non valida: %w", c.DeployFile, err)
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

// RollbackCmd torna a una versione rilasciata in precedenza nell'ambiente.
type RollbackCmd struct {
	global      *GlobalOptions
	Application string `short:"a" long:"application" required:"true" description:"applicazione"`
	Environment string `short:"e" long:"environment" required:"true" description:"ambiente"`
	Version     string `long:"to" description:"versione a cui tornare (default: la precedente a quella in esercizio)"`
	redeployOptions
}

// Execute implementa flags.Commander.
func (c *RollbackCmd) Execute([]string) error {
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		sub, err := svc.Rollback(ctx, service.RollbackRequest{Application: c.Application, Environment: c.Environment, Version: c.Version, Redeploy: c.redeploy()})
		if err != nil {
			return err
		}
		return follow(ctx, svc, sub, c.NoWait, c.Timeout)
	})
}

// InventoryCmd mostra la release in esercizio per applicazione e ambiente.
type InventoryCmd struct {
	global      *GlobalOptions
	Application string `short:"a" long:"application" description:"solo questa applicazione"`
	Environment string `short:"e" long:"environment" description:"solo questo ambiente"`
}

// Execute implementa flags.Commander.
func (c *InventoryCmd) Execute([]string) error {
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		inv, err := svc.Inventory(ctx, service.InventoryFilter{Application: c.Application, Environment: c.Environment})
		if err != nil {
			return err
		}
		return cliutil.PrintJSON(inv)
	})
}

// FreezesCmd elenca le finestre di freeze attive e future.
type FreezesCmd struct {
	global      *GlobalOptions
	Application string `short:"a" long:"application" description:"solo le finestre di questa applicazione"`
	Environment string `short:"e" long:"environment" description:"solo le finestre di questo ambiente"`
}

// Execute implementa flags.Commander.
func (c *FreezesCmd) Execute([]string) error {
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		ws, err := svc.Freezes(ctx, c.Application, c.Environment)
		if err != nil {
			return err
		}
		return cliutil.PrintJSON(ws)
	})
}

// WhoamiCmd mostra l'identità con cui il client parla al server e i suoi permessi.
type WhoamiCmd struct {
	global *GlobalOptions
}

// Execute implementa flags.Commander.
func (c *WhoamiCmd) Execute([]string) error {
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		id, err := svc.Me(ctx)
		if err != nil {
			return err
		}
		return cliutil.PrintJSON(id)
	})
}

// ApprovalsCmd elenca i deploy in attesa che l'utente può approvare.
type ApprovalsCmd struct {
	global *GlobalOptions
}

// Execute implementa flags.Commander.
func (c *ApprovalsCmd) Execute([]string) error {
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		deps, err := svc.Approvals(ctx)
		if err != nil {
			return err
		}
		return cliutil.PrintJSON(deps)
	})
}

// StepsCmd mostra la timeline degli step di un deploy.
type StepsCmd struct {
	global *GlobalOptions
	Args   idArgs `positional-args:"yes"`
}

// Execute implementa flags.Commander.
func (c *StepsCmd) Execute([]string) error {
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		steps, err := svc.Steps(ctx, c.Args.ID)
		if err != nil {
			return err
		}
		return cliutil.PrintJSON(steps)
	})
}

// LogsCmd mostra il log di un deploy e, con --follow, attende le righe nuove fino alla fine.
type LogsCmd struct {
	global   *GlobalOptions
	Follow   bool          `short:"f" long:"follow" description:"attende le righe nuove fino alla conclusione del deploy"`
	JSON     bool          `long:"json" description:"una riga JSON per voce"`
	Interval time.Duration `long:"interval" default:"2s" description:"intervallo di lettura con --follow"`
	Args     idArgs        `positional-args:"yes"`
}

// Execute implementa flags.Commander.
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

// DORACmd mostra le metriche DORA.
type DORACmd struct {
	global      *GlobalOptions
	Application string `short:"a" long:"application" description:"solo questa applicazione"`
	Environment string `short:"e" long:"environment" description:"solo questo ambiente"`
	Since       string `long:"since" default:"30d" description:"inizio del periodo: giorni (30d), durata (720h) o data RFC 3339"`
}

// Execute implementa flags.Commander.
func (c *DORACmd) Execute([]string) error {
	since, err := service.ParseSince(c.Since, time.Now())
	if err != nil {
		return err
	}
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		rep, err := svc.DORA(ctx, service.DORAFilter{Application: c.Application, Environment: c.Environment, Since: since})
		if err != nil {
			return err
		}
		return cliutil.PrintJSON(rep)
	})
}
