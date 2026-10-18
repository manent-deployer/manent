package authz

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/joeg-ita/manent/internal/audit"
	"github.com/joeg-ita/manent/internal/auth"
	"github.com/joeg-ita/manent/internal/deploylog"
	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/engine"
	"github.com/joeg-ita/manent/internal/governance"
	"github.com/joeg-ita/manent/internal/pipeline"
)

// Guard wraps an engine.Service: it checks the permissions of the identity in the context and
// records in the audit the state-changing actions and the denied ones. A denied action returns
// engine.ErrForbidden.
type Guard struct {
	Next   engine.Service
	Policy Policy
	Audit  audit.Recorder
	Logger *slog.Logger
}

var _ engine.Service = (*Guard)(nil)

// target identifies the object of an action, for permissions and audit.
type target struct {
	application, environment, deployment, schedule string
	dryRun                                         bool
	// comment is the comment of an approval or the reason of a freeze override.
	comment string
}

// check verifies the permission; if denied it records the audit entry and returns the error.
func (g *Guard) check(ctx context.Context, a Action, t target) error {
	p := auth.FromContext(ctx)
	if g.Policy.Allowed(p, a, t.application, t.environment) {
		return nil
	}
	err := fmt.Errorf("%w: %s cannot perform %s on %s/%s", engine.ErrForbidden, actor(p), a, t.application, t.environment)
	g.record(ctx, a, t, audit.OutcomeDenied, err)
	return err
}

// record records an audit entry; a recording error is only logged.
func (g *Guard) record(ctx context.Context, a Action, t target, outcome string, err error) {
	if g.Audit == nil {
		return
	}
	e := audit.Entry{
		Actor: actor(auth.FromContext(ctx)), Action: string(a), Outcome: outcome,
		Application: t.application, Environment: t.environment,
		Deployment: t.deployment, Schedule: t.schedule, DryRun: t.dryRun, Comment: t.comment,
	}
	if err != nil {
		e.Reason = err.Error()
	}
	if rerr := g.Audit.Record(ctx, e); rerr != nil {
		g.Logger.Error("registrazione nell'audit fallita", "action", a, "error", rerr)
	}
}

// done records the outcome of an authorized action.
func (g *Guard) done(ctx context.Context, a Action, t target, err error) {
	outcome := audit.OutcomeAllowed
	if err != nil {
		outcome = audit.OutcomeError
	}
	g.record(ctx, a, t, outcome, err)
}

func actor(p *auth.Principal) string {
	if p == nil {
		return "unauthenticated"
	}
	return p.String()
}

// Plan requires deployment.read: it reveals the pipeline configured for the application/environment.
func (g *Guard) Plan(ctx context.Context, d *descriptor.Descriptor) (pipeline.Plan, error) {
	if err := g.check(ctx, DeploymentRead, target{application: d.Application, environment: d.Environment}); err != nil {
		return nil, err
	}
	return g.Next.Plan(ctx, d)
}

// submitAction is the action required for a real deploy (act) or for a dry-run.
func submitAction(act Action, dryRun bool) Action {
	if dryRun {
		return DeploymentDryRun
	}
	return act
}

// checkOverride checks the freeze override permission, if requested.
func (g *Guard) checkOverride(ctx context.Context, override string, t target) error {
	if override == "" {
		return nil
	}
	return g.check(ctx, FreezeOverride, t)
}

