package clientcli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/joeg-ita/manent/internal/cliutil"
	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/service"
)

// ciReport says where the outcome of a deploy goes: key=value outputs and Markdown summary.
type ciReport struct {
	outputs, summary string
}

// report returns the destinations of the outcome, from the options and, with --ci, from the
// CI system's environment variables.
func (c *DeployCmd) report() (ciReport, error) {
	r := ciReport{outputs: c.Outputs, summary: c.Summary}
	if !c.CI {
		return r, nil
	}
	switch {
	case os.Getenv("GITHUB_ACTIONS") == "true":
		r.outputs = cmp.Or(r.outputs, os.Getenv("GITHUB_OUTPUT"))
		r.summary = cmp.Or(r.summary, os.Getenv("GITHUB_STEP_SUMMARY"))
	case os.Getenv("GITLAB_CI") == "true":
		r.outputs = cmp.Or(r.outputs, os.Getenv("MANENT_DOTENV"), "manent.env")
	default:
		return r, errors.New("--ci: neither GitHub Actions (GITHUB_ACTIONS) nor GitLab CI (GITLAB_CI) detected")
	}
	return r, nil
}

// write appends the outcome of the deploy. The state is read again, so that it is the final one
// after the wait (or the initial one with --no-wait).
func (r ciReport) write(ctx context.Context, svc service.Service, d *descriptor.Descriptor, sub *service.Submission) error {
	if r.outputs == "" && r.summary == "" {
		return nil
	}
	st := &service.Status{ID: sub.ID, State: sub.State}
	if sub.ID != "" {
		if s, err := svc.Status(ctx, sub.ID); err == nil {
			st = s
		}
	}
	var errs []error
	if r.outputs != "" {
		lines := []string{
			"deployment_id=" + sub.ID,
			"state=" + st.State,
			"application=" + d.Application,
			"environment=" + d.Environment,
			"version=" + d.Artifact.Version,
			fmt.Sprintf("dry_run=%t", d.DryRun),
		}
		for _, l := range lines {
			if strings.ContainsAny(l, "\r\n") {
				return fmt.Errorf("value cannot be written to %s: %q", r.outputs, l)
			}
		}
		// The exports of the deploy, for the next jobs of the pipeline.
		for _, name := range slices.Sorted(maps.Keys(st.Exports)) {
			v := st.Exports[name]
			if strings.ContainsAny(v, "\r\n") {
				fmt.Fprintf(os.Stderr, "attenzione: export %s su più righe, non scritto in %s\n", name, r.outputs)
				continue
			}
			lines = append(lines, "export_"+name+"="+v)
		}
		errs = append(errs, appendFile(r.outputs, strings.Join(lines, "\n")+"\n"))
	}
	if r.summary != "" {
		kind := "Deploy"
		if d.DryRun {
			kind = "Dry-run"
		}
		var b strings.Builder
		fmt.Fprintf(&b, "### manent · %s di %s in %s\n\n| | |\n|---|---|\n", kind, d.Application, d.Environment)
		fmt.Fprintf(&b, "| Versione | `%s` |\n| Deploy | `%s` |\n| Esito | **%s** |\n", d.Artifact.Version, sub.ID, st.State)
		if st.RolloutProgress != "" {
			fmt.Fprintf(&b, "| Rollout | %s |\n", st.RolloutProgress)
		}
		if st.Rollback != "" {
			fmt.Fprintf(&b, "| Rollback | %s |\n", st.Rollback)
		}
		if st.Error != "" {
			fmt.Fprintf(&b, "| Errore | %s |\n", strings.ReplaceAll(strings.ReplaceAll(st.Error, "|", "\\|"), "\n", " "))
		}
		errs = append(errs, appendFile(r.summary, b.String()+"\n"))
	}
	return errors.Join(errs...)
}

// appendFile appends text to the file, creating it if missing.
func appendFile(path, text string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("outputs file %s: %w", path, err)
	}
	_, err = f.WriteString(text)
	return errors.Join(err, f.Close())
}

// EnvValidateCmd checks a candidate environment configuration on the server before it is
// applied (a change in the repository of the configurations, a Terraform plan).
type EnvValidateCmd struct {
	global      *GlobalOptions
	Application string `short:"a" long:"application" required:"true" description:"applicazione"`
	Environment string `short:"e" long:"environment" required:"true" description:"ambiente"`
	Artifact    string `short:"A" long:"artifact" description:"artefatto a cui si riferisce la configurazione (template con {artifact})"`
	File        string `short:"f" long:"file" required:"true" description:"configurazione di ambiente YAML o JSON (com'è nel sistema di configurazione)"`
	Descriptor  string `short:"d" long:"descriptor" description:"descrittore di esempio; default la richiesta del deploy in esercizio"`
}

// Execute implements flags.Commander.
func (c *EnvValidateCmd) Execute([]string) error {
	raw, err := os.ReadFile(c.File)
	if err != nil {
		return err
	}
	check := service.EnvironmentCheck{Application: c.Application, Artifact: c.Artifact, Environment: c.Environment, Config: string(raw)}
	if c.Descriptor != "" {
		if check.Descriptor, err = descriptor.LoadYAMLFile(c.Descriptor); err != nil {
			return err
		}
	}
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		v, err := svc.ValidateEnvironment(ctx, check)
		if err != nil {
			return err
		}
		for _, w := range v.Warnings {
			fmt.Fprintln(os.Stderr, "attenzione:", w)
		}
		return cliutil.PrintJSON(v)
	})
}
