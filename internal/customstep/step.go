// Package customstep runs the custom steps defined in manent.yml: commands on the targets over
// SSH and HTTP requests. Every step is built from the spec captured in the plan
// (pipeline.PlannedStep.Spec), which is verified (signature and revocation) inside the DBOS step.
package customstep

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/joeg-ita/manent/internal/audit"
	"github.com/joeg-ita/manent/internal/customspec"
	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/pipeline"
	"github.com/joeg-ita/manent/internal/secrets"
)

// HTTPDoer abstracts the HTTP client for tests.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Builder builds the custom steps of the plan.
type Builder struct {
	Secrets secrets.Provider
	// Keys verify the spec signatures; without them only unsigned specs are accepted.
	Keys customspec.Keys
	// Revoked are the sha256 of the specs not to run any more.
	Revoked []string
	Dialer  Dialer
	HTTP    HTTPDoer
	// AllowHTTP allows http URLs.
	AllowHTTP bool
	// SecretsDir is the secrets directory on the targets (tmpfs).
	SecretsDir string
	Audit      audit.Recorder
	// Frozen means a freeze is in progress: in dry-run command steps are skipped.
	Frozen func(application, environment string, at time.Time) bool
	Logger *slog.Logger
	Now    func() time.Time
}

// NewHTTPClient creates the client of http steps: it does not follow redirects, which would
// carry requests and credentials to another host. The timeout is the step's.
func NewHTTPClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// Build returns the step of the captured spec. Signature and revocation are checked in Run,
// inside the DBOS step: so a recovery after a revocation resumes the same step sequence and
// fails only the steps not yet run.
func (b *Builder) Build(ps pipeline.PlannedStep, requestedBy string) pipeline.Step {
	s := &step{b: b, ps: ps, requestedBy: requestedBy}
	s.doc, s.decodeErr = customspec.Decode(ps.Spec)
	if s.decodeErr == nil && s.doc.DryRun != nil {
		return dryStep{s}
	}
	return s
}

// step is a custom step; it implements pipeline.Step.
type step struct {
	b           *Builder
	ps          pipeline.PlannedStep
	doc         customspec.Document
	decodeErr   error
	requestedBy string
}

// dryStep is a step with dry_run: it also implements pipeline.DryRunner.
type dryStep struct{ *step }

// Name returns the step name.
func (s *step) Name() string { return s.ps.Name }

// Run runs the run spec.
func (s *step) Run(ctx context.Context, in pipeline.Input) (pipeline.Output, error) {
	return s.exec(ctx, in, false)
}

// DryRun runs the dry_run spec.
func (s dryStep) DryRun(ctx context.Context, in pipeline.Input) (pipeline.Output, error) {
	return s.exec(ctx, in, true)
}

