package clientcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/manent-deployer/manent/internal/service"
)

// outputOptions choose the output format: tables and live progress on a terminal, JSON in
// pipes and scripts (as before) or with --json.
type outputOptions struct {
	JSON bool `long:"json" description:"output JSON (the default when stdout is not a terminal)"`
}

func (o outputOptions) human() bool { return !o.JSON && stdoutIsTerminal() }

// stdoutIsTerminal reports whether stdout is a terminal; a variable for the tests.
var stdoutIsTerminal = func() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// progressInterval is how often the progress of a followed deploy is read.
var progressInterval = 700 * time.Millisecond

// palette colors the human output, unless NO_COLOR is set (https://no-color.org).
type palette struct{ on bool }

func newPalette() palette { return palette{on: os.Getenv("NO_COLOR") == ""} }

func (p palette) wrap(code, s string) string {
	if !p.on {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}

func (p palette) green(s string) string  { return p.wrap("32", s) }
func (p palette) red(s string) string    { return p.wrap("31", s) }
func (p palette) yellow(s string) string { return p.wrap("33", s) }
func (p palette) cyan(s string) string   { return p.wrap("36", s) }
func (p palette) dim(s string) string    { return p.wrap("2", s) }
func (p palette) bold(s string) string   { return p.wrap("1", s) }

// state colors a deploy state: green when succeeded, red when failed, yellow while waiting.
func (p palette) state(s string) string {
	switch s {
	case service.StateSuccess:
		return p.green(s)
	case service.StatePending, service.StateEnqueued, service.StateDelayed:
		return p.cyan(s)
	case service.StateAwaitingApproval, service.StateWaitingDependency:
		return p.yellow(s)
	}
	if service.Terminal(s) {
		return p.red(s)
	}
	return s
}

// hiddenStep reports the bookkeeping steps the progress does not show: DBOS operations,
// notifications, callbacks, audit, policy checks, the checkpoints read before a step and the
// single health gate reads (a line per stage shows the gate's outcome).
func hiddenStep(name string) bool {
	for _, prefix := range []string{"DBOS.", "callback:", "notify:", "checkpoint:", "policy@", "change:status", "gate@"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return name == "audit" || name == "now" || name == "policy" || name == "waiters" || name == "cleanup"
}

// outcome is the word for how a deploy ended: succeeded, failed, cancelled, rejected...
func outcome(state string) string {
	switch state {
	case service.StateSuccess:
		return "succeeded"
	case service.StateError:
		return "failed"
	}
	return strings.ToLower(strings.ReplaceAll(state, "_", " "))
}

// cell is a table cell: the text and how to color it once padded.
type cell struct {
	text  string
	color func(string) string
}

func plain(s string) cell { return cell{text: s} }

// table prints aligned columns two spaces apart. Unlike tabwriter it pads before coloring, so
// the escape sequences do not shift the columns.
type table struct{ rows [][]cell }

func (t *table) add(cells ...cell) { t.rows = append(t.rows, cells) }

func (t *table) header(names ...string) {
	cells := make([]cell, len(names))
	for i, n := range names {
		cells[i] = plain(n)
	}
	t.add(cells...)
}

func (t *table) print(out io.Writer) error {
	var widths []int
	for _, r := range t.rows {
		for i, c := range r {
			if i == len(widths) {
				widths = append(widths, 0)
			}
			widths[i] = max(widths[i], utf8.RuneCountInString(c.text))
		}
	}
	var b strings.Builder
	for _, r := range t.rows {
		var line strings.Builder
		for i, c := range r {
			text := c.text
			if i < len(r)-1 {
				text += strings.Repeat(" ", widths[i]-utf8.RuneCountInString(c.text)+2)
			}
			if c.color != nil && c.text != "" {
				// Only the text is colored, not the padding.
				text = c.color(c.text) + text[len(c.text):]
			}
			line.WriteString(text)
		}
		b.WriteString(strings.TrimRight(line.String(), " ") + "\n")
	}
	_, err := io.WriteString(out, b.String())
	return err
}

// shortDuration formats d with a tenth of a second under a minute, to the second above.
func shortDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return d.Round(time.Second).String()
}

// release describes what a deploy releases: "shop 1.4.0 → prod", with the artifact when it
// differs from the application.
func release(s *service.Status) string {
	app := s.Application
	if s.Artifact != "" && s.Artifact != s.Application {
		app += "/" + s.Artifact
	}
	return strings.TrimSpace(app+" "+s.Version) + " → " + s.Environment
}

// watch follows a deploy on a terminal: it prints every step as it ends and, at the end, a
// summary line. When the deploy does not succeed it returns the cause, which the CLI prints
// after the summary.
func watch(ctx context.Context, svc service.Service, id, kind string, out io.Writer) error {
	p := newPalette()
	printed := map[int]bool{}
	stages := map[string]string{}
	for {
		st, err := svc.Status(ctx, id)
		if err != nil {
			return err
		}
		// Steps only once started: the queued deploy has none.
		if st.StartedAt != nil {
			steps, err := svc.Steps(ctx, id)
			if err != nil {
				return err
			}
			for _, s := range steps {
				if printed[s.ID] || (s.CompletedAt == nil && s.Error == "") {
					continue
				}
				printed[s.ID] = true
				if hiddenStep(s.Name) {
					continue
				}
				d := shortDuration(time.Duration(s.DurationMs) * time.Millisecond)
				if s.Error != "" {
					fmt.Fprintf(out, "  %s %-32s %s  %s\n", p.red("✗"), s.Name, p.dim(d), p.red(firstLine(s.Error)))
				} else {
					fmt.Fprintf(out, "  %s %-32s %s\n", p.green("✓"), s.Name, p.dim(d))
				}
			}
		}
		if st.Rollout != nil {
			printStages(out, p, st.Rollout, stages)
		}
		if service.Terminal(st.State) {
			took := shortDuration(st.UpdatedAt.Sub(st.CreatedAt))
			if st.State == service.StateSuccess {
				fmt.Fprintf(out, "%s %s %s in %s\n", p.green("✓"), p.bold(release(st)), kind+" succeeded", took)
				return nil
			}
			fmt.Fprintf(out, "%s %s %s after %s\n", p.red("✗"), p.bold(release(st)), kind+" "+outcome(st.State), took)
			if st.Rollback != "" {
				fmt.Fprintf(out, "  automatic rollback: %s\n", st.Rollback)
			}
			return errors.New(firstNonEmpty(st.Error, st.State))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s %s failed: %w", kind, id, ctx.Err())
		case <-time.After(progressInterval):
		}
	}
}

// printStages prints a line when a rollout stage starts its health gate or ends; seen holds
// the states already printed.
func printStages(out io.Writer, p palette, r *service.RolloutState, seen map[string]string) {
	for _, s := range r.Stages {
		if seen[s.Name] == s.State {
			continue
		}
		seen[s.Name] = s.State
		hosts := 0
		for _, pass := range s.Passes {
			hosts += len(pass.Hosts)
		}
		switch s.State {
		case service.StageBaking:
			fmt.Fprintf(out, "  %s %-32s %s\n", p.cyan("◷"), "wave "+s.Name, p.dim(plural(hosts, "host")+" released, health gate running"))
		case service.StageSucceeded:
			fmt.Fprintf(out, "  %s %-32s %s\n", p.green("✓"), "wave "+s.Name, p.green("healthy"))
		case service.StageFailed:
			// The details (probe, metric) are in the deploy's error, printed at the end.
			msg := "failed"
			if s.Gate != "" {
				msg = "health gate red"
			}
			fmt.Fprintf(out, "  %s %-32s %s\n", p.red("✗"), "wave "+s.Name, p.red(msg))
		case service.StagePending, service.StageRunning:
		default:
			// Rolled back, skipped and future states: shown as they are (the red gate is in the
			// deploy's error, printed at the end).
			fmt.Fprintf(out, "  %s %-32s %s\n", p.yellow("↺"), "wave "+s.Name, p.yellow(strings.ReplaceAll(s.State, "-", " ")))
		}
	}
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// printDeploys prints a deploy listing as a table.
func printDeploys(out io.Writer, deps []service.Status, now time.Time) error {
	if len(deps) == 0 {
		fmt.Fprintln(out, "No deploys.")
		return nil
	}
	p := newPalette()
	var t table
	t.header("ID", "APPLICATION", "ENV", "VERSION", "KIND", "STATE", "BY", "CREATED", "DURATION")
	for _, d := range deps {
		app := d.Application
		if d.Artifact != "" && d.Artifact != d.Application {
			app += "/" + d.Artifact
		}
		kind := dash(d.Kind)
		if d.DryRun {
			kind += " (dry-run)"
		}
		took := "-"
		if service.Terminal(d.State) {
			took = shortDuration(d.UpdatedAt.Sub(d.CreatedAt))
		}
		t.add(plain(d.ID), plain(dash(app)), plain(dash(d.Environment)), cell{dash(d.Version), p.bold}, plain(kind),
			cell{d.State, p.state}, plain(dash(d.RequestedBy)), plain(ago(&d.CreatedAt, now)), plain(took))
	}
	return t.print(out)
}

// printInventory prints the release in service per application and environment.
func printInventory(out io.Writer, inv []service.InventoryEntry, now time.Time) error {
	if len(inv) == 0 {
		fmt.Fprintln(out, "No releases.")
		return nil
	}
	p := newPalette()
	var t table
	t.header("APPLICATION", "ENV", "VERSION", "DEPLOYED", "BY", "NOTES")
	for _, e := range inv {
		app := e.Application
		if e.Artifact != "" && e.Artifact != e.Application {
			app += "/" + e.Artifact
		}
		// The applications no tenant lists ("untenanted") are shown without prefix.
		if e.Tenant != "" && e.Tenant != "untenanted" {
			app = e.Tenant + "/" + app
		}
		version, when, by := "-", "-", "-"
		if c := e.Current; c != nil {
			version, when, by = dash(c.Version), ago(&c.UpdatedAt, now), dash(c.RequestedBy)
		}
		// Notes are warnings: drift, targets changed by hand, a failure after the release in service.
		var notes []string
		if e.Drift {
			notes = append(notes, "drift")
		}
		if e.OutOfSync {
			notes = append(notes, "targets changed outside manent")
		}
		if f := e.LastFailure; f != nil {
			notes = append(notes, fmt.Sprintf("last failure: %s %s", dash(f.Version), outcome(f.State)))
		}
		t.add(plain(app), plain(e.Environment), cell{version, p.bold}, plain(when), plain(by), cell{strings.Join(notes, ", "), p.yellow})
	}
	return t.print(out)
}

// printStatus prints the state of a deploy as a list of fields.
func printStatus(out io.Writer, s *service.Status, now time.Time) error {
	p := newPalette()
	var t table
	row := func(k, v string, color ...func(string) string) {
		if v == "" {
			return
		}
		c := cell{text: v}
		if len(color) > 0 {
			c.color = color[0]
		}
		t.add(cell{k, p.dim}, c)
	}
	row("ID", s.ID)
	row("State", s.State, p.state)
	row("Release", release(s))
	row("Tenant", s.Tenant)
	kind := dash(s.Kind)
	if s.DryRun {
		kind += " (dry-run)"
	}
	row("Kind", kind)
	row("Requested by", s.RequestedBy)
	row("Created", s.CreatedAt.Local().Format("2006-01-02 15:04:05")+" ("+ago(&s.CreatedAt, now)+")")
	if s.ScheduledAt != nil {
		row("Scheduled", at(s.ScheduledAt, now))
	}
	if service.Terminal(s.State) {
		row("Duration", shortDuration(s.UpdatedAt.Sub(s.CreatedAt)))
	}
	row("Worker", s.Executor)
	if s.Rollout != nil {
		row("Rollout", fmt.Sprintf("%d/%d stages %s", len(s.Rollout.Completed()), len(s.Rollout.Stages), s.Rollout.Status))
	}
	if s.Approval != nil {
		row("Approval", approvalOf(*s))
	}
	row("Rollback", s.Rollback)
	row("SHA-256", s.SHA256)
	if s.Error != "" {
		row("Error", firstLine(s.Error), p.red)
	}
	return t.print(out)
}

// contextAttrs are the log attributes that repeat what the deploy already says (its ID,
// application and environment) or describe manent itself: not shown on a terminal.
var contextAttrs = map[string]bool{"component": true, "deployment": true, "executor": true, "application": true, "environment": true}

// humanLogLine formats a log line for a terminal: time only, colored level and step, without
// the context attributes.
func humanLogLine(l service.LogLine) string {
	p := newPalette()
	level := fmt.Sprintf("%-5s", l.Level)
	switch l.Level {
	case "WARN":
		level = p.yellow(level)
	case "ERROR":
		level = p.red(level)
	default:
		level = p.dim(level)
	}
	var b strings.Builder
	b.WriteString(p.dim(l.Time.Local().Format("15:04:05")) + " " + level)
	if l.Step != "" {
		b.WriteString(" " + p.cyan("["+l.Step+"]"))
	}
	b.WriteString(" " + l.Message)
	for _, k := range slices.Sorted(maps.Keys(l.Attrs)) {
		if v := l.Attrs[k]; !contextAttrs[k] && v != "" {
			b.WriteString(" " + p.dim(k+"=") + shortDigest(v))
		}
	}
	return b.String()
}

// shortDigest shortens a sha256 in hex to its first 12 digits, as git does with commits: the
// full value is in the JSON output.
func shortDigest(v string) string {
	if len(v) != 64 || strings.Trim(v, "0123456789abcdef") != "" {
		return v
	}
	return v[:12] + "…"
}

// levelRank orders the log levels (slog names, case-insensitive): an empty or unknown level is
// the lowest, so without --level every line is shown.
func levelRank(level string) int {
	switch strings.ToUpper(level) {
	case "DEBUG":
		return 1
	case "INFO":
		return 2
	case "WARN":
		return 3
	case "ERROR":
		return 4
	}
	return 0
}
