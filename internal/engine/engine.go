// Package engine orchestrates the deploy pipeline as a durable DBOS workflow.
package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/google/uuid"

	"github.com/joeg-ita/manent/internal/auth"
	"github.com/joeg-ita/manent/internal/callback"
	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/governance"
	"github.com/joeg-ita/manent/internal/pipeline"
	"github.com/joeg-ita/manent/internal/rollout"
	"github.com/joeg-ita/manent/internal/service"
	"github.com/joeg-ita/manent/internal/telemetry"
)

// Stable names the workflows are registered with in DBOS.
// They must not change: running workflows and schedules refer to them by name.
const (
	workflowName          = "manent.deploy"
	scheduledWorkflowName = "manent.scheduled-deploy"
	approvalWorkflowName  = "manent.approval"
)

// Request is the workflow input, persisted by DBOS.
type Request struct {
	Descriptor descriptor.Descriptor `json:"descriptor"`
	Plan       pipeline.Plan         `json:"plan"`
	// RequestedBy is the identity that requested the deploy (or created the schedule), e.g. "oidc:alice".
	RequestedBy string `json:"requested_by,omitempty"`
	// Kind is deploy, promotion or rollback (empty = deploy).
	Kind string `json:"kind,omitempty"`
	// Source is the deploy a promotion or a manual rollback derives from.
	Source string `json:"source,omitempty"`
	// Release is the composite release the deploy belongs to.
	Release string `json:"release,omitempty"`
	// Approval, if set, are the approvals required before execution: the rule is the one valid
	// at request time (or when the schedule was created).
	Approval *governance.Approvals `json:"approval,omitempty"`
	// Change, if set, opens a change management ticket for the deploy and possibly waits for its
	// approval. Like Approval, it is the rule valid at request time.
	Change *governance.Change `json:"change,omitempty"`
	// Notify are the chat notification destinations (integrations.notifications), chosen at
	// request time: the workflow step sequence does not depend on the configuration read during
	// a recovery.
	Notify []string `json:"notify,omitempty"`
	// TraceParent is the trace (W3C traceparent) of the request: the worker's spans become its
	// children. Empty for recurring schedule runs.
	TraceParent string `json:"trace_parent,omitempty"`
}

// ServiceOptions configures the service.
type ServiceOptions struct {
	// Client is the DBOS client: a Worker's context or a standalone client (dbos.NewClient).
	Client    dbos.Client
	QueueName string
	Planner   pipeline.Planner
	// CallbackPolicy is applied to callback URLs already at submission, to reject at once a
	// descriptor the worker could not notify.
	CallbackPolicy callback.Policy
	// Sources, if set, rejects already at submission an artifact source that is not allowed.
	// The worker checks it again at download.
	Sources SourceChecker
	// Rollback, if set, rejects already at submission a rollback the policy does not allow.
	// The final decision is the worker's, after merging with the environment configuration.
	// It also decides whether an artifact allows manual rollback.
	Rollback RollbackPolicy
	// Resolver, if set, checks already at submission the descriptor policy and the compatibility
	// with the environment configuration. The worker checks it again.
	Resolver ConfigResolver
	// Governance, if set, applies mandatory promotion, approvals and freezes.
	Governance Governance
	// Notifications, if set, chooses the chat notification destinations.
	Notifications NotificationRouter
	// Health, if set, checks that rollout health checks can run.
	Health rollout.Checker
	// Traffic, if set, checks the load balancer and router configuration.
	Traffic TrafficOps
	// Executors, if set, provides Workers with the worker registry; StaleAfter is the heartbeat
	// delay beyond which a worker is considered lost.
	Executors  ExecutorLister
	StaleAfter time.Duration
	// TargetStates, if set, adds to the inventory the state read from the targets by the
	// reconciliation.
	TargetStates TargetStateReader
	Logger       *slog.Logger
	// Now returns the current time; replaceable in tests.
	Now func() time.Time
}

// NotificationRouter chooses the notification destinations of a deploy.
type NotificationRouter interface {
	Match(application, environment string, dryRun bool) []string
}

// SourceChecker checks that an artifact source is allowed.
type SourceChecker interface {
	Check(source string) error
}

// Governance provides the release rules and the freeze windows.
type Governance interface {
	Rule(application, environment string) governance.Rule
	Frozen(application, environment string, t time.Time) *governance.WindowStatus
	// FrozenIn also considers windows limited to a region (rollout passes).
	FrozenIn(application, environment, region string, t time.Time) *governance.WindowStatus
	Windows(application, environment string, t time.Time) []governance.WindowStatus
}

// Engine implements Service using only the DBOS client APIs: it enqueues and queries
// workflows but does not run them (Workers do).
type Engine struct {
	client     dbos.Client
	queueName  string
	planner    pipeline.Planner
	callbacks  callback.Policy
	rollback   RollbackPolicy
	sources    SourceChecker
	resolver   ConfigResolver
	governance Governance
	notify     NotificationRouter
	health     rollout.Checker
	traffic    TrafficOps
	executors  ExecutorLister
	targets    TargetStateReader
	staleAfter time.Duration
	logger     *slog.Logger
	now        func() time.Time
	// inv is the process inventory, updated incrementally.
	inv inventoryCache
}

