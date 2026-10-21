package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/joeg-ita/manent/internal/auth"
	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/governance"
	"github.com/joeg-ita/manent/internal/pipeline"
	"github.com/joeg-ita/manent/internal/rbac"
	"github.com/joeg-ita/manent/internal/rollout"
	"github.com/joeg-ita/manent/internal/service"
)

// Policy service types (contract in internal/service).
type (
	PolicyService           = service.PolicyService
	FreezeRequest           = service.FreezeRequest
	PolicyRevision          = service.PolicyRevision
	PolicyTest              = service.PolicyTest
	PolicyList              = service.PolicyList
	PolicyPush              = service.PolicyPush
	PolicyEvaluationRequest = service.PolicyEvaluationRequest
	PolicyEvaluation        = service.PolicyEvaluation
	PolicyImpact            = service.PolicyImpact
	PolicyChange            = service.PolicyChange
	PolicyAccessRequest     = service.PolicyAccessRequest
	PolicyAccess            = service.PolicyAccess
	AccessCheck             = service.AccessCheck
	AccessOutcome           = service.AccessOutcome
)

// Governance evaluates the governance policy and lists the freeze windows. The built-in
// *governance.Policy implements it, and so does policy.Runtime (OPA and runtime freezes).
type Governance interface {
	// Evaluate returns the decision for the input; an error is not a denial (Decision.Deny).
	Evaluate(ctx context.Context, in governance.Input) (governance.Decision, error)
	// Windows lists the active and upcoming freeze windows.
	Windows(application, environment string, t time.Time) []governance.WindowStatus
}

// PolicyAdmin manages policy revisions and runtime freezes (policy.Runtime).
type PolicyAdmin interface {
	CreateFreeze(ctx context.Context, w governance.Window) error
	DeleteFreeze(ctx context.Context, name string) error
	List(ctx context.Context) (*PolicyList, error)
	Get(ctx context.Context, revision int64) (*PolicyRevision, error)
	Push(ctx context.Context, files map[string]string, comment, author string) (*PolicyRevision, error)
	// Activate activates a revision: a draft (draft true, four-eyes rule) or one already active
	// in the past.
	Activate(ctx context.Context, revision int64, by string, draft bool) error
	// EvaluateRevision evaluates a revision (0 = the one in force).
	EvaluateRevision(ctx context.Context, revision int64, in governance.Input) (governance.Decision, error)
	// AccessRevision returns the grants of an identity with a revision (0 = the one in force),
	// the revision label and the grants discarded; DenyRevision the denials for a change.
	AccessRevision(ctx context.Context, revision int64, p *auth.Principal) (string, []rbac.Grant, []string, error)
	DenyRevision(ctx context.Context, revision int64, r rbac.DenyRequest) ([]string, error)
}

// Values of governance.opa.start (WorkerOptions.PolicyStart).
const (
	// PolicyStartDeny applies at the deploy start only the denials and the freezes.
	PolicyStartDeny = "deny"
	// PolicyStartStrict also refuses a deploy whose obligations grew after the submission.
	PolicyStartStrict = "strict"
)

// freezeRecheck is how often a rollout paused by a freeze evaluates the policy again: a
// window can be closed early or extended at runtime, so the pause does not sleep until the
// end it read.
var freezeRecheck = 15 * time.Second

// policyStepRetries are the attempts of the policy steps of rollout stages and schedule ticks:
// a policy temporarily unavailable must not fail a long rollout at once.
const policyStepRetries = 10

