package clientcli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/joeg-ita/manent/internal/cliutil"
	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/service"
)

// policyCommands are the subcommands of "manent policy".
func policyCommands(g *GlobalOptions) []cliutil.Command {
	return []cliutil.Command{
		{Name: "list", Short: "Elenca le revisioni e la policy in vigore", Cmd: &PolicyListCmd{global: g}},
		{Name: "show", Short: "Mostra una revisione con i suoi file (current: quella in vigore)", Cmd: &PolicyShowCmd{global: g}},
		{Name: "pull", Short: "Scrive in una directory i file di una revisione", Cmd: &PolicyPullCmd{global: g}},
		{Name: "push", Short: "Carica come bozza una revisione da una directory (*.rego, data.json, data.yaml)", Cmd: &PolicyPushCmd{global: g}},
		{Name: "diff", Short: "Mostra le differenze tra due revisioni", Cmd: &PolicyDiffCmd{global: g}},
		{Name: "eval", Short: "Valuta una revisione su un descrittore", Cmd: &PolicyEvalCmd{global: g}},
		{Name: "impact", Short: "Ricalcola con una revisione le decisioni dei deploy reali di un periodo", Cmd: &PolicyImpactCmd{global: g}},
		{Name: "access", Short: "Simula i permessi di un'identità con una revisione (rbac.engine: opa)", Cmd: &PolicyAccessCmd{global: g}},
		{Name: "approve", Short: "Approva e attiva una revisione in bozza", Cmd: &PolicyActivateCmd{global: g, draft: true}},
		{Name: "activate", Short: "Torna a una revisione già attiva in passato", Cmd: &PolicyActivateCmd{global: g}},
	}
}

// freezeCommands are the subcommands of "manent freeze".
func freezeCommands(g *GlobalOptions) []cliutil.Command {
	return []cliutil.Command{
		{Name: "add", Short: "Crea una finestra di freeze a runtime (permesso freeze.write)", Cmd: &FreezeAddCmd{global: g}},
		{Name: "delete", Short: "Elimina una finestra di freeze creata a runtime", Cmd: &FreezeDeleteCmd{global: g}},
		{Name: "list", Short: "Elenca le finestre di freeze attive e future", Cmd: &FreezesCmd{global: g}},
	}
}

// revisionArg is the positional argument with a revision number or "current".
type revisionArg struct {
	Revision string `positional-arg-name:"revisione" required:"true"`
}

// parseRevision reads a revision number; "current" is 0.
func parseRevision(v string) (int64, error) {
	if v == "current" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid revision %q: a number or current", v)
	}
	return n, nil
}

// PolicyListCmd lists the policy revisions.
type PolicyListCmd struct {
	global *GlobalOptions
}

// Execute implements flags.Commander.
func (c *PolicyListCmd) Execute([]string) error {
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		l, err := svc.Policies(ctx)
		if err != nil {
			return err
		}
		return cliutil.PrintJSON(l)
	})
}

// PolicyShowCmd shows a revision.
type PolicyShowCmd struct {
	global *GlobalOptions
	Args   revisionArg `positional-args:"yes"`
}

// Execute implements flags.Commander.
func (c *PolicyShowCmd) Execute([]string) error {
	rev, err := parseRevision(c.Args.Revision)
	if err != nil {
		return err
	}
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		p, err := svc.PolicyRevision(ctx, rev)
		if err != nil {
			return err
		}
		return cliutil.PrintJSON(p)
	})
}

// PolicyPullCmd writes the files of a revision to a directory.
type PolicyPullCmd struct {
	global *GlobalOptions
	Dir    string      `short:"d" long:"dir" required:"true" description:"directory in cui scrivere i file"`
	Args   revisionArg `positional-args:"yes"`
}

// Execute implements flags.Commander.
func (c *PolicyPullCmd) Execute([]string) error {
	rev, err := parseRevision(c.Args.Revision)
	if err != nil {
		return err
	}
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		p, err := svc.PolicyRevision(ctx, rev)
		if err != nil {
			return err
		}
		for _, name := range slices.Sorted(maps.Keys(p.Files)) {
			clean := path.Clean(name)
			if strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") || clean == ".." {
				return fmt.Errorf("unsafe file path in the revision: %q", name)
			}
			dst := filepath.Join(c.Dir, filepath.FromSlash(clean))
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(dst, []byte(p.Files[name]), 0o644); err != nil {
				return err
			}
		}
		fmt.Fprintf(os.Stderr, "revisione %d: %d file scritti in %s\n", p.Revision, len(p.Files), c.Dir)
		return nil
	})
}

