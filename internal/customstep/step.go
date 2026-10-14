// Package customstep esegue gli step personalizzati definiti in manent.yml: comandi sui target
// via SSH e richieste HTTP. Ogni step è costruito dalla specifica fotografata nel piano
// (pipeline.PlannedStep.Spec), che viene verificata (firma e revoca) dentro lo step DBOS.
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

// HTTPDoer astrae il client HTTP per i test.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Builder costruisce gli step personalizzati del piano.
type Builder struct {
	Secrets secrets.Provider
	// Keys verificano la firma delle specifiche; senza, sono accettate solo quelle non firmate.
	Keys customspec.Keys
	// Revoked sono gli sha256 delle specifiche da non eseguire più.
	Revoked []string
	Dialer  Dialer
	HTTP    HTTPDoer
	// AllowHTTP ammette URL http.
	AllowHTTP bool
	// SecretsDir è la directory dei segreti sui target (tmpfs).
	SecretsDir string
	Audit      audit.Recorder
	// Frozen indica un freeze in corso: in dry-run gli step command vengono saltati.
	Frozen func(application, environment string, at time.Time) bool
	Logger *slog.Logger
	Now    func() time.Time
}

// NewHTTPClient crea il client degli step http: non segue i redirect, che porterebbero
// richieste e credenziali verso un altro host. Il timeout è quello dello step.
func NewHTTPClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// Build restituisce lo step della specifica fotografata. La verifica della firma e della
// revoca avviene in Run, dentro lo step DBOS: così una recovery dopo una revoca riprende la
// stessa sequenza di step e fallisce solo quelli non ancora eseguiti.
func (b *Builder) Build(ps pipeline.PlannedStep, requestedBy string) pipeline.Step {
	s := &step{b: b, ps: ps, requestedBy: requestedBy}
	s.doc, s.decodeErr = customspec.Decode(ps.Spec)
	if s.decodeErr == nil && s.doc.DryRun != nil {
		return dryStep{s}
	}
	return s
}

// step è uno step personalizzato; implementa pipeline.Step.
type step struct {
	b           *Builder
	ps          pipeline.PlannedStep
	doc         customspec.Document
	decodeErr   error
	requestedBy string
}

// dryStep è uno step con dry_run: implementa anche pipeline.DryRunner.
type dryStep struct{ *step }

// Name restituisce il nome dello step.
func (s *step) Name() string { return s.ps.Name }

// Run esegue la specifica run.
func (s *step) Run(ctx context.Context, in pipeline.Input) (pipeline.Output, error) {
	return s.exec(ctx, in, false)
}

// DryRun esegue la specifica dry_run.
func (s dryStep) DryRun(ctx context.Context, in pipeline.Input) (pipeline.Output, error) {
	return s.exec(ctx, in, true)
}

