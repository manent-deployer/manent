package clientcli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/manent-deployer/manent/internal/cliutil"
	"github.com/manent-deployer/manent/internal/service"
)

// WorkersCmd lists the workers with state, heartbeat and running work.
type WorkersCmd struct {
	global *GlobalOptions
	JSON   bool          `long:"json" description:"output JSON"`
	Watch  time.Duration `short:"w" long:"watch" description:"repeats the listing at this interval, e.g. 5s"`
}

// Execute implements flags.Commander.
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

// QueuesCmd lists the DBOS queues with limits, content and listening workers.
type QueuesCmd struct {
	global *GlobalOptions
	JSON   bool          `long:"json" description:"output JSON"`
	Watch  time.Duration `short:"w" long:"watch" description:"repeats the listing at this interval, e.g. 5s"`
}

// Execute implements flags.Commander.
func (c *QueuesCmd) Execute([]string) error {
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		return repeat(ctx, c.Watch, func() error {
			qs, err := svc.Queues(ctx)
			if err != nil {
				return err
			}
			if c.JSON {
				return cliutil.PrintJSON(qs)
			}
			return printQueues(os.Stdout, qs, time.Now())
		})
	})
}

// ActivityCmd shows the deploys running, awaiting approval, queued and scheduled.
type ActivityCmd struct {
	global      *GlobalOptions
	Application string        `short:"a" long:"application" description:"only this application"`
	Environment string        `short:"e" long:"environment" description:"only this environment"`
	Tenant      string        `short:"t" long:"tenant" description:"only this tenant"`
	JSON        bool          `long:"json" description:"output JSON"`
	Watch       time.Duration `short:"w" long:"watch" description:"repeats the listing at this interval, e.g. 5s"`
}

// Execute implements flags.Commander.
func (c *ActivityCmd) Execute([]string) error {
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		return repeat(ctx, c.Watch, func() error {
			a, err := svc.Activity(ctx, service.ActivityFilter{Application: c.Application, Environment: c.Environment, Tenant: c.Tenant})
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

// repeat runs fn once or, with every > 0, periodically until ctx is cancelled, clearing the
// screen before every listing.
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
		_, err := fmt.Fprintln(out, "No worker registered.")
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "WORKER\tSTATE\tREGION\tHOST\tVERSION\tHEARTBEAT\tSTARTED\tRUNNING")
	for _, w := range ws {
		running := fmt.Sprint(len(w.Tasks))
		if w.HiddenTasks > 0 {
			running += fmt.Sprintf(" (+%d not visible)", w.HiddenTasks)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", w.ID, w.State, dash(w.Region), dash(w.Hostname),
			dash(versionOf(w)), ago(w.HeartbeatAt, now), ago(w.StartedAt, now), running)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if err := printResources(out, ws); err != nil {
		return err
	}
	// Detail of the running work, per worker.
	var tasks int
	for _, w := range ws {
		tasks += len(w.Tasks)
	}
	if tasks == 0 {
		return nil
	}
	fmt.Fprintln(out)
	tw = tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "WORKER\tKIND\tDEPLOY\tAPPLICATION/ENVIRONMENT\tREGION\tVERSION\tSINCE")
	for _, w := range ws {
		for _, t := range w.Tasks {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", w.ID, t.Kind, t.DeploymentID, appEnv(t.Application, t.Environment),
				dash(t.Region), dash(t.Version), ago(t.StartedAt, now))
		}
	}
	return tw.Flush()
}

