package governance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Phases of a governance evaluation (Input.Phase).
const (
	// PhaseSubmit is the submission of a deploy, promotion or rollback: obligations are fixed here.
	PhaseSubmit = "submit"
	// PhaseValidate describes the decision without enqueuing (validate, plan).
	PhaseValidate = "validate"
	// PhaseStart is the start of the deploy on the worker (step config): denials and freezes only.
	PhaseStart = "start"
	// PhaseStage is the start of a rollout stage, once per region.
	PhaseStage = "stage"
	// PhaseTick is a run of a recurring schedule.
	PhaseTick = "tick"
)

// InputSchema is the version of the Input document. Fields are only added, never renamed.
const InputSchema = 1

// ErrUnavailable means no valid policy is loaded (or it is older than max_staleness): real
// deploys are refused until it comes back.
var ErrUnavailable = errors.New("governance policy unavailable")

// Input describes what is being done, by whom, when and in which phase. It never contains
// secrets nor plugin parameters.
type Input struct {
	Schema int    `json:"schema"`
	Phase  string `json:"phase"`
	// Now is the instant of the evaluation: the submission time (or schedule.at), or the time
	// read by the DBOS step that evaluates. Policies must use it instead of the clock.
	Now         time.Time       `json:"now"`
	Application string          `json:"application"`
	Environment string          `json:"environment"`
	Region      string          `json:"region,omitempty"`
	Deployment  InputDeployment `json:"deployment"`
	Artifact    InputArtifact   `json:"artifact"`
	RequestedBy InputIdentity   `json:"requested_by"`
	Plugin      string          `json:"plugin,omitempty"`
	Rollout     *InputRollout   `json:"rollout,omitempty"`
	Targets     []InputTarget   `json:"targets,omitempty"`
	Current     *InputRelease   `json:"current,omitempty"`
	Freezes     InputFreezes    `json:"freezes"`
}

// InputDeployment describes the deploy request.
type InputDeployment struct {
	ID string `json:"id,omitempty"`
	// Kind is deploy, promotion or rollback.
	Kind           string        `json:"kind"`
	Source         string        `json:"source,omitempty"`
	DryRun         bool          `json:"dry_run"`
	Schedule       InputSchedule `json:"schedule"`
	FreezeOverride string        `json:"freeze_override,omitempty"`
	After          []string      `json:"after,omitempty"`
	Release        string        `json:"release,omitempty"`
}

// InputSchedule is the schedule of the deploy, if any.
type InputSchedule struct {
	At   *time.Time `json:"at,omitempty"`
	Cron string     `json:"cron,omitempty"`
}