// Submit requires deployment.submit (deployment.dry-run for a dry-run) and, for a recurring
// schedule, also schedule.write. An existing schedule with the same name but of another
// application or environment also requires schedule.write on that one, since it would be
// overwritten. A freeze override requires deployment.freeze-override.
func (g *Guard) Submit(ctx context.Context, d *descriptor.Descriptor) (*engine.Submission, error) {
	t := target{application: d.Application, environment: d.Environment, dryRun: d.DryRun, comment: d.FreezeOverride}
	act := submitAction(DeploymentSubmit, d.DryRun)
	if err := g.check(ctx, act, t); err != nil {
		return nil, err
	}
	if err := g.checkOverride(ctx, d.FreezeOverride, t); err != nil {
		return nil, err
	}
	if err := g.checkDependencies(ctx, d.After); err != nil {
		return nil, err
	}
	if d.Schedule != nil && d.Schedule.Cron != "" {
		t.schedule = d.ScheduleName()
		if err := g.check(ctx, ScheduleWrite, t); err != nil {
			return nil, err
		}
		if cur, err := g.Next.GetSchedule(ctx, t.schedule); err == nil && (cur.Application != d.Application || cur.Environment != d.Environment) {
			if err := g.check(ctx, ScheduleWrite, target{application: cur.Application, environment: cur.Environment, schedule: cur.Name}); err != nil {
				return nil, err
			}
		}
	}
	sub, err := g.Next.Submit(ctx, d)
	if sub != nil {
		t.deployment = sub.ID
	}
	g.done(ctx, act, t, err)
	return sub, err
}