// NewService creates the service on top of a DBOS client.
func NewService(opts ServiceOptions) *Engine {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Engine{
		client:     opts.Client,
		queueName:  opts.QueueName,
		planner:    opts.Planner,
		callbacks:  opts.CallbackPolicy,
		rollback:   opts.Rollback,
		sources:    opts.Sources,
		resolver:   opts.Resolver,
		governance: opts.Governance,
		notify:     opts.Notifications,
		health:     opts.Health,
		traffic:    opts.Traffic,
		executors:  opts.Executors,
		targets:    opts.TargetStates,
		staleAfter: cmp.Or(opts.StaleAfter, time.Minute),
		logger:     opts.Logger.With("component", "engine"),
		now:        opts.Now,
	}
}

// Plan delegates to the configured planner.
// The returned plan does not contain the specs of custom steps (commands and URLs), only
// type and hash.
func (e *Engine) Plan(_ context.Context, d *descriptor.Descriptor) (pipeline.Plan, error) {
	plan, err := e.planner.Plan(d)
	return plan.Public(), err
}

// submitMeta describes the origin of a deploy request.
type submitMeta struct {
	kind, source, release string
	// prechecked skips the policy and environment configuration check (check), already done by
	// whoever accepted the request (deploy of a release).
	prechecked bool
}

// Submit computes the plan and, depending on schedule in the descriptor:
//   - no schedule: enqueues the deploy at once;
//   - schedule.at: enqueues the deploy in DELAYED state until the given time;
//   - schedule.cron: creates or updates the recurring schedule.
//
// If governance rules require approvals, the deploy waits (AWAITING_APPROVAL) and is enqueued
// only after the approval.
// If the descriptor has an ID, it is used as workflow ID and makes the request idempotent.
func (e *Engine) Submit(ctx context.Context, d *descriptor.Descriptor) (*Submission, error) {
	return e.submit(ctx, d, submitMeta{kind: KindDeploy})
}

func (e *Engine) submit(ctx context.Context, d *descriptor.Descriptor, meta submitMeta) (*Submission, error) {
	if sub, ok := e.alreadyScheduled(ctx, d); ok {
		return sub, nil
	}
	plan, err := e.planner.Plan(d)
	if err != nil {
		return nil, err
	}
	var eff *descriptor.Descriptor
	if !meta.prechecked {
		if eff, err = e.check(ctx, d); err != nil {
			return nil, err
		}
	}
	req := Request{
		Descriptor: *d, Plan: plan, RequestedBy: auth.FromContext(ctx).String(), Kind: meta.kind, Source: meta.source,
		Release: meta.release, TraceParent: telemetry.TraceParent(ctx),
	}
	if req.Approval, req.Change, err = e.govern(ctx, d); err != nil {
		return nil, err
	}
	if eff != nil {
		if err := checkRolloutApprovals(eff, req.Approval); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
		}
	}
	if e.notify != nil {
		req.Notify = e.notify.Match(d.Application, d.Environment, d.DryRun)
	}
	// With dependencies not yet successful the deploy waits: then approval and enqueuing.
	wait := false
	if len(d.After) > 0 {
		done, err := e.checkAfter(ctx, d)
		if err != nil {
			return nil, err
		}
		wait = !done
	}
	if d.Schedule != nil && d.Schedule.Cron != "" {
		return e.applySchedule(req)
	}

	sub := &Submission{State: string(dbos.WorkflowStatusEnqueued), DryRun: d.DryRun}
	var delay time.Duration
	if d.Schedule != nil && d.Schedule.At != nil {
		if delay = d.Schedule.At.Sub(e.now()); delay <= 0 {
			return nil, fmt.Errorf("%w: schedule.at %s è nel passato", ErrInvalidRequest, d.Schedule.At.Format(time.RFC3339))
		}
		sub.State = string(dbos.WorkflowStatusDelayed)
		sub.ScheduledAt = d.Schedule.At
	}
	var id string
	if d.ID != "" {
		id = workflowID(d)
	}
	if wait {
		if id == "" {
			id = uuid.NewString()
		}
		if err := enqueueAfter(e.client, e.queueName, req, id); err != nil {
			return nil, err
		}
		sub.ID, sub.State = id, StateWaitingDependency
		e.logger.Info("deploy in attesa delle dipendenze", "deployment", id, "application", d.Application, "environment", d.Environment, "after", d.After, "requested_by", req.RequestedBy)
		return sub, nil
	}
	if needsApproval(req) {
		// The deploy does not exist yet: the ID must be chosen now, because it is the one to approve.
		if id == "" {
			id = uuid.NewString()
		}
		if err := enqueueApproval(e.client, e.queueName, req, id); err != nil {
			return nil, err
		}
		sub.ID, sub.State = id, StateAwaitingApproval
		e.logger.Info("deploy in attesa di approvazione", "deployment", id, "application", d.Application, "environment", d.Environment,
			"required", req.Approval.RequiredOrZero(), "change_approval", req.Change != nil && req.Change.Approval, "requested_by", req.RequestedBy)
		return sub, nil
	}
	opts := deployOptions(req, id)
	if delay > 0 {
		opts = append(opts, dbos.WithEnqueueDelay(delay))
	}
	h, err := dbos.Enqueue[Result](e.client, e.queueName, workflowName, req, opts...)
	if err != nil {
		return nil, err
	}
	sub.ID = h.GetWorkflowID()
	e.logger.Info("deploy accodato", "deployment", sub.ID, "application", d.Application, "environment", d.Environment, "state", sub.State, "dry_run", d.DryRun, "kind", meta.kind, "requested_by", req.RequestedBy)
	return sub, nil
}