// InputArtifact is the artifact to release.
type InputArtifact struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Type    string `json:"type,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
	Source  string `json:"source,omitempty"`
}

// InputIdentity is who requested the deploy.
type InputIdentity struct {
	// ID is "<method>:<name>", e.g. "oidc:alice".
	ID     string   `json:"id"`
	Method string   `json:"method,omitempty"`
	Name   string   `json:"name,omitempty"`
	Groups []string `json:"groups,omitempty"`
}

// InputRollout summarizes the rollout strategy of the effective deploy.
type InputRollout struct {
	Strategy       string   `json:"strategy"`
	Waves          int      `json:"waves,omitempty"`
	Regions        []string `json:"regions,omitempty"`
	ApprovalPoints int      `json:"approval_points,omitempty"`
}

// InputTarget is a target of the effective deploy (host and labels only).
type InputTarget struct {
	Host   string            `json:"host"`
	Labels map[string]string `json:"labels,omitempty"`
}

// InputRelease is the release in service in the environment.
type InputRelease struct {
	ID         string    `json:"id"`
	Version    string    `json:"version"`
	SHA256     string    `json:"sha256,omitempty"`
	DeployedAt time.Time `json:"deployed_at"`
}

// InputFreezes lists the freeze windows active at Now, computed by manent from the data.
type InputFreezes struct {
	Active []WindowStatus `json:"active"`
}

// Denial is a reason the policy refuses the deploy.
type Denial struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Decision is the outcome of an evaluation: denials and obligations. manent enforces the
// obligations: the policy says what is needed, the engine checks it.
type Decision struct {
	// Revision identifies the policy that produced the decision.
	Revision string `json:"revision,omitempty"`
	// Rule is a descriptive name, like Rule.Name.
	Rule     string   `json:"rule,omitempty"`
	Deny     []Denial `json:"deny,omitempty"`
	Requires []string `json:"requires,omitempty"`
	// Approvals is nil when no approval is required.
	Approvals *Approvals `json:"approvals,omitempty"`
	Change    *Change    `json:"change,omitempty"`
	// FreezeExempt names the active windows the policy exempts (never the hard ones).
	FreezeExempt []string `json:"freeze_exempt,omitempty"`
	Notes        []string `json:"notes,omitempty"`
	// Freeze is the active window that blocks the deploy, after exemptions and override.
	Freeze *WindowStatus `json:"freeze,omitempty"`
	// InputSHA256 is the hash of the evaluated input, for the audit.
	InputSHA256 string `json:"input_sha256,omitempty"`
}

// Snapshot is the decision recorded in the deploy request at submission.
type Snapshot struct {
	Revision    string   `json:"revision"`
	Rule        string   `json:"rule,omitempty"`
	InputSHA256 string   `json:"input_sha256,omitempty"`
	Notes       []string `json:"notes,omitempty"`
}

// Snapshot returns the part of the decision to record in the request.
func (d Decision) Snapshot() *Snapshot {
	return &Snapshot{Revision: d.Revision, Rule: d.Rule, InputSHA256: d.InputSHA256, Notes: d.Notes}
}

// Denied returns the error describing the denials, nil if there are none.
func (d Decision) Denied() error {
	if len(d.Deny) == 0 {
		return nil
	}
	msgs := make([]string, len(d.Deny))
	for i, x := range d.Deny {
		msgs[i] = x.Message
		if x.Code != "" {
			msgs[i] = x.Code + ": " + x.Message
		}
	}
	return fmt.Errorf("denied by the governance policy %s: %s", d.Revision, strings.Join(msgs, "; "))
}

// Check verifies the obligations of the decision.
func (d *Decision) Check() error {
	var errs []error
	for i, x := range d.Deny {
		if x.Message == "" {
			errs = append(errs, fmt.Errorf("deny[%d]: message is required", i))
		}
	}
	if a := d.Approvals; a != nil {
		if a.Required < 0 {
			errs = append(errs, errors.New("approvals.required must be >= 0"))
		}
		if w := a.When; w != "" && w != ApprovalsBefore && w != ApprovalsRollout {
			errs = append(errs, fmt.Errorf("invalid approvals.when %q (before or rollout)", w))
		}
		if a.Timeout < 0 {
			errs = append(errs, errors.New("approvals.timeout must be >= 0"))
		}
		if a.Required == 0 {
			d.Approvals = nil
		} else if a.Timeout == 0 {
			a.Timeout = DefaultApprovalTimeout
		}
	}
	if c := d.Change; c != nil && c.Timeout < 0 {
		errs = append(errs, errors.New("change.timeout must be >= 0"))
	}
	for _, env := range d.Requires {
		if env == "" {
			errs = append(errs, errors.New("requires: empty environment"))
		}
	}
	return errors.Join(errs...)
}

// EvalFunc evaluates the policy on a prepared input.
type EvalFunc func(context.Context, Input) (Decision, error)

// Decide prepares the input (schema, active freezes), evaluates it and applies the freeze
// semantics: active windows block unless exempted by the policy or overridden with
// freezeOverride, and hard windows always block.
func Decide(ctx context.Context, in Input, windows []Window, eval EvalFunc) (Decision, error) {
	in.Schema = InputSchema
	in.Freezes.Active = ActiveWindows(windows, in.Application, in.Environment, in.Region, in.Now)
	if in.Freezes.Active == nil {
		in.Freezes.Active = []WindowStatus{}
	}
	dec, err := eval(ctx, in)
	if err != nil {
		return Decision{}, err
	}
	if err := dec.Check(); err != nil {
		return Decision{}, fmt.Errorf("invalid policy decision: %w", err)
	}
	dec.Freeze = blocking(in.Freezes.Active, dec.FreezeExempt, in.Deployment.FreezeOverride != "")
	dec.InputSHA256 = InputHash(in)
	return dec, nil
}

// InputHash is the sha256 of the input in JSON.
func InputHash(in Input) string {
	b, _ := json.Marshal(in)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// blocking returns the first active window that blocks: hard ones always, the others unless
// exempted or overridden.
func blocking(active []WindowStatus, exempt []string, override bool) *WindowStatus {
	for _, w := range active {
		if w.Hard || (!override && !slices.Contains(exempt, w.Name)) {
			return &w
		}
	}
	return nil
}

// Evaluate implements the built-in engine: the first matching rule gives the obligations and
// the configured windows the freezes.
func (p *Policy) Evaluate(ctx context.Context, in Input) (Decision, error) {
	var windows []Window
	if p != nil {
		windows = p.Freezes
	}
	return Decide(ctx, in, windows, func(_ context.Context, in Input) (Decision, error) {
		return p.RuleDecision(in.Application, in.Environment), nil
	})
}

// RuleDecision is the decision of the first rule matching application and environment.
func (p *Policy) RuleDecision(application, environment string) Decision {
	r := p.Rule(application, environment)
	dec := Decision{Revision: p.Revision(), Rule: r.Name, Requires: r.Requires}
	if r.Approvals.Required > 0 {
		a := r.Approvals
		dec.Approvals = &a
	}
	if r.Change != nil {
		c := *r.Change
		dec.Change = &c
	}
	return dec
}

// Revision identifies the built-in rules: "builtin:" and the start of their hash.
func (p *Policy) Revision() string {
	var rules []Rule
	if p != nil {
		rules = p.Rules
	}
	b, _ := json.Marshal(rules)
	sum := sha256.Sum256(b)
	return "builtin:" + hex.EncodeToString(sum[:6])
}