// readPolicyDir reads the *.rego and data.json/data.yaml files of a directory, hidden ones
// excluded.
func readPolicyDir(dir string) (map[string]string, error) {
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != dir && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		base := d.Name()
		if !strings.HasSuffix(base, ".rego") && base != "data.json" && base != "data.yaml" && base != "data.yml" {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no policy file (*.rego) in %s", dir)
	}
	return files, nil
}

// PolicyPushCmd uploads a draft revision.
type PolicyPushCmd struct {
	global  *GlobalOptions
	Dir     string `short:"d" long:"dir" required:"true" description:"directory con i moduli Rego e i dati"`
	Comment string `short:"m" long:"message" description:"descrizione della modifica"`
}

// Execute implements flags.Commander.
func (c *PolicyPushCmd) Execute([]string) error {
	files, err := readPolicyDir(c.Dir)
	if err != nil {
		return err
	}
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		p, err := svc.PushPolicy(ctx, service.PolicyPush{Files: files, Comment: c.Comment})
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "revisione %d in bozza: per attivarla, manent policy approve %d (da un'altra identità se è richiesta la revisione)\n", p.Revision, p.Revision)
		return cliutil.PrintJSON(p)
	})
}

// PolicyDiffCmd shows the differences between two revisions.
type PolicyDiffCmd struct {
	global *GlobalOptions
	Args   struct {
		From string `positional-arg-name:"da" required:"true"`
		To   string `positional-arg-name:"a" required:"true"`
	} `positional-args:"yes"`
}

// Execute implements flags.Commander.
func (c *PolicyDiffCmd) Execute([]string) error {
	from, err := parseRevision(c.Args.From)
	if err != nil {
		return err
	}
	to, err := parseRevision(c.Args.To)
	if err != nil {
		return err
	}
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		a, err := svc.PolicyRevision(ctx, from)
		if err != nil {
			return err
		}
		b, err := svc.PolicyRevision(ctx, to)
		if err != nil {
			return err
		}
		fmt.Print(diffFiles(a.Files, b.Files, strconv.FormatInt(a.Revision, 10), strconv.FormatInt(b.Revision, 10)))
		return nil
	})
}

// diffFiles describes the differences between two sets of files, line by line.
func diffFiles(a, b map[string]string, la, lb string) string {
	var sb strings.Builder
	names := slices.Sorted(maps.Keys(maps.Collect(func(yield func(string, struct{}) bool) {
		for k := range a {
			if !yield(k, struct{}{}) {
				return
			}
		}
		for k := range b {
			if !yield(k, struct{}{}) {
				return
			}
		}
	})))
	for _, n := range names {
		x, inA := a[n]
		y, inB := b[n]
		if inA && inB && x == y {
			continue
		}
		fmt.Fprintf(&sb, "--- %s/%s\n+++ %s/%s\n", la, n, lb, n)
		for _, l := range diffLines(splitLines(x), splitLines(y)) {
			sb.WriteString(l)
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// diffLines is a line diff by longest common subsequence (policies are small files).
func diffLines(a, b []string) []string {
	n, m := len(a), len(b)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out []string
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			out = append(out, "  "+a[i])
			i, j = i+1, j+1
		case lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, "- "+a[i])
			i++
		default:
			out = append(out, "+ "+b[j])
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, "- "+a[i])
	}
	for ; j < m; j++ {
		out = append(out, "+ "+b[j])
	}
	return out
}

// PolicyEvalCmd evaluates a revision on a descriptor.
type PolicyEvalCmd struct {
	global *GlobalOptions
	File   string      `short:"f" long:"file" required:"true" description:"descrittore YAML"`
	Phase  string      `long:"phase" choice:"submit" choice:"validate" choice:"start" choice:"stage" choice:"tick" description:"fase della valutazione (default submit)"`
	Kind   string      `long:"kind" choice:"deploy" choice:"promotion" choice:"rollback" description:"tipo di deploy (default deploy)"`
	Args   revisionArg `positional-args:"yes"`
}

// Execute implements flags.Commander.
func (c *PolicyEvalCmd) Execute([]string) error {
	rev, err := parseRevision(c.Args.Revision)
	if err != nil {
		return err
	}
	d, err := descriptor.LoadYAMLFile(c.File)
	if err != nil {
		return err
	}
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		ev, err := svc.EvaluatePolicy(ctx, rev, service.PolicyEvaluationRequest{Descriptor: d, Phase: c.Phase, Kind: c.Kind})
		if err != nil {
			return err
		}
		return cliutil.PrintJSON(ev)
	})
}

// PolicyImpactCmd recomputes the decisions of the deploys of a period.
type PolicyImpactCmd struct {
	global *GlobalOptions
	Since  string      `long:"since" default:"7d" description:"periodo: durata (720h) o giorni (30d)"`
	Args   revisionArg `positional-args:"yes"`
}