// alreadyScheduled makes idempotent the resubmission of a deploy with id and schedule.at after
// the given time (e.g. a retrying CI): if the deploy already exists for the same application
// and environment it returns it, instead of rejecting the time as past. Before the time,
// enqueuing with the same workflow ID already takes care of it.
func (e *Engine) alreadyScheduled(ctx context.Context, d *descriptor.Descriptor) (*Submission, bool) {
	if d.ID == "" || d.DryRun || d.Schedule == nil || d.Schedule.At == nil || d.Schedule.At.After(e.now()) {
		return nil, false
	}
	st, err := e.Status(ctx, workflowID(d))
	if err != nil || st.Application != d.Application || st.Environment != d.Environment {
		return nil, false
	}
	return &Submission{ID: st.ID, State: st.State, ScheduledAt: d.Schedule.At}, true
}

// check rejects at once what the worker could not run: callbacks and sources not allowed,
// descriptor violating the policy or incompatible with the environment configuration,
// rollback not allowed. If the environment configuration is unreachable the check is left to
// the worker. It returns the effective descriptor, or nil if unknown.
func (e *Engine) check(ctx context.Context, d *descriptor.Descriptor) (*descriptor.Descriptor, error) {
	v, eff, err := e.inspect(ctx, d)
	if err != nil {
		return nil, err
	}
	for _, w := range v.Warnings {
		e.logger.Warn(w, "application", d.Application, "environment", d.Environment)
	}
	return eff, nil
}

// Validate checks the descriptor as at submission, without enqueuing it, and returns the plan
// and the effective deploy: environment configuration, rollback and governance rules.
// The client uses it for "manent validate", since it has no access to the environment configuration.
func (e *Engine) Validate(ctx context.Context, d *descriptor.Descriptor) (*Validation, error) {
	plan, err := e.planner.Plan(d)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	v, _, err := e.inspect(ctx, d)
	if err != nil {
		return nil, err
	}
	v.Plan = plan.Public()
	e.describeGovernance(v, d)
	return v, nil
}

// describeGovernance adds to the validation the governance rules that apply to the deploy.
func (e *Engine) describeGovernance(v *Validation, d *descriptor.Descriptor) {
	if e.governance == nil {
		return
	}
	rule := e.governance.Rule(d.Application, d.Environment)
	v.Governance = &GovernanceValidation{Rule: rule.Name, Requires: rule.Requires, Approvals: rule.Approvals.Required}
	at := e.now()
	if d.Schedule != nil && d.Schedule.At != nil {
		at = *d.Schedule.At
	}
	v.Governance.Freeze = e.governance.Frozen(d.Application, d.Environment, at)
}

// CandidateResolver merges a descriptor with a given environment configuration instead of the
// provider's (envconfig.Resolver).
type CandidateResolver interface {
	ResolveWith(ctx context.Context, d descriptor.Descriptor, config []byte) (descriptor.Descriptor, map[string]string, error)
}

// ValidateEnvironment checks a candidate environment configuration as the deploy of the
// example descriptor would use it at submission: strict parsing, descriptor policy, merge,
// rollback, wave and traffic plan, plugins and health checks. Nothing is enqueued or written.
func (e *Engine) ValidateEnvironment(ctx context.Context, c EnvironmentCheck) (*Validation, error) {
	cr, ok := e.resolver.(CandidateResolver)
	if !ok {
		return nil, fmt.Errorf("%w: il server non gestisce la configurazione di ambiente", ErrInvalidRequest)
	}
	d, err := e.exampleDescriptor(ctx, c)
	if err != nil {
		return nil, err
	}
	plan, err := e.planner.Plan(d)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	v, _, err := e.inspectWith(ctx, d, func(ctx context.Context, d descriptor.Descriptor) (descriptor.Descriptor, map[string]string, error) {
		return cr.ResolveWith(ctx, d, []byte(c.Config))
	})
	if err != nil {
		return nil, err
	}
	v.Plan = plan.Public()
	e.describeGovernance(v, d)
	return v, nil
}