// policyInput builds the governance input of a request. eff is the effective descriptor
// (environment configuration merged); with nil, targets, plugin and rollout are unknown.
func policyInput(phase string, req Request, eff *descriptor.Descriptor, now time.Time) governance.Input {
	d := req.Descriptor
	src := &d
	if eff != nil {
		src = eff
	}
	method, name, _ := strings.Cut(req.RequestedBy, ":")
	in := governance.Input{
		Phase: phase, Now: now.UTC(), Application: d.Application, Environment: d.Environment,
		Deployment: governance.InputDeployment{
			ID: d.ID, Kind: cmp.Or(req.Kind, KindDeploy), Source: req.Source, DryRun: d.DryRun,
			FreezeOverride: d.FreezeOverride, After: d.After, Release: req.Release,
		},
		Artifact: governance.InputArtifact{
			Name: src.Artifact.Name, Version: src.Artifact.Version, Type: src.Artifact.Type,
			SHA256: src.Artifact.SHA256, Source: src.Artifact.Source,
		},
		RequestedBy: governance.InputIdentity{ID: req.RequestedBy, Method: method, Name: name, Groups: req.RequestedGroups},
		Plugin:      src.Deploy.Plugin,
	}
	if req.Tenant != nil {
		in.Tenant = &governance.InputTenant{Name: req.Tenant.Name, Tags: req.Tenant.Tags}
	} else if d.Tenant != "" {
		in.Tenant = &governance.InputTenant{Name: d.Tenant}
	}
	if req.Run != "" {
		in.TenantsRun = &governance.InputTenantsRun{ID: req.Run}
	}
	if s := d.Schedule; s != nil {
		in.Deployment.Schedule = governance.InputSchedule{At: s.At, Cron: s.Cron}
	}
	if eff == nil {
		return in
	}
	var regions []string
	for _, t := range eff.Deploy.Targets {
		in.Targets = append(in.Targets, governance.InputTarget{Host: t.Host, Labels: t.Labels})
		if r := t.Labels["region"]; r != "" && !slices.Contains(regions, r) {
			regions = append(regions, r)
		}
	}
	slices.Sort(regions)
	if r := eff.Rollout; r != nil {
		in.Rollout = &governance.InputRollout{Strategy: "waves", Waves: len(r.Waves), Regions: regions, ApprovalPoints: len(r.ApprovalsBetween)}
		if r.IsTraffic() {
			in.Rollout.Strategy = "traffic"
		}
	}
	return in
}

// DescriptorPolicyInput builds the governance input of a descriptor not yet submitted (local
// validation and evaluation): kind deploy unless given.
func DescriptorPolicyInput(phase string, d, eff *descriptor.Descriptor, kind, requestedBy string, groups []string, now time.Time) governance.Input {
	in := policyInput(phase, Request{Descriptor: *d, Kind: kind, RequestedBy: requestedBy, RequestedGroups: groups}, eff, now)
	in.Schema = governance.InputSchema
	return in
}