// Execute implements flags.Commander.
func (c *PolicyImpactCmd) Execute([]string) error {
	rev, err := parseRevision(c.Args.Revision)
	if err != nil {
		return err
	}
	now := time.Now()
	from, err := service.ParseSince(c.Since, now)
	if err != nil {
		return err
	}
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		imp, err := svc.PolicyImpact(ctx, rev, now.Sub(from))
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "%d deploy valutati, %d con una decisione diversa\n", imp.Evaluated, len(imp.Changes))
		return cliutil.PrintJSON(imp)
	})
}

// PolicyAccessCmd simulates with a revision the permissions of an identity given by hand.
type PolicyAccessCmd struct {
	global      *GlobalOptions
	Subject     string            `long:"subject" required:"true" description:"identità <metodo>:<nome> (es. oidc:alice)"`
	Groups      []string          `long:"group" description:"gruppo dell'identità (ripetibile)"`
	Claims      map[string]string `long:"claim" key-value-delimiter:"=" description:"claim dell'identità chiave=valore (ripetibile)"`
	Compare     bool              `long:"compare" description:"mostra i permessi guadagnati e persi rispetto alla revisione attiva"`
	Action      string            `long:"action" description:"azione di cui simulare l'esito (es. deployment.submit)"`
	Application string            `short:"a" long:"application" description:"applicazione dell'azione"`
	Environment string            `short:"e" long:"environment" description:"ambiente dell'azione"`
	Artifact    string            `short:"A" long:"artifact" description:"artefatto dell'azione (binding con artifacts)"`
	File        string            `short:"f" long:"file" description:"descrittore YAML del deploy (artefatto per i divieti)"`
	JSON        bool              `long:"json" description:"output JSON"`
	Args        revisionArg       `positional-args:"yes"`
}

// Execute implements flags.Commander.
func (c *PolicyAccessCmd) Execute([]string) error {
	rev, err := parseRevision(c.Args.Revision)
	if err != nil {
		return err
	}
	r := service.PolicyAccessRequest{Subject: c.Subject, Groups: c.Groups, Claims: c.Claims, Compare: c.Compare}
	if c.Action != "" {
		r.Check = &service.AccessCheck{Action: c.Action, Application: c.Application, Environment: c.Environment, Artifact: c.Artifact}
		if c.File != "" {
			if r.Check.Descriptor, err = descriptor.LoadYAMLFile(c.File); err != nil {
				return err
			}
		}
	} else if c.File != "" || c.Application != "" || c.Environment != "" {
		return errors.New("--application, --environment, --tenant and --file need --action")
	}
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		acc, err := svc.PolicyAccess(ctx, rev, r)
		if err != nil {
			return err
		}
		if c.JSON {
			return cliutil.PrintJSON(acc)
		}
		printAccess(os.Stdout, acc)
		return nil
	})
}

// printAccess prints the simulation as tables: grants, differences and outcome of the action.
func printAccess(out io.Writer, acc *service.PolicyAccess) {
	fmt.Fprintf(out, "Identità %s, revisione %s\n\n", acc.Subject, acc.Revision)
	printGrants(out, acc.Grants, "")
	for _, w := range acc.Warnings {
		fmt.Fprintf(out, "attenzione: permesso scartato: %s\n", w)
	}
	if acc.Compared != "" {
		fmt.Fprintf(out, "\nRispetto alla revisione attiva %s:", acc.Compared)
		if len(acc.Gained)+len(acc.Lost) == 0 {
			fmt.Fprintln(out, " nessuna differenza")
		} else {
			fmt.Fprintln(out)
			printGrants(out, acc.Gained, "+ ")
			printGrants(out, acc.Lost, "- ")
		}
	}
	if ch := acc.Check; ch != nil {
		fmt.Fprintln(out)
		switch {
		case ch.Allowed:
			fmt.Fprintf(out, "Esito: consentito (permesso %s)\n", ch.GrantedBy)
		case len(ch.Denied) > 0:
			fmt.Fprintf(out, "Esito: negato dalla policy (permesso %s)\n", ch.GrantedBy)
			for _, d := range ch.Denied {
				fmt.Fprintf(out, "  - %s\n", d)
			}
		default:
			fmt.Fprintf(out, "Esito: negato: %s\n", ch.Reason)
		}
	}
}