// exampleDescriptor returns the descriptor to merge with a candidate configuration: the given
// one, or the request of the deploy in service in the environment, without schedule and
// dependencies (they do not concern the configuration).
func (e *Engine) exampleDescriptor(ctx context.Context, c EnvironmentCheck) (*descriptor.Descriptor, error) {
	if strings.TrimSpace(c.Config) == "" {
		return nil, fmt.Errorf("%w: configurazione vuota", ErrInvalidRequest)
	}
	if c.Descriptor != nil {
		d := *c.Descriptor
		if d.Application != c.Application || d.Environment != c.Environment {
			return nil, fmt.Errorf("%w: il descrittore di esempio è di %s/%s, non di %s/%s", ErrInvalidRequest, d.Application, d.Environment, c.Application, c.Environment)
		}
		return &d, nil
	}
	inv, err := e.Inventory(ctx, InventoryFilter{Application: c.Application, Environment: c.Environment})
	if err != nil {
		return nil, err
	}
	if len(inv) == 0 || inv[0].Current == nil {
		return nil, fmt.Errorf("%w: nessun deploy in esercizio in %s/%s: indicare un descrittore di esempio", ErrInvalidRequest, c.Application, c.Environment)
	}
	req, err := e.loadRequest(inv[0].Current.ID)
	if err != nil {
		return nil, err
	}
	d := req.Descriptor
	d.ID, d.Schedule, d.After, d.AfterTimeout, d.DryRun = "", nil, nil, "", false
	return &d, nil
}

// inspect runs the check verifications and describes the effective deploy. A permanent error
// makes the request invalid; an unreachable environment configuration only produces a
// warning, and the returned effective descriptor is nil.
func (e *Engine) inspect(ctx context.Context, d *descriptor.Descriptor) (*Validation, *descriptor.Descriptor, error) {
	var resolve func(context.Context, descriptor.Descriptor) (descriptor.Descriptor, map[string]string, error)
	if e.resolver != nil {
		resolve = e.resolver.Resolve
	}
	return e.inspectWith(ctx, d, resolve)
}

// inspectWith is inspect with the given environment configuration resolution (nil: none).
func (e *Engine) inspectWith(ctx context.Context, d *descriptor.Descriptor,
	resolve func(context.Context, descriptor.Descriptor) (descriptor.Descriptor, map[string]string, error)) (*Validation, *descriptor.Descriptor, error) {
	for _, cb := range d.Callbacks {
		if err := e.callbacks.Check(cb.URL); err != nil {
			return nil, nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
		}
	}
	if e.sources != nil {
		if err := e.sources.Check(d.Artifact.Source); err != nil {
			return nil, nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
		}
	}
	v := &Validation{Valid: true}
	eff := *d
	if resolve != nil {
		r, meta, err := resolve(ctx, *d)
		switch {
		case errors.Is(err, pipeline.ErrPermanent):
			return nil, nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
		case err != nil:
			v.Warnings = append(v.Warnings, "configurazione di ambiente non verificabile all'accodamento: la verifica resta al worker ("+err.Error()+")")
		default:
			eff, v.Config = r, meta
		}
	}
	if e.rollback != nil {
		r, err := e.rollback.Apply(eff)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
		}
		eff = r
	}
	if eff.Rollout != nil && v.Config != nil {
		// With the environment configuration read, the wave plan is the final one.
		p, err := checkRollout(eff, e.health, e.traffic)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: rollout: %w", ErrInvalidRequest, err)
		}
		v.Rollout = newRolloutState(p, eff.Rollout)
		if eff.Rollout.IsTraffic() {
			v.Rollout = newTrafficState(&eff, p)
		}
		v.Rollout.Status = "planned"
	}
	// Without the environment configuration, required parameters and secrets may still come from
	// there: only the values already present are checked.
	plan, err := e.planner.Plan(d)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	if err := checkCustomSteps(plan, eff, e.resolver == nil || v.Config != nil); err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	v.Deploy, v.Rollback = &eff.Deploy, eff.Rollback
	for _, sec := range eff.Secrets {
		v.Secrets = append(v.Secrets, sec.Name)
	}
	if e.resolver == nil || v.Config != nil {
		return v, &eff, nil
	}
	return v, nil, nil
}

// govern applies the governance rules to a real deploy: freeze, mandatory promotion and
// approvals. It returns the required approvals, or nil. Dry-runs are not subject to the rules
// because they do not change the environments.
func (e *Engine) govern(ctx context.Context, d *descriptor.Descriptor) (*governance.Approvals, *governance.Change, error) {
	if e.governance == nil || d.DryRun {
		return nil, nil, nil
	}
	if d.FreezeOverride == "" && (d.Schedule == nil || d.Schedule.Cron == "") {
		at := e.now()
		if d.Schedule != nil && d.Schedule.At != nil {
			at = *d.Schedule.At
		}
		if w := e.governance.Frozen(d.Application, d.Environment, at); w != nil {
			return nil, nil, fmt.Errorf("%w: %s", ErrInvalidRequest, freezeMessage(w))
		}
	}
	rule := e.governance.Rule(d.Application, d.Environment)
	for _, env := range rule.Requires {
		rel, err := e.findRelease(ctx, d.Application, env, func(s Status) bool { return sameArtifact(s, d) })
		if err != nil {
			return nil, nil, err
		}
		if rel == nil {
			return nil, nil, fmt.Errorf("%w: %s %s non risulta rilasciato con successo in %s: va promosso da lì (governance)", ErrInvalidRequest, d.Artifact.Name, d.Artifact.Version, env)
		}
	}
	var approvals *governance.Approvals
	if rule.Approvals.Required > 0 {
		a := rule.Approvals
		approvals = &a
	}
	var change *governance.Change
	if rule.Change != nil {
		c := *rule.Change
		change = &c
	}
	return approvals, change, nil
}