// versionRe limita le versioni usabili nei segnaposto: il descrittore non le valida.
var versionRe = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+_-]{0,127}$`)

// Chiavi di output scritte da manent (non dalla specifica).
const (
	OutDryRun = "dry_run"
	OutReason = "reason"
)

// run è il contesto di un'esecuzione.
type run struct {
	s       *step
	in      pipeline.Input
	d       *descriptor.Descriptor
	action  *customspec.Action
	dry     bool
	data    templateData
	secrets map[string]string // nome logico → valore, solo quelli dell'azione
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
		// Il dry-run non è soggetto alla governance, ma i comandi girano sui target di
		// produzione: durante un freeze non vengono eseguiti.
		log.InfoContext(ctx, "dry-run: comando saltato per il freeze in corso")
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
		err = pipeline.Permanent(fmt.Errorf("tipo di step %q non supportato", s.doc.Type))
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
	log.InfoContext(ctx, "step personalizzato completato", "type", s.doc.Type, "dry_run", dry, "outputs", len(out))
	return out, nil
}

func (s *step) now() time.Time {
	if s.b.Now != nil {
		return s.b.Now()
	}
	return time.Now()
}

// verify controlla che la specifica sia leggibile, appartenga allo step, sia firmata e non revocata.
func (s *step) verify() error {
	if s.decodeErr != nil {
		return s.decodeErr
	}
	if s.doc.Name != s.ps.Name {
		return fmt.Errorf("la specifica appartiene allo step %q, non a %q", s.doc.Name, s.ps.Name)
	}
	if err := s.b.Keys.Verify(s.ps.Spec, s.ps.SpecMAC); err != nil {
		return err
	}
	if h := customspec.Hash(s.ps.Spec); slices.Contains(s.b.Revoked, h) {
		return fmt.Errorf("la specifica %s dello step %q è revocata (pipeline.custom.revoked_specs)", h, s.ps.Name)
	}
	return nil
}

// prepare valida parametri e versione, risolve i segreti e costruisce i dati dei segnaposto.
func (s *step) prepare(ctx context.Context, in pipeline.Input, dry bool, log *slog.Logger) (*run, error) {
	d := in.Descriptor
	if d.Artifact.Version != "" && !versionRe.MatchString(d.Artifact.Version) {
		return nil, pipeline.Permanent(fmt.Errorf("versione %q non utilizzabile dagli step personalizzati: %s", d.Artifact.Version, versionRe))
	}
	params, err := s.doc.EffectiveParams(d.StepParams[s.ps.Name], true)
	if err != nil {
		return nil, pipeline.Permanent(fmt.Errorf("parametri dello step: %w", err))
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
			return nil, pipeline.Permanent(fmt.Errorf("segreto %q dello step non dichiarato in secrets (descrittore o configurazione di ambiente)", n))
		}
		v, err := res.Secret(ctx, n)
		if err != nil {
			return nil, fmt.Errorf("segreto %q: %w", n, err)
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

// outputs accetta solo le chiavi dichiarate, con valori conformi al pattern e senza segreti.
func (r *run) outputs(raw map[string]string) (pipeline.Output, error) {
	out := pipeline.Output{}
	if r.dry && len(raw) == 0 {
		out[OutDryRun] = "executed"
	}
	for _, k := range slices.Sorted(maps.Keys(raw)) {
		spec, ok := r.s.doc.Outputs[k]
		if !ok {
			return nil, pipeline.Permanent(fmt.Errorf("output %q non dichiarato dallo step", k))
		}
		re, err := customspec.Compile(spec.Pattern)
		if err != nil {
			return nil, pipeline.Permanent(err)
		}
		v := raw[k]
		if r.red.contains(v) {
			return nil, pipeline.Permanent(fmt.Errorf("output %q contiene un segreto: rifiutato", k))
		}
		if !re.MatchString(v) {
			return nil, pipeline.Permanent(fmt.Errorf("output %q: valore non conforme al pattern dichiarato", k))
		}
		out[k] = v
	}
	return out, nil
}

// audit registra l'esecuzione: una voce per tentativo, con la specifica e i target.
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
		r.log.ErrorContext(ctx, "registrazione nell'audit fallita", "error", aerr)
	}
}

// ActionStepRun è l'azione di audit dell'esecuzione di uno step personalizzato.
const ActionStepRun = "step.custom.run"

func nonEmpty(v ...string) []string {
	return slices.DeleteFunc(v, func(s string) bool { return s == "" })
}

// temporary marca un errore come ritentabile (il default degli step); usato per chiarezza.
func temporary(format string, a ...any) error { return fmt.Errorf(format, a...) }

// permanent crea un errore permanente.
func permanent(format string, a ...any) error { return pipeline.Permanent(fmt.Errorf(format, a...)) }

// isPermanent indica un errore permanente.
func isPermanent(err error) bool { return errors.Is(err, pipeline.ErrPermanent) }

// safeHost descrive un target nei log e nell'audit.
func safeHost(t descriptor.Target) string { return strings.TrimSpace(t.Host) }