// versionRe limits the versions usable in placeholders: the descriptor does not validate them.
var versionRe = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+_-]{0,127}$`)

// Output keys written by manent (not by the spec).
const (
	OutDryRun = "dry_run"
	OutReason = "reason"
)

// run is the context of a run.
type run struct {
	s       *step
	in      pipeline.Input
	d       *descriptor.Descriptor
	action  *customspec.Action
	dry     bool
	data    templateData
	secrets map[string]string // logical name → value, only the action's ones
	red     *redactor
	log     *slog.Logger
	targets []string
}

func (s *step) exec(ctx context.Context, in pipeline.Input, dry bool) (pipeline.Output, error) {
	if err := s.verify(); err != nil {
		return nil, pipeline.Permanent(err)
	}
	d := in.Descriptor
	log := s.b.Logger.With("deployment", in.DeploymentID, "step", s.ps.Name, "spec", s.ps.SpecHash)
	if dry && s.doc.Type == customspec.TypeCommand && s.b.Frozen != nil && d.FreezeOverride == "" &&
		s.b.Frozen(d.Application, d.Environment, s.now()) {
		// Dry-runs are not subject to governance, but the commands run on production targets: they
		// do not run during a freeze.
		log.InfoContext(ctx, "dry-run: command skipped for the freeze in progress")
		return pipeline.Output{OutDryRun: "skipped", OutReason: "freeze"}, nil
	}
	r, err := s.prepare(ctx, in, dry, log)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.doc.Timeout)
	defer cancel()
	var raw map[string]string
	switch s.doc.Type {
	case customspec.TypeCommand:
		raw, err = r.command(ctx)
	case customspec.TypeHTTP:
		raw, err = r.http(ctx)
	default:
		err = pipeline.Permanent(fmt.Errorf("step type %q not supported", s.doc.Type))
	}
	var out pipeline.Output
	if err == nil {
		out, err = r.outputs(raw)
	}
	if err != nil {
		err = r.red.err(err)
	}
	r.audit(ctx, err)
	if err != nil {
		return nil, err
	}
	log.InfoContext(ctx, "custom step completed", "type", s.doc.Type, "dry_run", dry, "outputs", len(out))
	return out, nil
}

func (s *step) now() time.Time {
	if s.b.Now != nil {
		return s.b.Now()
	}
	return time.Now()
}

// verify checks that the spec is readable, belongs to the step, is signed and not revoked.
func (s *step) verify() error {
	if s.decodeErr != nil {
		return s.decodeErr
	}
	if s.doc.Name != s.ps.Name {
		return fmt.Errorf("the specification belongs to step %q, not to %q", s.doc.Name, s.ps.Name)
	}
	if err := s.b.Keys.Verify(s.ps.Spec, s.ps.SpecMAC); err != nil {
		return err
	}
	if h := customspec.Hash(s.ps.Spec); slices.Contains(s.b.Revoked, h) {
		return fmt.Errorf("specification %s of step %q is revoked (pipeline.custom.revoked_specs)", h, s.ps.Name)
	}
	return nil
}

// prepare validates parameters and version, resolves the secrets and builds the placeholder data.
func (s *step) prepare(ctx context.Context, in pipeline.Input, dry bool, log *slog.Logger) (*run, error) {
	d := in.Descriptor
	if d.Artifact.Version != "" && !versionRe.MatchString(d.Artifact.Version) {
		return nil, pipeline.Permanent(fmt.Errorf("version %q cannot be used by custom steps: %s", d.Artifact.Version, versionRe))
	}
	params, err := s.doc.EffectiveParams(d.StepParams[s.ps.Name], true)
	if err != nil {
		return nil, pipeline.Permanent(fmt.Errorf("step parameters: %w", err))
	}
	action, names := s.doc.Run, s.doc.Secrets
	if dry {
		action, names = s.doc.DryRun, s.doc.DryRun.Secrets
	}
	r := &run{s: s, in: in, d: d, action: action, dry: dry, secrets: map[string]string{}, log: log}
	all := slices.Clone(names)
	if s.doc.SSH != nil {
		all = append(all, nonEmpty(s.doc.SSH.KeySecret, s.doc.SSH.PasswordSecret)...)
	}
	var values []string
	res := secrets.DescriptorResolver{Provider: s.b.Secrets, Descriptor: d}
	for _, n := range all {
		if _, ok := d.SecretRefByName(n); !ok {
			return nil, pipeline.Permanent(fmt.Errorf("step secret %q not declared in secrets (descriptor or environment configuration)", n))
		}
		v, err := res.Secret(ctx, n)
		if err != nil {
			return nil, fmt.Errorf("secret %q: %w", n, err)
		}
		values = append(values, v)
		if slices.Contains(names, n) {
			r.secrets[n] = v
		}
	}
	r.red = newRedactor(values)
	sha := d.Artifact.SHA256
	if o := in.Outputs["artifacts"]; o["sha256"] != "" {
		sha = o["sha256"]
	}
	r.data = templateData{
		Application: d.Application, Environment: d.Environment, Version: d.Artifact.Version, Artifact: d.Artifact.Name,
		DeploymentID: in.DeploymentID, SHA256: sha, DryRun: dry, Params: params, Outputs: in.Outputs,
	}
	return r, nil
}

// outputs accepts only declared keys, with values matching the pattern and without secrets.
func (r *run) outputs(raw map[string]string) (pipeline.Output, error) {
	out := pipeline.Output{}
	if r.dry && len(raw) == 0 {
		out[OutDryRun] = "executed"
	}
	for _, k := range slices.Sorted(maps.Keys(raw)) {
		spec, ok := r.s.doc.Outputs[k]
		if !ok {
			return nil, pipeline.Permanent(fmt.Errorf("output %q not declared by the step", k))
		}
		re, err := customspec.Compile(spec.Pattern)
		if err != nil {
			return nil, pipeline.Permanent(err)
		}
		v := raw[k]
		if r.red.contains(v) {
			return nil, pipeline.Permanent(fmt.Errorf("output %q contains a secret: rejected", k))
		}
		if !re.MatchString(v) {
			return nil, pipeline.Permanent(fmt.Errorf("output %q: value does not match the declared pattern", k))
		}
		out[k] = v
	}
	return out, nil
}

// audit records the run: one entry per attempt, with the spec and the targets.
func (r *run) audit(ctx context.Context, err error) {
	if r.s.b.Audit == nil {
		return
	}
	e := audit.Entry{
		Time: r.s.now().UTC(), Actor: r.s.requestedBy, Action: ActionStepRun, Outcome: audit.OutcomeSucceeded,
		Application: r.d.Application, Environment: r.d.Environment, Deployment: r.in.DeploymentID, DryRun: r.dry,
		Step: r.s.ps.Name, Spec: customspec.Hash(r.s.ps.Spec), Targets: r.targets,
	}
	if err != nil {
		e.Outcome, e.Reason = audit.OutcomeFailed, err.Error()
	}
	if aerr := r.s.b.Audit.Record(ctx, e); aerr != nil {
		r.log.ErrorContext(ctx, "audit recording failed", "error", aerr)
	}
}

// ActionStepRun is the audit action of a custom step run.
const ActionStepRun = "step.custom.run"

func nonEmpty(v ...string) []string {
	return slices.DeleteFunc(v, func(s string) bool { return s == "" })
}

// temporary marks an error as retryable (the step default); used for clarity.
func temporary(format string, a ...any) error { return fmt.Errorf(format, a...) }

// permanent creates a permanent error.
func permanent(format string, a ...any) error { return pipeline.Permanent(fmt.Errorf(format, a...)) }

// isPermanent reports a permanent error.
func isPermanent(err error) bool { return errors.Is(err, pipeline.ErrPermanent) }

// safeHost describes a target in logs and audit.
func safeHost(t descriptor.Target) string { return strings.TrimSpace(t.Host) }