// changeTicket reads the change ticket from the workflow attributes.
func changeTicket(st dbos.WorkflowStatus) *ChangeTicket {
	n := attr(st, attrChange)
	if n == "" {
		return nil
	}
	return &ChangeTicket{Number: n, URL: attr(st, attrChangeURL)}
}

// needsApproval reports whether the deploy waits for an approval before enqueuing: by manent
// identities, by the change in the ITSM or both.
// Approvals moved into the rollout (approvals.when: rollout) do not count.
func needsApproval(req Request) bool {
	return (req.Approval != nil && !req.Approval.InRollout()) || (req.Change != nil && req.Change.Approval)
}

// checkRolloutApprovals checks that a deploy with approvals moved into the rollout has a
// rollout with approval points that are actually asked: otherwise it would start without
// approvals.
func checkRolloutApprovals(d *descriptor.Descriptor, a *governance.Approvals) error {
	if !a.InRollout() || rolloutAsksApproval(d.Rollout) {
		return nil
	}
	return errors.New("la regola di governance sposta le approvazioni nel rollout (approvals.when: rollout), ma il rollout non ha punti di approvazione in approvalsBetween (dopo un'ondata che non sia l'ultima, o un gradino del traffico)")
}

// rolloutAsksApproval reports whether the rollout pauses for an approval at least once. After
// the last wave there is nothing left to approve, so that point does not count. With the
// traffic strategy every point counts: on the first release, which skips the intermediate
// steps, runTraffic asks the approval before shifting traffic.
func rolloutAsksApproval(r *descriptor.Rollout) bool {
	if r == nil {
		return false
	}
	for _, a := range r.ApprovalsBetween {
		if r.IsTraffic() {
			return true
		}
		for _, w := range r.Waves[:max(len(r.Waves)-1, 0)] {
			if w.Name == a {
				return true
			}
		}
	}
	return false
}

// freezeMessage describes the freeze blocking a deploy.
func freezeMessage(w *governance.WindowStatus) string {
	msg := fmt.Sprintf("deploy bloccato dal freeze %q fino a %s", w.Name, w.End.Format(time.RFC3339))
	if w.Reason != "" {
		msg += " (" + w.Reason + ")"
	}
	return msg + "; per un'emergenza indicare freezeOverride con il motivo"
}

// sameArtifact reports whether the recorded deploy released the same artifact as the
// descriptor: same name and version and, if both known, same sha256.
func sameArtifact(s Status, d *descriptor.Descriptor) bool {
	if s.Artifact != d.Artifact.Name || s.Version != d.Artifact.Version {
		return false
	}
	return d.Artifact.SHA256 == "" || s.SHA256 == "" || strings.EqualFold(s.SHA256, d.Artifact.SHA256)
}

// deployOptions are the enqueue options of a deploy: queue partition, user, attributes for
// listings and inventory and, if given, workflow ID.
func deployOptions(req Request, id string) []dbos.EnqueueOption {
	opts := []dbos.EnqueueOption{
		dbos.WithEnqueueQueuePartitionKey(partitionKey(&req.Descriptor)),
		dbos.WithEnqueueAttributes(attributes(req)),
	}
	if req.RequestedBy != "" {
		opts = append(opts, dbos.WithEnqueueAuthenticatedUser(req.RequestedBy))
	}
	if id != "" {
		opts = append(opts, dbos.WithEnqueueWorkflowID(id))
	}
	return opts
}

// Workflow attribute keys. They are recorded at creation and updated at the end of the
// deploy (outcome, sha256, rollback): they enable listings and inventory without loading the input.
const (
	attrApplication = "application"
	attrEnvironment = "environment"
	attrArtifact    = "artifact"
	attrVersion     = "version"
	attrKind        = "kind"
	attrSource      = "source"
	attrDryRun      = "dry_run"
	attrApproval    = "approval"
	attrOutcome     = "outcome"
	attrSHA256      = "sha256"
	attrRollback    = "rollback"
	// attrChange and attrChangeURL are number and link of the change ticket.
	attrChange    = "change"
	attrChangeURL = "change_url"
	// attrRolloutProgress summarizes a wave rollout: "stages passed/total stages".
	attrRolloutProgress = "rollout_progress"
)