func printGrants(out io.Writer, gs []service.Grant, prefix string) {
	if len(gs) == 0 && prefix == "" {
		fmt.Fprintln(out, "nessun permesso")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if prefix == "" {
		fmt.Fprintln(tw, "PERMESSO\tAZIONI\tAPPLICAZIONI\tAMBIENTI\tTENANT\tFINO A")
	}
	for _, g := range gs {
		acts := g.Role
		if acts == "" {
			acts = strings.Join(g.Actions, ",")
		}
		until := "-"
		if g.Until != nil {
			until = g.Until.Local().Format(time.DateTime)
		}
		fmt.Fprintf(tw, "%s%s\t%s\t%s\t%s\t%s\t%s\n", prefix, g.Name, acts, strings.Join(g.Applications, ","),
			strings.Join(g.Environments, ","), cmp.Or(g.Tenants, "*"), until)
	}
	tw.Flush()
}

// PolicyActivateCmd approves a draft or goes back to a previous revision.
type PolicyActivateCmd struct {
	global *GlobalOptions
	draft  bool
	Args   revisionArg `positional-args:"yes"`
}

// Execute implements flags.Commander.
func (c *PolicyActivateCmd) Execute([]string) error {
	rev, err := parseRevision(c.Args.Revision)
	if err != nil {
		return err
	}
	if rev == 0 {
		return errors.New("give the number of the revision to activate")
	}
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		op := svc.ActivatePolicy
		if c.draft {
			op = svc.ApprovePolicy
		}
		if err := op(ctx, rev); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "revisione %d attiva\n", rev)
		return nil
	})
}

// FreezeAddCmd creates a freeze window at runtime.
type FreezeAddCmd struct {
	global       *GlobalOptions
	Name         string   `long:"name" required:"true" description:"nome della finestra"`
	Reason       string   `long:"reason" description:"motivo, mostrato a chi prova a rilasciare"`
	Applications []string `short:"a" long:"application" description:"applicazioni (pattern con *, ripetibile; default *)"`
	Environments []string `short:"e" long:"environment" required:"true" description:"ambienti (pattern con *, ripetibile)"`
	Regions      []string `long:"region" description:"solo queste region (ripetibile)"`
	Tenants      string   `long:"tenants" description:"solo i deploy dei tenant scelti dal selettore (es. name=acme, tier=gold)"`
	Artifacts    []string `short:"A" long:"artifact" description:"solo questi artefatti (pattern con *, ripetibile)"`
	Selector     string   `long:"selector" description:"solo le applicazioni scelte dal selettore sui tag (es. criticality=high)"`
	From         string   `long:"from" description:"inizio, RFC 3339 (default adesso)"`
	Until        string   `long:"until" description:"fine, RFC 3339"`
	For          string   `long:"for" description:"durata dall'inizio, in alternativa a --until (es. 4h)"`
	Hard         bool     `long:"hard" description:"non esentabile dalla policy né superabile con freezeOverride"`
}

// Execute implements flags.Commander.
func (c *FreezeAddCmd) Execute([]string) error {
	r := service.FreezeRequest{Name: c.Name, Reason: c.Reason, Applications: c.Applications, Environments: c.Environments, Regions: c.Regions, Tenants: c.Tenants, Artifacts: c.Artifacts, Selector: c.Selector, Hard: c.Hard}
	if len(r.Applications) == 0 {
		r.Applications = []string{"*"}
	}
	start := time.Now()
	if c.From != "" {
		t, err := time.Parse(time.RFC3339, c.From)
		if err != nil {
			return fmt.Errorf("--from: %w", err)
		}
		r.From, start = t, t
	}
	switch {
	case c.Until != "" && c.For != "":
		return errors.New("--until and --for are alternatives")
	case c.Until != "":
		t, err := time.Parse(time.RFC3339, c.Until)
		if err != nil {
			return fmt.Errorf("--until: %w", err)
		}
		r.To = t
	case c.For != "":
		d, err := time.ParseDuration(c.For)
		if err != nil || d <= 0 {
			return fmt.Errorf("--for: invalid duration %q", c.For)
		}
		r.To = start.Add(d)
	default:
		return errors.New("give the end of the freeze with --until or --for")
	}
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		w, err := svc.CreateFreeze(ctx, r)
		if err != nil {
			return err
		}
		return cliutil.PrintJSON(w)
	})
}

// FreezeDeleteCmd deletes a freeze window created at runtime.
type FreezeDeleteCmd struct {
	global *GlobalOptions
	Args   struct {
		Name string `positional-arg-name:"nome" required:"true"`
	} `positional-args:"yes"`
}

// Execute implements flags.Commander.
func (c *FreezeDeleteCmd) Execute([]string) error {
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		if err := svc.DeleteFreeze(ctx, c.Args.Name); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "freeze %s eliminato\n", c.Args.Name)
		return nil
	})
}