// printResources prints the resources and the queues of the workers that reported them.
func printResources(out io.Writer, ws []service.WorkerInfo) error {
	var rows []service.WorkerInfo
	for _, w := range ws {
		if w.Resources != nil || len(w.Queues) > 0 {
			rows = append(rows, w)
		}
	}
	if len(rows) == 0 {
		return nil
	}
	fmt.Fprintln(out)
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "WORKER\tCPU (USED/AVAILABLE)\tMEMORY (PROCESS/AVAILABLE)\tLOAD\tWORKDIR DISK\tCONCURRENT DEPLOYS\tQUEUES")
	for _, w := range rows {
		cpu, mem, load, disk, conc := "-", "-", "-", "-", "-"
		if r := w.Resources; r != nil {
			cpu = fmt.Sprintf("%.2f/%d", r.ProcessCPU, r.CPULimit)
			if r.CPULimit != r.CPUs {
				cpu += fmt.Sprintf(" (host %d)", r.CPUs)
			}
			mem = bytesIEC(r.ProcessMemory) + "/" + bytesIEC(memoryCapacity(r))
			if r.Load1 > 0 {
				load = fmt.Sprintf("%.2f", r.Load1)
			}
			if r.WorkdirFree > 0 {
				disk = bytesIEC(r.WorkdirFree) + " free"
			}
			if r.Concurrency > 0 {
				conc = fmt.Sprint(r.Concurrency)
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", w.ID, cpu, mem, load, disk, conc, dash(strings.Join(w.Queues, ", ")))
	}
	return tw.Flush()
}

// memoryCapacity is the memory the worker can use: the container limit, otherwise the host memory.
func memoryCapacity(r *service.WorkerResources) uint64 {
	if r.MemoryLimit > 0 {
		return r.MemoryLimit
	}
	return r.MemoryTotal
}

// bytesIEC formats a size with binary units; "-" if unknown.
func bytesIEC(n uint64) string {
	if n == 0 {
		return "-"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

func printQueues(out io.Writer, qs []service.QueueInfo, now time.Time) error {
	if len(qs) == 0 {
		_, err := fmt.Fprintln(out, "No queue.")
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "QUEUE\tKIND\tQUEUED\tRUNNING\tSCHEDULED\tLONGEST WAIT\tLIMITS\tWORKERS")
	for _, q := range qs {
		kind := q.Kind
		if q.Region != "" {
			kind += " " + q.Region
		}
		wait := "-"
		if q.OldestEnqueued != nil {
			wait = now.Sub(*q.OldestEnqueued).Round(time.Second).String()
		}
		workers := fmt.Sprint(len(q.Workers))
		if len(q.Workers) > 0 {
			workers += " (" + strings.Join(q.Workers, ", ") + ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%s\t%s\t%s\n", q.Name, kind, q.Enqueued, q.Running, q.Delayed, wait, queueLimits(q), workers)
	}
	return tw.Flush()
}

// queueLimits describes the concurrency limits of a queue.
func queueLimits(q service.QueueInfo) string {
	var parts []string
	if q.WorkerConcurrency > 0 {
		parts = append(parts, fmt.Sprintf("%d per worker", q.WorkerConcurrency))
	}
	if q.PartitionConcurrency > 0 {
		parts = append(parts, fmt.Sprintf("%d per environment", q.PartitionConcurrency))
	}
	if q.GlobalConcurrency > 0 {
		parts = append(parts, fmt.Sprintf("%d in total", q.GlobalConcurrency))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

func printActivity(out io.Writer, a *service.Activity, now time.Time) error {
	sections := []struct {
		title string
		items []service.Status
		when  func(service.Status) string
		extra func(service.Status) string
	}{
		{"Running", a.Running, func(s service.Status) string { return ago(s.StartedAt, now) }, func(s service.Status) string {
			return strings.TrimSpace(dash(s.Executor) + "  " + stepOf(s))
		}},
		{"Awaiting approval", a.AwaitingApproval, func(s service.Status) string { return ago(&s.CreatedAt, now) }, approvalOf},
		{"Waiting for dependencies", a.WaitingDependency, func(s service.Status) string { return ago(&s.CreatedAt, now) }, dependenciesOf},
		{"Queued", a.Queued, func(s service.Status) string { return ago(&s.CreatedAt, now) }, func(s service.Status) string { return dash(s.Queue) }},
		{"Scheduled", a.Delayed, func(s service.Status) string { return at(s.ScheduledAt, now) }, func(service.Status) string { return "" }},
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
		fmt.Fprintln(out, "No active deploy.")
	}
	if a.Truncated {
		fmt.Fprintln(out, "\nList truncated: there are more active deploys than those examined.")
	}
	return nil
}

func stepOf(s service.Status) string {
	parts := []string{}
	if s.LastStep != "" {
		parts = append(parts, "step "+s.LastStep)
	}
	switch {
	case s.Rollout != nil && s.Rollout.Traffic != nil:
		parts = append(parts, fmt.Sprintf("traffic to the candidate %d%%", s.Rollout.Traffic.Weight))
	case s.RolloutProgress != "":
		parts = append(parts, "waves "+s.RolloutProgress)
	}
	return strings.Join(parts, ", ")
}

func approvalOf(s service.Status) string {
	if s.Approval == nil {
		return ""
	}
	r := fmt.Sprintf("%d/%d approvals", len(s.Approval.Decisions), s.Approval.Required)
	if c := s.Approval.Change; c != nil {
		r += fmt.Sprintf(", change %s %s", c.Number, c.Approval)
	}
	// Between two waves: the last stage passed.
	if s.Rollout != nil {
		last := ""
		for _, st := range s.Rollout.Stages {
			if st.State == service.StageSucceeded {
				last = st.Name
			}
		}
		if last != "" {
			r += ", after stage " + last
		}
	}
	return r
}

// dependenciesOf lists the not yet successful dependencies with their state.
func dependenciesOf(s service.Status) string {
	if s.After == nil {
		return ""
	}
	var parts []string
	for _, d := range s.After.Dependencies {
		if d.State != service.StateSuccess {
			parts = append(parts, d.ID+" ("+d.State+")")
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "waiting for " + strings.Join(parts, ", ")
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

// ago describes how long ago, rounded to the second.
func ago(t *time.Time, now time.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	return now.Sub(*t).Round(time.Second).String() + " ago"
}

// at describes a future time with the time left.
func at(t *time.Time, now time.Time) string {
	if t == nil {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04") + " (in " + t.Sub(now).Round(time.Second).String() + ")"
}