// attributes returns the initial attributes of a workflow.
func attributes(req Request) map[string]any {
	d := req.Descriptor
	a := map[string]any{
		attrApplication: d.Application,
		attrEnvironment: d.Environment,
		attrArtifact:    d.Artifact.Name,
		attrVersion:     d.Artifact.Version,
		attrKind:        cmp.Or(req.Kind, KindDeploy),
	}
	if req.Source != "" {
		a[attrSource] = req.Source
	}
	if d.DryRun {
		a[attrDryRun] = "true"
	}
	if needsApproval(req) {
		a[attrApproval] = "required"
	}
	if len(d.After) > 0 {
		a[attrAfter] = "true"
	}
	if req.Release != "" {
		a[attrRelease] = req.Release
	}
	return a
}

// Status reads the deploy state from the DBOS system database: with a single query the deploy
// workflow and the approval and dependency wait ones. A waiting deploy has no workflow yet:
// its state is the approval's or the wait's.
func (e *Engine) Status(_ context.Context, id string) (*Status, error) {
	ws, err := dbos.ListWorkflows(e.client, dbos.WithFilterWorkflowIDs(id, id+approvalSuffix, id+afterSuffix),
		dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(true))
	if err != nil {
		return nil, err
	}
	g := groupDeploy(id, ws)
	if g.deploy == nil {
		return e.pendingStatus(id, g)
	}
	st := *g.deploy
	s := toStatus(st)
	e.readRollout(&s, st)
	if st.Attributes[attrApproval] != nil && s.Approval == nil && g.approval != nil {
		s.Approval = e.approval(*g.approval)
	}
	if attr(st, attrAfter) == "true" && g.after != nil {
		s.After = e.afterState(*g.after)
	}
	if st.Status == dbos.WorkflowStatusSuccess {
		var res Result
		if err := decodeInput(st.Output, &res); err != nil {
			return nil, fmt.Errorf("risultato del deploy %s: %w", id, err)
		}
		s.Result = &res
	}
	return &s, nil
}

// deployWorkflows are the workflows that represent a deploy: the deploy itself, the approval
// and the dependency wait, each nil if missing.
type deployWorkflows struct {
	deploy, approval, after *dbos.WorkflowStatus
}

// groupDeploy finds among the read workflows those of deploy id.
func groupDeploy(id string, ws []dbos.WorkflowStatus) deployWorkflows {
	var g deployWorkflows
	for i := range ws {
		w := &ws[i]
		switch {
		case w.ID == id:
			g.deploy = w
		case w.ID == id+approvalSuffix && w.Name == approvalWorkflowName:
			g.approval = w
		case w.ID == id+afterSuffix && w.Name == afterWorkflowName:
			g.after = w
		}
	}
	return g
}

// readRollout adds to s the rollout state of deploy st, if it must be read (readsRollout).
func (e *Engine) readRollout(s *Status, st dbos.WorkflowStatus) {
	if !readsRollout(st) {
		return
	}
	if rs, err := dbos.GetEvent[RolloutState](e.client, st.ID, rolloutEvent, 0); err == nil && rs.Status != "" {
		s.Rollout = &rs
		if awaitsDecision(rs.Status) && st.Status == dbos.WorkflowStatusPending {
			// Approval between two waves or steps, or weights changed by others: the deploy is running
			// but waits for a decision.
			s.State, s.Approval = StateAwaitingApproval, rs.Approval
		}
	}
}

// readsRollout says whether the rollout state must be read from the event: for running
// deploys and for those ended with a rollout (attribute recorded at the end) or interrupted
// before the end.
func readsRollout(st dbos.WorkflowStatus) bool {
	switch st.Status {
	case dbos.WorkflowStatusPending, dbos.WorkflowStatusCancelled, dbos.WorkflowStatusMaxRecoveryAttemptsExceeded:
		return true
	}
	return attr(st, attrRolloutProgress) != ""
}

// maxListScan limits the deploys examined by a filtered listing or by the inventory.
const maxListScan = 10000

// List lists the deploys from the most recent, including those awaiting approval or rejected.
// With f.Schedule it returns only the runs of that schedule. Filters are applied page by page,
// until the limit is reached.
func (e *Engine) List(_ context.Context, f ListFilter) ([]Status, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	var prefix []string
	if f.Schedule != "" {
		prefix = []string{scheduledIDPrefix(f.Schedule)}
	}
	out := []Status{}
	// With a cursor the listing restarts from the deploy after the given one: the scan starts
	// from its creation time, then skips deploys up to and including the cursor.
	var from time.Time
	skipping := f.Cursor != ""
	if skipping {
		at, err := e.createdAt(f.Cursor)
		if err != nil {
			return nil, err
		}
		if at.IsZero() {
			// The cursor's deploy no longer exists (e.g. retention): the listing is over.
			return out, nil
		}
		from = at
	}
	err := e.scanFrom(prefix, from, func(s Status) bool {
		if skipping {
			skipping = s.ID != f.Cursor
			return true
		}
		if f.Matches(s) {
			out = append(out, s)
		}
		return len(out) < f.Limit
	})
	return out, err
}

