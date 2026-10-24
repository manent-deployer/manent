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

	"github.com/manent-deployer/manent/internal/cliutil"
	"github.com/manent-deployer/manent/internal/descriptor"
	"github.com/manent-deployer/manent/internal/service"
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
		if st.Tenant != "" {
			lines = append(lines, "tenant="+st.Tenant)
		}
		if st.ConfigRevision != "" {
			lines = append(lines, "config_revision="+st.ConfigRevision)
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
				fmt.Fprintf(os.Stderr, "warning: multi-line export %s, not written to %s\n", name, r.outputs)
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
		fmt.Fprintf(&b, "### manent · %s of %s to %s\n\n| | |\n|---|---|\n", kind, d.Application, d.Environment)
		fmt.Fprintf(&b, "| Version | `%s` |\n| Deploy | `%s` |\n| Outcome | **%s** |\n", d.Artifact.Version, sub.ID, st.State)
		if st.ConfigRevision != "" {
			fmt.Fprintf(&b, "| Configuration | `%s` |\n", st.ConfigRevision)
		}
		if st.RolloutProgress != "" {
			fmt.Fprintf(&b, "| Rollout | %s |\n", st.RolloutProgress)
		}
		if st.Rollback != "" {
			fmt.Fprintf(&b, "| Rollback | %s |\n", st.Rollback)
		}
		if st.Error != "" {
			fmt.Fprintf(&b, "| Error | %s |\n", strings.ReplaceAll(strings.ReplaceAll(st.Error, "|", "\\|"), "\n", " "))
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
	Tenant      string `short:"t" long:"tenant" description:"tenant of the application, required if several tenants have an application with this name"`
	Application string `short:"a" long:"application" required:"true" description:"application"`
	Environment string `short:"e" long:"environment" required:"true" description:"environment"`
	Artifact    string `short:"A" long:"artifact" description:"artifact the configuration is for (templates with {artifact})"`
	File        string `short:"f" long:"file" required:"true" description:"environment configuration in YAML or JSON (as stored in the configuration system)"`
	Descriptor  string `short:"d" long:"descriptor" description:"example descriptor; default the request of the deploy in service"`
}

// Execute implements flags.Commander.
func (c *EnvValidateCmd) Execute([]string) error {
	raw, err := os.ReadFile(c.File)
	if err != nil {
		return err
	}
	check := service.EnvironmentCheck{Tenant: c.Tenant, Application: c.Application, Artifact: c.Artifact, Environment: c.Environment, Config: string(raw)}
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
			fmt.Fprintln(os.Stderr, "warning:", w)
		}
		return cliutil.PrintJSON(v)
	})
}
