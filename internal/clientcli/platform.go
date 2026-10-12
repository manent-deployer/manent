package clientcli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/joeg-ita/manent/internal/cliutil"
	"github.com/joeg-ita/manent/internal/service"
)

// WorkersCmd elenca i worker con stato, heartbeat e lavoro in corso.
type WorkersCmd struct {
	global *GlobalOptions
	JSON   bool          `long:"json" description:"output JSON"`
	Watch  time.Duration `short:"w" long:"watch" description:"ripete l'elenco a questo intervallo, es. 5s"`
}

// Execute implementa flags.Commander.
func (c *WorkersCmd) Execute([]string) error {
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		return repeat(ctx, c.Watch, func() error {
			ws, err := svc.Workers(ctx)
			if err != nil {
				return err
			}
			if c.JSON {
				return cliutil.PrintJSON(ws)
			}
			return printWorkers(os.Stdout, ws, time.Now())
		})
	})
}

// ActivityCmd mostra i deploy in esecuzione, in attesa di approvazione, in coda e pianificati.
type ActivityCmd struct {
	global      *GlobalOptions
	Application string        `short:"a" long:"application" description:"solo questa applicazione"`
	Environment string        `short:"e" long:"environment" description:"solo questo ambiente"`
	JSON        bool          `long:"json" description:"output JSON"`
	Watch       time.Duration `short:"w" long:"watch" description:"ripete l'elenco a questo intervallo, es. 5s"`
}

// Execute implementa flags.Commander.
func (c *ActivityCmd) Execute([]string) error {
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		return repeat(ctx, c.Watch, func() error {
			a, err := svc.Activity(ctx, service.ActivityFilter{Application: c.Application, Environment: c.Environment})
			if err != nil {
				return err
			}
			if c.JSON {
				return cliutil.PrintJSON(a)
			}
			return printActivity(os.Stdout, a, time.Now())
		})
	})
}

// repeat esegue fn una volta o, con every > 0, a intervalli finché ctx non viene annullato,
// pulendo lo schermo prima di ogni elenco.
func repeat(ctx context.Context, every time.Duration, fn func() error) error {
	if every <= 0 {
		return fn()
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		fmt.Print("\033[H\033[2J")
		if err := fn(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func printWorkers(out io.Writer, ws []service.WorkerInfo, now time.Time) error {
	if len(ws) == 0 {
		_, err := fmt.Fprintln(out, "Nessun worker registrato.")
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "WORKER\tSTATO\tREGION\tHOST\tVERSIONE\tHEARTBEAT\tAVVIATO\tIN CORSO")
	for _, w := range ws {
		running := fmt.Sprint(len(w.Tasks))
		if w.HiddenTasks > 0 {
			running += fmt.Sprintf(" (+%d non visibili)", w.HiddenTasks)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", w.ID, w.State, dash(w.Region), dash(w.Hostname),
			dash(versionOf(w)), ago(w.HeartbeatAt, now), ago(w.StartedAt, now), running)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	// Dettaglio del lavoro in corso, per worker.
	var tasks int
	for _, w := range ws {
		tasks += len(w.Tasks)
	}
	if tasks == 0 {
		return nil
	}
	fmt.Fprintln(out)
	tw = tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "WORKER\tTIPO\tDEPLOY\tAPPLICAZIONE/AMBIENTE\tREGION\tVERSIONE\tDA")
	for _, w := range ws {
		for _, t := range w.Tasks {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", w.ID, t.Kind, t.DeploymentID, appEnv(t.Application, t.Environment),
				dash(t.Region), dash(t.Version), ago(t.StartedAt, now))
		}
	}
	return tw.Flush()
}

func printActivity(out io.Writer, a *service.Activity, now time.Time) error {
	sections := []struct {
		title string
		items []service.Status
		when  func(service.Status) string
		extra func(service.Status) string
	}{
		{"In esecuzione", a.Running, func(s service.Status) string { return ago(s.StartedAt, now) }, func(s service.Status) string {
			return strings.TrimSpace(dash(s.Executor) + "  " + stepOf(s))
		}},
		{"In attesa di approvazione", a.AwaitingApproval, func(s service.Status) string { return ago(&s.CreatedAt, now) }, approvalOf},
		{"In coda", a.Queued, func(s service.Status) string { return ago(&s.CreatedAt, now) }, func(s service.Status) string { return dash(s.Queue) }},
		{"Pianificati", a.Delayed, func(s service.Status) string { return at(s.ScheduledAt, now) }, func(service.Status) string { return "" }},
	}
	empty := true
	for _, sec := range sections {
		if len(sec.items) == 0 {
			continue
		}
		if !empty {
			fmt.Fprintln(out)
		}
		empty = false
		fmt.Fprintf(out, "%s (%d)\n", sec.title, len(sec.items))
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		for _, s := range sec.items {
			id := s.ID
			if s.DryRun {
				id += " (dry-run)"
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\n", id, appEnv(s.Application, s.Environment), dash(s.Version),
				dash(s.RequestedBy), sec.when(s), sec.extra(s))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	if empty {
		fmt.Fprintln(out, "Nessun deploy attivo.")
	}
	if a.Truncated {
		fmt.Fprintln(out, "\nElenco troncato: i deploy attivi sono più di quelli esaminati.")
	}
	return nil
}

func stepOf(s service.Status) string {
	parts := []string{}
	if s.LastStep != "" {
		parts = append(parts, "step "+s.LastStep)
	}
	if s.RolloutProgress != "" {
		parts = append(parts, "ondate "+s.RolloutProgress)
	}
	return strings.Join(parts, ", ")
}

func approvalOf(s service.Status) string {
	if s.Approval == nil {
		return ""
	}
	r := fmt.Sprintf("%d/%d approvazioni", len(s.Approval.Decisions), s.Approval.Required)
	// Tra due ondate: l'ultimo stadio superato.
	if s.Rollout != nil {
		last := ""
		for _, st := range s.Rollout.Stages {
			if st.State == service.StageSucceeded {
				last = st.Name
			}
		}
		if last != "" {
			r += ", dopo lo stadio " + last
		}
	}
	return r
}

func versionOf(w service.WorkerInfo) string {
	if w.Build != "" && w.Version != "" {
		return w.Version + " (" + w.Build + ")"
	}
	return w.Version
}

func appEnv(app, env string) string {
	if app == "" {
		return "-"
	}
	return app + "/" + env
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ago descrive quanto tempo fa, arrotondato al secondo.
func ago(t *time.Time, now time.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	return now.Sub(*t).Round(time.Second).String() + " fa"
}

// at descrive un orario futuro con il tempo che manca.
func at(t *time.Time, now time.Time) string {
	if t == nil {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04") + " (tra " + t.Sub(now).Round(time.Second).String() + ")"
}