// createdAt returns the most recent creation time among the workflows of deploy id (deploy,
// approval, wait), or zero if none exists.
func (e *Engine) createdAt(id string) (time.Time, error) {
	ws, err := dbos.ListWorkflows(e.client, dbos.WithFilterWorkflowIDs(id, id+approvalSuffix, id+afterSuffix),
		dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false))
	if err != nil {
		return time.Time{}, err
	}
	var at time.Time
	for _, w := range ws {
		if w.CreatedAt.After(at) {
			at = w.CreatedAt
		}
	}
	return at, nil
}

// scan goes through deploys and approvals from the most recent, in pages, while fn returns true.
func (e *Engine) scan(prefix []string, fn func(Status) bool) error {
	return e.scanFrom(prefix, time.Time{}, fn)
}

// scanFrom is scan starting from the workflows created up to and including from (zero: from
// the most recent). Approvals ended with a deploy and satisfied waits are not reported: what
// they enqueued represents them.
func (e *Engine) scanFrom(prefix []string, from time.Time, fn func(Status) bool) error {
	return e.scanRawFrom(prefix, from, func(w dbos.WorkflowStatus) bool {
		return superseded(w) || fn(toStatus(w))
	})
}

// superseded says whether the workflow is an approval ended with a deploy or a satisfied
// wait: the deploy they enqueued represents them.
func superseded(w dbos.WorkflowStatus) bool {
	return (w.Name == approvalWorkflowName && attr(w, attrOutcome) == ApprovalApproved) ||
		(w.Name == afterWorkflowName && attr(w, attrOutcome) == service.AfterSatisfied)
}

// scanRaw goes through deploys, approvals and waits from the most recent, unfiltered, while
// fn returns true.
func (e *Engine) scanRaw(prefix []string, fn func(dbos.WorkflowStatus) bool) error {
	return e.scanRawFrom(prefix, time.Time{}, fn)
}

// scanRawFrom is scanRaw starting from the workflows created up to and including from. Pages
// are by key (creation time), not by offset: every page costs one query on the created_at
// index, wherever it is in the history.
//
// The order is deterministic, by creation time and then by ID (descending): DBOS orders only
// by time, and workflows created in the same millisecond can come back in a different order
// from one query to the next. So a group with the same time is returned only once read
// entirely: the last group of a full page is read again with the next page.
func (e *Engine) scanRawFrom(prefix []string, from time.Time, fn func(dbos.WorkflowStatus) bool) error {
	const page = 200
	list := func(opts ...dbos.ListWorkflowsOption) ([]dbos.WorkflowStatus, error) {
		opts = append(opts,
			dbos.WithFilterName(workflowName, approvalWorkflowName, afterWorkflowName),
			dbos.WithFilterSortDesc(),
			dbos.WithFilterLoadInput(false),
			dbos.WithFilterLoadOutput(false),
		)
		if len(prefix) > 0 {
			opts = append(opts, dbos.WithFilterWorkflowIDPrefix(prefix...))
		}
		return dbos.ListWorkflows(e.client, opts...)
	}
	for scanned := 0; scanned < maxListScan; {
		opts := []dbos.ListWorkflowsOption{dbos.WithFilterLimit(page)}
		if !from.IsZero() {
			opts = append(opts, dbos.WithFilterCreatedBefore(from))
		}
		wfs, err := list(opts...)
		if err != nil {
			return err
		}
		last := len(wfs) == page
		if last {
			boundary := wfs[len(wfs)-1].CreatedAt
			if wfs[0].CreatedAt.Equal(boundary) {
				// A whole page in the same millisecond: read the whole group.
				if wfs, err = list(dbos.WithFilterCreatedAfter(boundary), dbos.WithFilterCreatedBefore(boundary)); err != nil {
					return err
				}
				from = boundary.Add(-time.Millisecond)
			} else {
				// The last group may be incomplete: it is read again with the next page.
				wfs = slices.DeleteFunc(wfs, func(w dbos.WorkflowStatus) bool { return w.CreatedAt.Equal(boundary) })
				from = boundary
			}
		}
		slices.SortStableFunc(wfs, func(x, y dbos.WorkflowStatus) int {
			return cmp.Or(y.CreatedAt.Compare(x.CreatedAt), strings.Compare(y.ID, x.ID))
		})
		for _, w := range wfs {
			scanned++
			if !fn(w) {
				return nil
			}
		}
		if !last {
			return nil
		}
	}
	return nil
}