// Status requires deployment.read on the deploy's application/environment.
func (g *Guard) Status(ctx context.Context, id string) (*engine.Status, error) {
	st, err := g.Next.Status(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := g.check(ctx, DeploymentRead, target{application: st.Application, environment: st.Environment, deployment: id}); err != nil {
		return nil, err
	}
	remember(ctx, id, st.Application, st.Environment)
	return st, nil
}

// List returns only the deploys the identity can read. The engine applies the filter while
// scanning, so the list still reaches the requested limit.
func (g *Guard) List(ctx context.Context, f engine.ListFilter) ([]engine.Status, error) {
	p := auth.FromContext(ctx)
	match := f.Match
	f.Match = func(st engine.Status) bool {
		return g.Policy.Allowed(p, DeploymentRead, st.Application, st.Environment) && (match == nil || match(st))
	}
	all, err := g.Next.List(ctx, f)
	if err != nil {
		return nil, err
	}
	// A remote service ignores Match: the filter is applied again.
	out := all[:0]
	for _, st := range all {
		if f.Match(st) {
			out = append(out, st)
		}
	}
	return out, nil
}

// Wait requires deployment.read.
func (g *Guard) Wait(ctx context.Context, id string) (*engine.Result, error) {
	if _, err := g.Status(ctx, id); err != nil {
		return nil, err
	}
	return g.Next.Wait(ctx, id)
}

// Cancel requires deployment.cancel.
func (g *Guard) Cancel(ctx context.Context, id string) error {
	st, err := g.Next.Status(ctx, id)
	if err != nil {
		return err
	}
	t := target{application: st.Application, environment: st.Environment, deployment: id}
	if err := g.check(ctx, DeploymentCancel, t); err != nil {
		return err
	}
	err = g.Next.Cancel(ctx, id)
	g.done(ctx, DeploymentCancel, t, err)
	return err
}

// ListSchedules returns only the schedules the identity can read.
func (g *Guard) ListSchedules(ctx context.Context) ([]engine.ScheduleInfo, error) {
	all, err := g.Next.ListSchedules(ctx)
	if err != nil {
		return nil, err
	}
	p := auth.FromContext(ctx)
	out := all[:0]
	for _, s := range all {
		if g.Policy.Allowed(p, ScheduleRead, s.Application, s.Environment) {
			out = append(out, s)
		}
	}
	return out, nil
}

// GetSchedule requires schedule.read.
func (g *Guard) GetSchedule(ctx context.Context, name string) (*engine.ScheduleInfo, error) {
	s, err := g.Next.GetSchedule(ctx, name)
	if err != nil {
		return nil, err
	}
	if err := g.check(ctx, ScheduleRead, target{application: s.Application, environment: s.Environment, schedule: name}); err != nil {
		return nil, err
	}
	return s, nil
}

// PauseSchedule requires schedule.write.
func (g *Guard) PauseSchedule(ctx context.Context, name string) error {
	return g.scheduleOp(ctx, name, "schedule.pause", g.Next.PauseSchedule)
}

// ResumeSchedule requires schedule.write.
func (g *Guard) ResumeSchedule(ctx context.Context, name string) error {
	return g.scheduleOp(ctx, name, "schedule.resume", g.Next.ResumeSchedule)
}

// DeleteSchedule requires schedule.write.
func (g *Guard) DeleteSchedule(ctx context.Context, name string) error {
	return g.scheduleOp(ctx, name, "schedule.delete", g.Next.DeleteSchedule)
}

// scheduleOp checks schedule.write on the schedule and runs the operation. In the audit the
// action names the specific operation (e.g. schedule.pause).
func (g *Guard) scheduleOp(ctx context.Context, name string, auditAction Action, op func(context.Context, string) error) error {
	s, err := g.Next.GetSchedule(ctx, name)
	if err != nil {
		return err
	}
	t := target{application: s.Application, environment: s.Environment, schedule: name}
	if err := g.check(ctx, ScheduleWrite, t); err != nil {
		return err
	}
	err = op(ctx, name)
	g.done(ctx, auditAction, t, err)
	return err
}

// Inventory returns only the applications/environments the identity can read.
func (g *Guard) Inventory(ctx context.Context, f engine.InventoryFilter) ([]engine.InventoryEntry, error) {
	all, err := g.Next.Inventory(ctx, f)
	if err != nil {
		return nil, err
	}
	p := auth.FromContext(ctx)
	out := all[:0]
	for _, ie := range all {
		if g.Policy.Allowed(p, DeploymentRead, ie.Application, ie.Environment) {
			out = append(out, ie)
		}
	}
	return out, nil
}

// Promote requires deployment.read on the source environment and deployment.submit (or
// deployment.dry-run) on the destination one.
func (g *Guard) Promote(ctx context.Context, r engine.PromoteRequest) (*engine.Submission, error) {
	if err := g.check(ctx, DeploymentRead, target{application: r.Application, environment: r.From}); err != nil {
		return nil, err
	}
	t := target{application: r.Application, environment: r.To, dryRun: r.DryRun, comment: r.FreezeOverride}
	if err := g.check(ctx, submitAction(DeploymentSubmit, r.DryRun), t); err != nil {
		return nil, err
	}
	if err := g.checkOverride(ctx, r.FreezeOverride, t); err != nil {
		return nil, err
	}
	sub, err := g.Next.Promote(ctx, r)
	if sub != nil {
		t.deployment = sub.ID
	}
	g.done(ctx, ActionPromote, t, err)
	return sub, err
}

// Rollback requires deployment.rollback (or deployment.dry-run).
func (g *Guard) Rollback(ctx context.Context, r engine.RollbackRequest) (*engine.Submission, error) {
	t := target{application: r.Application, environment: r.Environment, dryRun: r.DryRun, comment: r.FreezeOverride}
	if err := g.check(ctx, submitAction(DeploymentRollback, r.DryRun), t); err != nil {
		return nil, err
	}
	if err := g.checkOverride(ctx, r.FreezeOverride, t); err != nil {
		return nil, err
	}
	sub, err := g.Next.Rollback(ctx, r)
	if sub != nil {
		t.deployment = sub.ID
	}
	g.done(ctx, DeploymentRollback, t, err)
	return sub, err
}

// Decide requires deployment.approve on the deploy's application/environment. Whoever
// requested the deploy cannot approve it, unless a rule has allow_self.
func (g *Guard) Decide(ctx context.Context, id string, r engine.DecisionRequest) error {
	st, err := g.Next.Status(ctx, id)
	if err != nil {
		return err
	}
	act := ActionApprove
	if !r.Approve {
		act = ActionReject
	}
	t := target{application: st.Application, environment: st.Environment, deployment: id, comment: r.Comment}
	if err := g.check(ctx, DeploymentApprove, t); err != nil {
		return err
	}
	p := auth.FromContext(ctx)
	if r.Approve && st.RequestedBy == p.String() && (st.Approval == nil || !st.Approval.AllowSelf) {
		err := fmt.Errorf("%w: %s requested deploy %s and cannot approve it", engine.ErrForbidden, actor(p), id)
		g.record(ctx, act, t, audit.OutcomeDenied, err)
		return err
	}
	if a := pendingApproval(st); !g.Policy.Disabled && a != nil && !a.Approvers.Allows(p.String(), principalGroups(p)) {
		err := fmt.Errorf("%w: %s is not among the approvers of deploy %s set by the governance policy", engine.ErrForbidden, actor(p), id)
		g.record(ctx, act, t, audit.OutcomeDenied, err)
		return err
	}
	err = g.Next.Decide(ctx, id, r)
	g.done(ctx, act, t, err)
	return err
}

// pendingApproval returns the approval the deploy waits for: before the deploy or between
// two rollout waves.
func pendingApproval(st *engine.Status) *engine.ApprovalState {
	if st.Approval != nil && st.Approval.Status == engine.ApprovalPending {
		return st.Approval
	}
	if st.Rollout != nil && st.Rollout.Approval != nil {
		return st.Rollout.Approval
	}
	return st.Approval
}

func principalGroups(p *auth.Principal) []string {
	if p == nil {
		return nil
	}
	return p.Groups
}

// Freezes requires only authentication: freeze windows are not restricted.
func (g *Guard) Freezes(ctx context.Context, application, environment string) ([]governance.WindowStatus, error) {
	return g.Next.Freezes(ctx, application, environment)
}

// Specific audit actions, besides those subject to permissions.
const (
	ActionPromote Action = "deployment.promote"
	ActionApprove Action = "deployment.approve"
	ActionReject  Action = "deployment.reject"
)

// Steps requires read on the deploy's application/environment.
func (g *Guard) Steps(ctx context.Context, id string) ([]engine.StepRecord, error) {
	if _, err := g.Status(ctx, id); err != nil {
		return nil, err
	}
	return g.Next.Steps(ctx, id)
}

// Logs requires read on the deploy's application/environment.
func (g *Guard) Logs(ctx context.Context, id string, offset int) (*deploylog.Page, error) {
	if t, ok := recall(ctx, id); ok {
		if err := g.check(ctx, DeploymentRead, t); err != nil {
			return nil, err
		}
	} else if _, err := g.Status(ctx, id); err != nil {
		return nil, err
	}
	return g.Next.Logs(ctx, id, offset)
}

// DORA computes the metrics only on the deploys the identity can read.
func (g *Guard) DORA(ctx context.Context, f engine.DORAFilter) (*engine.DORAReport, error) {
	p := auth.FromContext(ctx)
	match := f.Match
	f.Match = func(st engine.Status) bool {
		return g.Policy.Allowed(p, DeploymentRead, st.Application, st.Environment) && (match == nil || match(st))
	}
	rep, err := g.Next.DORA(ctx, f)
	if err != nil {
		return nil, err
	}
	// A remote service ignores Match: unreadable environments are removed.
	envs := rep.Environments[:0]
	for _, m := range rep.Environments {
		if g.Policy.Allowed(p, DeploymentRead, m.Application, m.Environment) {
			envs = append(envs, m)
		}
	}
	rep.Environments = envs
	return rep, nil
}

// Validate requires read on the application and environment: it shows the effective
// configuration, which may include the environment's targets and parameters.
func (g *Guard) Validate(ctx context.Context, d *descriptor.Descriptor) (*engine.Validation, error) {
	if err := g.check(ctx, DeploymentRead, target{application: d.Application, environment: d.Environment}); err != nil {
		return nil, err
	}
	return g.Next.Validate(ctx, d)
}

// ValidateEnvironment requires reading the deploys of the environment: the check reveals its
// effective configuration, as Validate does.
func (g *Guard) ValidateEnvironment(ctx context.Context, c engine.EnvironmentCheck) (*engine.Validation, error) {
	if err := g.check(ctx, DeploymentRead, target{application: c.Application, environment: c.Environment}); err != nil {
		return nil, err
	}
	return g.Next.ValidateEnvironment(ctx, c)
}

// Me returns the identity and the roles that apply to it (the bindings that concern it),
// without checking permissions: everyone can read their own.
func (g *Guard) Me(ctx context.Context) (*engine.Identity, error) {
	id, err := g.Next.Me(ctx)
	if err != nil {
		return nil, err
	}
	id.AuthEnabled = !g.Policy.Disabled
	if g.Policy.Disabled {
		return id, nil
	}
	id.Grants = []engine.Grant{}
	p := auth.FromContext(ctx)
	if p == nil {
		return id, nil
	}
	for _, b := range g.Policy.Bindings {
		if !b.matches(p) {
			continue
		}
		acts := make([]string, len(g.Policy.Roles[b.Role]))
		for i, a := range g.Policy.Roles[b.Role] {
			acts[i] = string(a)
		}
		id.Grants = append(id.Grants, engine.Grant{Role: b.Role, Actions: acts, Applications: b.Applications, Environments: b.Environments})
	}
	return id, nil
}

// Approvals returns only the deploys the identity can decide: deployment.approve on the
// application/environment and, unless a rule has allow_self, not requested by it.
func (g *Guard) Approvals(ctx context.Context) ([]engine.Status, error) {
	all, err := g.Next.Approvals(ctx)
	if err != nil {
		return nil, err
	}
	p := auth.FromContext(ctx)
	out := all[:0]
	for _, st := range all {
		if !g.Policy.Allowed(p, DeploymentApprove, st.Application, st.Environment) {
			continue
		}
		if !g.Policy.Disabled && st.RequestedBy == p.String() && (st.Approval == nil || !st.Approval.AllowSelf) {
			continue
		}
		if a := pendingApproval(&st); !g.Policy.Disabled && a != nil && !a.Approvers.Allows(p.String(), principalGroups(p)) {
			continue
		}
		out = append(out, st)
	}
	return out, nil
}

// Workers requires platform.read. Only the running workflows of readable
// applications/environments stay visible: the others are only counted.
func (g *Guard) Workers(ctx context.Context) ([]engine.WorkerInfo, error) {
	p := auth.FromContext(ctx)
	if !g.Policy.AllowedAnywhere(p, PlatformRead) {
		err := fmt.Errorf("%w: %s cannot perform %s", engine.ErrForbidden, actor(p), PlatformRead)
		g.record(ctx, PlatformRead, target{}, audit.OutcomeDenied, err)
		return nil, err
	}
	ws, err := g.Next.Workers(ctx)
	if err != nil {
		return nil, err
	}
	for i := range ws {
		tasks := ws[i].Tasks[:0]
		for _, t := range ws[i].Tasks {
			if g.Policy.Allowed(p, DeploymentRead, t.Application, t.Environment) {
				tasks = append(tasks, t)
			} else {
				ws[i].HiddenTasks++
			}
		}
		ws[i].Tasks = tasks
	}
	return ws, nil
}

// Activity returns only the deploys the identity can read.
func (g *Guard) Activity(ctx context.Context, f engine.ActivityFilter) (*engine.Activity, error) {
	p := auth.FromContext(ctx)
	match := f.Match
	f.Match = func(st engine.Status) bool {
		return g.Policy.Allowed(p, DeploymentRead, st.Application, st.Environment) && (match == nil || match(st))
	}
	a, err := g.Next.Activity(ctx, f)
	if err != nil {
		return nil, err
	}
	// A remote service ignores Match: the filter is applied again.
	keep := func(in []engine.Status) []engine.Status {
		out := []engine.Status{}
		for _, st := range in {
			if f.Match(st) {
				out = append(out, st)
			}
		}
		return out
	}
	a.Running, a.AwaitingApproval, a.Queued, a.Delayed = keep(a.Running), keep(a.AwaitingApproval), keep(a.Queued), keep(a.Delayed)
	return a, nil
}

// checkDependencies checks that the identity can read the deploys a deploy depends on:
// otherwise the outcome of the wait would reveal the state of other teams' applications.
func (g *Guard) checkDependencies(ctx context.Context, ids []string) error {
	for _, id := range ids {
		if _, err := g.Status(ctx, id); err != nil && !errors.Is(err, engine.ErrNotFound) {
			return err
		}
	}
	return nil
}

// SubmitRelease requires, for every deploy of the release, the same permissions as a single
// deploy (deployment.submit, freeze override, read of external dependencies) and, with
// onFailure: rollback, deployment.rollback. Every enqueued deploy is recorded in the audit.
func (g *Guard) SubmitRelease(ctx context.Context, r *descriptor.Release) (*engine.ReleaseStatus, error) {
	for _, rd := range r.Deploys {
		d := rd.Descriptor
		if d == nil {
			continue // rejected by the engine validation
		}
		t := target{application: d.Application, environment: d.Environment, comment: d.FreezeOverride}
		if err := g.check(ctx, DeploymentSubmit, t); err != nil {
			return nil, err
		}
		if err := g.checkOverride(ctx, d.FreezeOverride, t); err != nil {
			return nil, err
		}
		if r.FailurePolicy() == descriptor.ReleaseOnFailureRollback {
			if err := g.check(ctx, DeploymentRollback, t); err != nil {
				return nil, err
			}
		}
		if err := g.checkDependencies(ctx, d.After); err != nil {
			return nil, err
		}
	}
	rs, err := g.Next.SubmitRelease(ctx, r)
	if err != nil {
		g.done(ctx, DeploymentSubmit, target{deployment: r.ID, comment: "release"}, err)
		return nil, err
	}
	for _, d := range rs.Deploys {
		g.done(ctx, DeploymentSubmit, target{application: d.Application, environment: d.Environment, deployment: d.DeploymentID, comment: "release " + rs.ID}, nil)
	}
	return rs, nil
}

// readableRelease reports whether the identity can read all the deploys of the release.
func (g *Guard) readableRelease(ctx context.Context, rs *engine.ReleaseStatus, a Action) error {
	for _, d := range rs.Deploys {
		if err := g.check(ctx, a, target{application: d.Application, environment: d.Environment, deployment: d.DeploymentID}); err != nil {
			return err
		}
	}
	return nil
}

// ReleaseStatus requires deployment.read on all the deploys of the release.
func (g *Guard) ReleaseStatus(ctx context.Context, id string) (*engine.ReleaseStatus, error) {
	rs, err := g.Next.ReleaseStatus(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := g.readableRelease(ctx, rs, DeploymentRead); err != nil {
		return nil, err
	}
	return rs, nil
}

// ListReleases returns only the releases whose deploys the identity can all read.
func (g *Guard) ListReleases(ctx context.Context, limit int) ([]engine.ReleaseStatus, error) {
	all, err := g.Next.ListReleases(ctx, limit)
	if err != nil {
		return nil, err
	}
	p := auth.FromContext(ctx)
	out := []engine.ReleaseStatus{}
	for _, rs := range all {
		if !slices.ContainsFunc(rs.Deploys, func(d engine.ReleaseDeployState) bool {
			return !g.Policy.Allowed(p, DeploymentRead, d.Application, d.Environment)
		}) {
			out = append(out, rs)
		}
	}
	return out, nil
}

// CancelRelease requires deployment.cancel on all the deploys of the release.
func (g *Guard) CancelRelease(ctx context.Context, id string) error {
	rs, err := g.Next.ReleaseStatus(ctx, id)
	if err != nil {
		return err
	}
	if err := g.readableRelease(ctx, rs, DeploymentCancel); err != nil {
		return err
	}
	err = g.Next.CancelRelease(ctx, id)
	g.done(ctx, DeploymentCancel, target{deployment: id, comment: "release"}, err)
	return err
}