// policyError maps an evaluation failure: a policy not loaded is temporary, any other
// failure refuses the request (fail-closed).
func policyError(err error) error {
	if errors.Is(err, governance.ErrUnavailable) {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return fmt.Errorf("%w: governance policy evaluation failed: %w", ErrInvalidRequest, err)
}

// inventoryReader reads the release in service, for the input's current field.
type inventoryReader interface {
	Inventory(ctx context.Context, f InventoryFilter) ([]InventoryEntry, error)
}

// currentRelease returns the release in service in the environment, nil if none or unreadable.
// artifact restricts it to an artifact (artifact identity; empty: the application's entry).
func currentRelease(ctx context.Context, r inventoryReader, application, artifact, environment, tenantName string) *governance.InputRelease {
	if r == nil {
		return nil
	}
	inv, err := r.Inventory(ctx, InventoryFilter{Application: application, Environment: environment, Tenant: tenantName, Artifact: artifact})
	if err != nil {
		return nil
	}
	i := slices.IndexFunc(inv, func(e InventoryEntry) bool { return e.Tenant == tenantName && e.Current != nil })
	if i < 0 {
		return nil
	}
	c := inv[i].Current
	return &governance.InputRelease{ID: c.ID, Version: c.Version, SHA256: c.SHA256, DeployedAt: c.UpdatedAt}
}

// govern applies the governance policy to a real deploy at submission: denials, freeze,
// mandatory promotion, then it records in the request the obligations (approvals, change)
// and the decision. Dry-runs are not subject to it, since they do not change the environments.
func (e *Engine) govern(ctx context.Context, req *Request, eff *descriptor.Descriptor) error {
	d := &req.Descriptor
	if e.governance == nil || d.DryRun {
		return nil
	}
	at := e.now()
	if d.Schedule != nil && d.Schedule.At != nil {
		at = *d.Schedule.At
	}
	in := policyInput(governance.PhaseSubmit, *req, eff, at)
	in.Current = currentRelease(ctx, e, d.Application, e.releaseArtifact(d.Artifact.Name), d.Environment, d.Tenant)
	dec, err := e.governance.Evaluate(ctx, in)
	if err != nil {
		return policyError(err)
	}
	if err := dec.Denied(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	// A recurring schedule is checked at every run: its windows apply at the start.
	if dec.Freeze != nil && (d.Schedule == nil || d.Schedule.Cron == "") {
		return fmt.Errorf("%w: %s", ErrInvalidRequest, freezeMessage(dec.Freeze))
	}
	for _, env := range dec.Requires {
		rel, err := e.findRelease(ctx, d.Application, "", env, func(s Status) bool { return sameArtifact(s, d) && s.Tenant == d.Tenant })
		if err != nil {
			return err
		}
		if rel == nil {
			return fmt.Errorf("%w: %s %s was not released successfully in %s%s: it must be promoted from there (governance)", ErrInvalidRequest, d.Artifact.Name, d.Artifact.Version, env, tenantSuffix(d.Tenant))
		}
	}
	req.Approval, req.Change, req.Policy = dec.Approvals, dec.Change, dec.Snapshot()
	e.logger.Info("decisione di governance", "application", d.Application, "environment", d.Environment, "phase", in.Phase,
		"revision", dec.Revision, "rule", dec.Rule, "approvals", dec.Approvals.RequiredOrZero(), "change", dec.Change != nil,
		"input_sha256", dec.InputSHA256, "requested_by", req.RequestedBy)
	return nil
}

// describeGovernance adds to the validation the decision of the governance policy.
func (e *Engine) describeGovernance(ctx context.Context, v *Validation, d *descriptor.Descriptor, eff *descriptor.Descriptor) {
	if e.governance == nil {
		return
	}
	p := auth.FromContext(ctx)
	req := Request{Descriptor: *d, Kind: KindDeploy, RequestedBy: p.String()}
	if p != nil {
		req.RequestedGroups = p.Groups
	}
	at := e.now()
	if d.Schedule != nil && d.Schedule.At != nil {
		at = *d.Schedule.At
	}
	in := policyInput(governance.PhaseValidate, req, eff, at)
	in.Current = currentRelease(ctx, e, d.Application, e.releaseArtifact(d.Artifact.Name), d.Environment, d.Tenant)
	v.Governance = &GovernanceValidation{}
	dec, err := e.governance.Evaluate(ctx, in)
	if err != nil {
		v.Governance.Error = err.Error()
		return
	}
	g := v.Governance
	g.Revision, g.Rule, g.Requires, g.Change, g.Freeze, g.Deny, g.Notes = dec.Revision, dec.Rule, dec.Requires, dec.Change, dec.Freeze, dec.Deny, dec.Notes
	if a := dec.Approvals; a != nil {
		g.Approvals, g.Approvers = a.Required, a.Approvers
	}
}

// freezeMessage describes the freeze blocking a deploy.
func freezeMessage(w *governance.WindowStatus) string {
	kind := "freeze"
	if w.Hard {
		kind = "hard freeze"
	}
	msg := fmt.Sprintf("deploy blocked by %s %q until %s", kind, w.Name, w.End.Format(time.RFC3339))
	if w.Reason != "" {
		msg += " (" + w.Reason + ")"
	}
	if w.Hard {
		return msg + "; it cannot be overridden"
	}
	return msg + "; in an emergency set freezeOverride with the reason"
}

// startPolicy evaluates the policy at the start of the deploy (step config) on the effective
// descriptor: a denial or a freeze arrived after the submission stop it. An evaluation error is
// temporary: the step is retried.
func (w *Worker) startPolicy(ctx context.Context, req Request, eff *descriptor.Descriptor, meta map[string]string) error {
	d := req.Descriptor
	if w.governance == nil || d.DryRun {
		return nil
	}
	if d.FreezeOverride != "" {
		meta["freeze_override"] = d.FreezeOverride
	}
	in := policyInput(governance.PhaseStart, req, eff, time.Now())
	in.Current = currentRelease(ctx, w.service, d.Application, w.reader.releaseArtifact(d.Artifact.Name), d.Environment, d.Tenant)
	dec, err := w.governance.Evaluate(ctx, in)
	if err != nil {
		return err
	}
	meta["policy_revision"] = dec.Revision
	w.logger.InfoContext(ctx, "decisione di governance all'avvio", "application", d.Application, "environment", d.Environment,
		"revision", dec.Revision, "rule", dec.Rule, "denied", len(dec.Deny) > 0, "freeze", dec.Freeze != nil, "input_sha256", dec.InputSHA256)
	if err := dec.Denied(); err != nil {
		return pipeline.Permanent(err)
	}
	if dec.Freeze != nil {
		return pipeline.Permanent(errors.New(freezeMessage(dec.Freeze)))
	}
	if w.policyStart == PolicyStartStrict && req.Policy != nil {
		if err := grownObligations(req, dec); err != nil {
			return pipeline.Permanent(err)
		}
	}
	return nil
}

// grownObligations reports the obligations of the decision missing from those recorded at
// submission (governance.opa.start: strict).
func grownObligations(req Request, dec governance.Decision) error {
	var missing []string
	if n := dec.Approvals.RequiredOrZero(); n > req.Approval.RequiredOrZero() {
		missing = append(missing, strconv.Itoa(n)+" approvals")
	}
	if dec.Approvals != nil && dec.Approvals.Approvers != nil && (req.Approval == nil || req.Approval.Approvers == nil) {
		missing = append(missing, "approvals by specific approvers")
	}
	if c := dec.Change; c != nil && (req.Change == nil || (c.Approval && !req.Change.Approval)) {
		missing = append(missing, "change management")
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("the governance policy %s now requires %s, not asked at submission (revision %s): submit the deploy again",
		dec.Revision, strings.Join(missing, ", "), req.Policy.Revision)
}

// stagePolicy is the output of the policy step of a rollout stage.
type stagePolicy struct {
	Time     time.Time                `json:"time"`
	Revision string                   `json:"revision,omitempty"`
	Freeze   *governance.WindowStatus `json:"freeze,omitempty"`
	Region   string                   `json:"region,omitempty"`
	Deny     string                   `json:"deny,omitempty"`
}

// evaluateStage evaluates the policy for every region of the stage, at the current time.
func (w *Worker) evaluateStage(ctx context.Context, req Request, d *descriptor.Descriptor, stage rollout.Stage) (stagePolicy, error) {
	out := stagePolicy{Time: time.Now().UTC()}
	for _, reg := range stage.Regions() {
		in := policyInput(governance.PhaseStage, req, d, out.Time)
		in.Region = reg
		dec, err := w.governance.Evaluate(ctx, in)
		if err != nil {
			return stagePolicy{}, err
		}
		out.Revision = dec.Revision
		if err := dec.Denied(); err != nil {
			out.Deny = err.Error()
			return out, nil
		}
		if dec.Freeze != nil {
			out.Freeze, out.Region = dec.Freeze, reg
			return out, nil
		}
	}
	return out, nil
}

// waitFreeze waits for the end of the freezes active on the stage's regions (or on the whole
// environment). For requests with a recorded decision every check is a DBOS step
// (policy@<stage>#<n>): a policy changed meanwhile does not alter the step sequence in a
// recovery. A denial stops the rollout.
func (w *Worker) waitFreeze(ctx dbos.Context, req Request, d *descriptor.Descriptor, stage rollout.Stage, rs *RolloutState) error {
	if w.governance == nil {
		return nil
	}
	if req.Policy == nil {
		return w.waitFreezeLegacy(ctx, req, d, stage, rs)
	}
	for n := 0; ; n++ {
		sp, err := dbos.RunAsStep(ctx, func(c context.Context) (stagePolicy, error) {
			return w.evaluateStage(c, req, d, stage)
		}, dbos.WithStepName(fmt.Sprintf("policy@%s#%d", stage.Name, n)), dbos.WithStepMaxRetries(policyStepRetries))
		if err != nil {
			return err
		}
		if sp.Deny != "" {
			return fmt.Errorf("rollout stopped at stage %s: %s", stage.Name, sp.Deny)
		}
		if sp.Freeze == nil {
			w.resumeAfterFreeze(ctx, rs)
			return nil
		}
		if err := w.pauseForFreeze(ctx, stage, sp.Freeze, sp.Region, sp.Time, freezeRecheck, rs); err != nil {
			return err
		}
	}
}

// waitFreezeLegacy is the check of the workflows enqueued before the decision snapshot
// (Request.Policy empty): evaluated in the workflow, with the time from the step now. An
// override ignores every window, as it did.
func (w *Worker) waitFreezeLegacy(ctx dbos.Context, req Request, d *descriptor.Descriptor, stage rollout.Stage, rs *RolloutState) error {
	if d.FreezeOverride != "" {
		return nil
	}
	for {
		t, err := now(ctx)
		if err != nil {
			return err
		}
		var win *governance.WindowStatus
		region := ""
		for _, reg := range stage.Regions() {
			in := policyInput(governance.PhaseStage, req, d, t)
			in.Region = reg
			if dec, err := w.governance.Evaluate(ctx, in); err == nil && dec.Freeze != nil {
				win, region = dec.Freeze, reg
				break
			}
		}
		if win == nil {
			w.resumeAfterFreeze(ctx, rs)
			return nil
		}
		if err := w.pauseForFreeze(ctx, stage, win, region, t, 0, rs); err != nil {
			return err
		}
	}
}

// resumeAfterFreeze marks the rollout running again after a freeze pause.
func (w *Worker) resumeAfterFreeze(ctx dbos.Context, rs *RolloutState) {
	if rs.Status == service.RolloutPausedFreeze {
		rs.Status, rs.Paused = service.RolloutRunning, ""
		w.publishRollout(ctx, rs)
	}
}

// pauseForFreeze publishes the pause and sleeps (durably) until the end of the window or, with
// recheck > 0, at most recheck before checking again.
func (w *Worker) pauseForFreeze(ctx dbos.Context, stage rollout.Stage, win *governance.WindowStatus, region string, t time.Time, recheck time.Duration, rs *RolloutState) error {
	paused := fmt.Sprintf("freeze %q fino a %s", win.Name, win.End.Format(time.RFC3339))
	if region != "" {
		paused += " in " + region
	}
	// A recheck that finds the same window changes nothing: no new event and no new log line.
	if rs.Status != service.RolloutPausedFreeze || rs.Paused != paused {
		rs.Status, rs.Paused = service.RolloutPausedFreeze, paused
		w.publishRollout(ctx, rs)
		w.logger.Info("rollout in pausa per un freeze", "stage", stage.Name, "freeze", win.Name, "until", win.End)
	}
	d := max(win.End.Sub(t), time.Second)
	if recheck > 0 {
		d = min(d, recheck)
	}
	_, err := dbos.Sleep(ctx, d)
	return err
}

// tickPolicy is the output of the policy step of a schedule run.
type tickPolicy struct {
	Approval *governance.Approvals `json:"approval,omitempty"`
	Change   *governance.Change    `json:"change,omitempty"`
	Policy   *governance.Snapshot  `json:"policy,omitempty"`
	Deny     string                `json:"deny,omitempty"`
}

// evaluateTick evaluates the policy in force for a run of a recurring schedule: obligations
// are those of the run, not of the schedule's creation. Freezes apply at the deploy start.
func (w *Worker) evaluateTick(ctx context.Context, req Request, at time.Time) (tickPolicy, error) {
	in := policyInput(governance.PhaseTick, req, nil, at)
	in.Current = currentRelease(ctx, w.service, req.Descriptor.Application, w.reader.releaseArtifact(req.Descriptor.Artifact.Name), req.Descriptor.Environment, req.Descriptor.Tenant)
	dec, err := w.governance.Evaluate(ctx, in)
	if err != nil {
		return tickPolicy{}, err
	}
	if err := dec.Denied(); err != nil {
		return tickPolicy{Deny: err.Error()}, nil
	}
	return tickPolicy{Approval: dec.Approvals, Change: dec.Change, Policy: dec.Snapshot()}, nil
}

// freezeNameRe validates the names of runtime freeze windows.
var freezeNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// policies returns the policy administration, or an error if the server has none.
func (e *Engine) policyAdmin() (PolicyAdmin, error) {
	if e.policies == nil {
		return nil, fmt.Errorf("%w: governance policies and runtime freezes are not available on this server (database not reachable)", ErrInvalidRequest)
	}
	return e.policies, nil
}

// CreateFreeze implements PolicyService: an absolute window from From (default now) to To.
func (e *Engine) CreateFreeze(ctx context.Context, r FreezeRequest) (*governance.WindowStatus, error) {
	pa, err := e.policyAdmin()
	if err != nil {
		return nil, err
	}
	if !freezeNameRe.MatchString(r.Name) {
		return nil, fmt.Errorf("%w: invalid freeze name %q (letters, digits, '.', '_', '-', at most 64)", ErrInvalidRequest, r.Name)
	}
	if r.From.IsZero() {
		r.From = e.now().UTC()
	}
	w := governance.Window{
		Name: r.Name, Reason: r.Reason, Applications: r.Applications, Environments: r.Environments, Regions: r.Regions,
		Tenants: r.Tenants, Artifacts: r.Artifacts, From: r.From, To: r.To, Hard: r.Hard, CreatedBy: auth.FromContext(ctx).String(), CreatedAt: e.now().UTC(),
	}
	if w.CreatedBy == "" {
		w.CreatedBy = "unknown"
	}
	if errs := governance.PrepareWindows([]governance.Window{w}, "freeze"); len(errs) > 0 {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, errors.Join(errs...))
	}
	if !w.To.After(e.now()) {
		return nil, fmt.Errorf("%w: the freeze ends in the past", ErrInvalidRequest)
	}
	if err := pa.CreateFreeze(ctx, w); err != nil {
		return nil, err
	}
	e.logger.Warn("freeze creato", "freeze", w.Name, "applications", w.Applications, "environments", w.Environments,
		"from", w.From, "to", w.To, "hard", w.Hard, "created_by", w.CreatedBy)
	for _, st := range governance.ListWindows([]governance.Window{w}, "", "", e.now()) {
		return &st, nil
	}
	return nil, fmt.Errorf("freeze %s created but not listed", w.Name)
}

// DeleteFreeze implements PolicyService.
func (e *Engine) DeleteFreeze(ctx context.Context, name string) error {
	pa, err := e.policyAdmin()
	if err != nil {
		return err
	}
	if err := pa.DeleteFreeze(ctx, name); err != nil {
		return err
	}
	e.logger.Warn("freeze eliminato", "freeze", name, "by", auth.FromContext(ctx).String())
	return nil
}

// Policies implements PolicyService.
func (e *Engine) Policies(ctx context.Context) (*PolicyList, error) {
	pa, err := e.policyAdmin()
	if err != nil {
		return nil, err
	}
	return pa.List(ctx)
}

// PolicyRevision implements PolicyService.
func (e *Engine) PolicyRevision(ctx context.Context, revision int64) (*PolicyRevision, error) {
	pa, err := e.policyAdmin()
	if err != nil {
		return nil, err
	}
	return pa.Get(ctx, revision)
}

// PushPolicy implements PolicyService.
func (e *Engine) PushPolicy(ctx context.Context, r PolicyPush) (*PolicyRevision, error) {
	pa, err := e.policyAdmin()
	if err != nil {
		return nil, err
	}
	rev, err := pa.Push(ctx, r.Files, r.Comment, auth.FromContext(ctx).String())
	if err != nil {
		return nil, err
	}
	e.logger.Info("revisione della policy caricata", "revision", rev.Revision, "sha256", rev.SHA256, "author", rev.Author)
	return rev, nil
}

// ApprovePolicy implements PolicyService.
func (e *Engine) ApprovePolicy(ctx context.Context, revision int64) error {
	return e.activatePolicy(ctx, revision, true)
}

// ActivatePolicy implements PolicyService.
func (e *Engine) ActivatePolicy(ctx context.Context, revision int64) error {
	return e.activatePolicy(ctx, revision, false)
}

func (e *Engine) activatePolicy(ctx context.Context, revision int64, draft bool) error {
	pa, err := e.policyAdmin()
	if err != nil {
		return err
	}
	by := auth.FromContext(ctx).String()
	if err := pa.Activate(ctx, revision, by, draft); err != nil {
		return err
	}
	e.logger.Warn("revisione della policy attivata", "revision", revision, "by", by)
	return nil
}

// EvaluatePolicy implements PolicyService: the input is built as at submission, with the
// effective descriptor if the environment configuration is readable.
func (e *Engine) EvaluatePolicy(ctx context.Context, revision int64, r PolicyEvaluationRequest) (*PolicyEvaluation, error) {
	pa, err := e.policyAdmin()
	if err != nil {
		return nil, err
	}
	if r.Descriptor == nil {
		return nil, fmt.Errorf("%w: descriptor is required", ErrInvalidRequest)
	}
	phase := cmp.Or(r.Phase, governance.PhaseSubmit)
	if !slices.Contains([]string{governance.PhaseSubmit, governance.PhaseValidate, governance.PhaseStart, governance.PhaseStage, governance.PhaseTick}, phase) {
		return nil, fmt.Errorf("%w: invalid phase %q", ErrInvalidRequest, phase)
	}
	d := r.Descriptor
	var eff *descriptor.Descriptor
	if _, x, err := e.inspect(ctx, d); err == nil {
		eff = x
	}
	p := auth.FromContext(ctx)
	req := Request{Descriptor: *d, Kind: cmp.Or(r.Kind, KindDeploy), RequestedBy: p.String()}
	if p != nil {
		req.RequestedGroups = p.Groups
	}
	in := policyInput(phase, req, eff, e.now())
	in.Current = currentRelease(ctx, e, d.Application, e.releaseArtifact(d.Artifact.Name), d.Environment, d.Tenant)
	dec, err := pa.EvaluateRevision(ctx, revision, in)
	if err != nil {
		return nil, policyError(err)
	}
	in.Schema = governance.InputSchema
	in.Freezes.Active = []governance.WindowStatus{}
	if e.governance != nil {
		for _, w := range e.governance.Windows(in.Application, in.Environment, in.Now) {
			if w.Active && len(w.Regions) == 0 {
				in.Freezes.Active = append(in.Freezes.Active, w)
			}
		}
	}
	return &PolicyEvaluation{Input: in, Decision: dec}, nil
}

// maxImpactDeploys limits the deploys reevaluated by PolicyImpact.
const maxImpactDeploys = 500

// PolicyImpact implements PolicyService: it rebuilds the inputs of the real deploys created in
// the period from the recorded requests (with now = their creation) and compares the
// obligations recorded then with those of the revision. Freezes are not compared: they
// depend on the moment, not on the policy.
func (e *Engine) PolicyImpact(ctx context.Context, revision int64, since time.Duration) (*PolicyImpact, error) {
	pa, err := e.policyAdmin()
	if err != nil {
		return nil, err
	}
	if since <= 0 {
		since = 7 * 24 * time.Hour
	}
	from := e.now().Add(-since)
	out := &PolicyImpact{Since: from}
	var statuses []Status
	err = e.scan(nil, func(s Status) bool {
		if s.CreatedAt.Before(from) {
			return false
		}
		if !s.DryRun && s.Application != "" {
			statuses = append(statuses, s)
		}
		return len(statuses) < maxImpactDeploys
	})
	if err != nil {
		return nil, err
	}
	for _, s := range statuses {
		req, err := e.loadRequest(s.ID)
		if err != nil {
			continue
		}
		in := policyInput(governance.PhaseSubmit, req, nil, s.CreatedAt)
		dec, err := pa.EvaluateRevision(ctx, revision, in)
		if err != nil {
			return nil, policyError(err)
		}
		out.Revision, out.Evaluated = dec.Revision, out.Evaluated+1
		before := obligations(nil, req.Approval, req.Change)
		after := obligations(dec.Deny, dec.Approvals, dec.Change)
		if before != after {
			out.Changes = append(out.Changes, PolicyChange{
				Deployment: s.ID, Application: s.Application, Environment: s.Environment, Version: s.Version,
				CreatedAt: s.CreatedAt, Before: before, After: after,
			})
		}
	}
	return out, nil
}

// obligations summarizes denials and obligations for PolicyImpact.
func obligations(deny []governance.Denial, a *governance.Approvals, c *governance.Change) string {
	if len(deny) > 0 {
		codes := make([]string, len(deny))
		for i, d := range deny {
			codes[i] = cmp.Or(d.Code, d.Message)
		}
		return "deny: " + strings.Join(codes, ", ")
	}
	s := "approvals=" + strconv.Itoa(a.RequiredOrZero())
	if a != nil && a.Approvers != nil {
		s += " approvers=" + strings.Join(slices.Concat(a.Approvers.Groups, a.Approvers.Subjects), ",")
	}
	switch {
	case c == nil:
		s += " change=no"
	case c.Approval:
		s += " change=approval"
	default:
		s += " change=ticket"
	}
	return s
}