// Wait blocks until the workflow ends, or until ctx expires if it has a deadline.
// The deploy runs on a Worker: without live workers it stays queued. For a deploy awaiting
// approval it first waits for the decision.
func (e *Engine) Wait(ctx context.Context, id string) (*Result, error) {
	var opts []dbos.GetResultOption
	if deadline, ok := ctx.Deadline(); ok {
		opts = append(opts, dbos.WithHandleTimeout(time.Until(deadline)))
	}
	// Before execution the deploy may wait for dependencies, then for approval: each one, if
	// successful, enqueues the next before ending.
	for range 3 {
		if h, err := dbos.RetrieveWorkflow[Result](e.client, id); err == nil {
			res, err := h.GetResult(opts...)
			if err != nil {
				return nil, err
			}
			return &res, nil
		}
		if ah, err := dbos.RetrieveWorkflow[ApprovalState](e.client, id+approvalSuffix); err == nil {
			a, err := ah.GetResult(opts...)
			if err != nil {
				return nil, err
			}
			if a.Status != ApprovalApproved {
				return nil, fmt.Errorf("deploy %s non eseguito: approvazione %s", id, a.Status)
			}
			continue
		}
		if wh, err := dbos.RetrieveWorkflow[AfterState](e.client, id+afterSuffix); err == nil {
			a, err := wh.GetResult(opts...)
			if err != nil {
				return nil, err
			}
			if a.Status != service.AfterSatisfied {
				return nil, fmt.Errorf("deploy %s non eseguito: dipendenze %s (%s)", id, a.Status, a.Reason)
			}
			continue
		}
		break
	}
	return nil, fmt.Errorf("%w: deploy %s", ErrNotFound, id)
}

// Cancel cancels the deploy: if waiting it is removed from the queue, if running it stops at
// the start of the next step, if awaiting approval it will not run.
func (e *Engine) Cancel(ctx context.Context, id string) error {
	st, err := e.Status(ctx, id)
	if err != nil {
		return err
	}
	target := id
	switch st.State {
	case StateAwaitingApproval:
		if st.Rollout == nil {
			target = id + approvalSuffix
		}
	case StateWaitingDependency:
		target = id + afterSuffix
	}
	if err := dbos.CancelWorkflow(e.client, target); err != nil {
		return err
	}
	e.notifyWaiters(id)
	e.logger.Info("deploy annullato", "deployment", id)
	return nil
}

// toStatus converts the DBOS state to the format exposed by manent.
// Attributes allow applying permissions and building the inventory without loading the
// workflow input.
func toStatus(st dbos.WorkflowStatus) Status {
	s := Status{
		ID: st.ID, State: string(st.Status), CreatedAt: st.CreatedAt, UpdatedAt: st.UpdatedAt, RequestedBy: st.AuthenticatedUser,
		Application: attr(st, attrApplication), Environment: attr(st, attrEnvironment),
		Artifact: attr(st, attrArtifact), Version: attr(st, attrVersion), SHA256: attr(st, attrSHA256),
		Kind: attr(st, attrKind), Source: attr(st, attrSource), DryRun: attr(st, attrDryRun) == "true",
		Rollback: attr(st, attrRollback), RolloutProgress: attr(st, attrRolloutProgress), Change: changeTicket(st),
	}
	if s.Application == "" {
		// Deploys enqueued before attributes existed: the partition key is "<application>/<environment>".
		s.Application, s.Environment, _ = strings.Cut(st.QueuePartitionKey, "/")
	}
	s.Release = attr(st, attrRelease)
	switch st.Name {
	case approvalWorkflowName:
		s.ID = strings.TrimSuffix(st.ID, approvalSuffix)
		s.State = approvalWorkflowState(st.Status, attr(st, attrOutcome))
		s.Error = ""
		return s
	case afterWorkflowName:
		s.ID = strings.TrimSuffix(st.ID, afterSuffix)
		s.State = afterWorkflowState(st.Status, attr(st, attrOutcome))
		s.Error = ""
		return s
	}
	if !st.DelayUntil.IsZero() {
		at := st.DelayUntil
		s.ScheduledAt = &at
	}
	// A queued deploy has no worker yet: the recorded executor is the one that enqueued it.
	if st.Status != dbos.WorkflowStatusEnqueued && st.Status != dbos.WorkflowStatusDelayed && !st.StartedAt.IsZero() {
		at := st.StartedAt.UTC()
		s.StartedAt, s.Executor = &at, st.ExecutorID
	}
	s.Queue = st.QueueName
	if st.Status == dbos.WorkflowStatusError && st.Error != nil {
		s.Error = st.Error.Error()
	}
	return s
}

// attr returns a string attribute of the workflow.
func attr(st dbos.WorkflowStatus, key string) string {
	v, _ := st.Attributes[key].(string)
	return v
}

// dryRunSuffix tells a dry-run's workflow ID apart from the real deploy's.
const dryRunSuffix = ".dry-run"

// workflowID returns the workflow ID for a descriptor with an ID. A dry-run uses a separate
// ID: otherwise, because of idempotency, the real deploy with the same ID would never run.
func workflowID(d *descriptor.Descriptor) string {
	if d.DryRun {
		return d.ID + dryRunSuffix
	}
	return d.ID
}

// partitionKey is the queue partition key: deploys of the same application/environment obey
// the environment_concurrency limit across all workers.
func partitionKey(d *descriptor.Descriptor) string {
	return d.Application + "/" + d.Environment
}
