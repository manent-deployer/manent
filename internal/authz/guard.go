package authz

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/manent-deployer/manent/internal/audit"
	"github.com/manent-deployer/manent/internal/auth"
	"github.com/manent-deployer/manent/internal/deploylog"
	"github.com/manent-deployer/manent/internal/descriptor"
	"github.com/manent-deployer/manent/internal/engine"
	"github.com/manent-deployer/manent/internal/governance"
	"github.com/manent-deployer/manent/internal/pipeline"
	"github.com/manent-deployer/manent/internal/tenant"
)

// Guard wraps an engine.Service: it checks the permissions of the identity in the context and
// records in the audit the state-changing actions and the denied ones. A denied action returns
// engine.ErrForbidden.
type Guard struct {
	Next engine.Service
	// Policy is the static configuration (rbac.roles, rbac.bindings): it decides the actions
	// policy.* always, and every other action when Authorizer is nil.
	Policy Policy
	// Authorizer, if set, decides the actions other than policy.* (rbac.engine: opa).
	Authorizer Authorizer
	Audit      audit.Recorder
	Logger     *slog.Logger
	// Tenants, if set, gives the tags of the tenants for the bindings with tenants.
	Registry tenant.Directory
	// Now returns the current time, for the grants with until; replaceable in tests.
	Now func() time.Time
	// TenantRoles are the roles of the tenant members and admins on the tenant's applications
	// (rbac.tenant_roles); an empty role gives nothing.
	TenantRoles TenantRoles
	// CrossTenantInventory lets whoever belongs to a tenant read the inventory of the others
	// (rbac.cross_tenant_inventory).
	CrossTenantInventory bool
}

// TenantRoles are the roles given implicitly to members and admins of a tenant.
type TenantRoles struct {
	Members, Admins string
}

var _ engine.Service = (*Guard)(nil)

// target identifies the object of an action, for permissions and audit.
type target struct {
	tenant, application, environment, deployment, schedule string
	// artifact is the artifact of the deploy ("" for the actions on the whole environment).
	artifact string
	dryRun   bool
	// comment is the comment of an approval or the reason of a freeze override.
	comment string
	// dep describes the deploy of a change, for the Deny of the authorizer.
	dep *DeploymentInfo
}

// check verifies the permission and, for a change, the denials of the authorizer; if refused
// it records the audit entry and returns the error.
func (g *Guard) check(ctx context.Context, a Action, t target) error {
	p := auth.FromContext(ctx)
	ref := g.scope(ctx, t.tenant, t.application)
	where := t.application + "/" + t.environment
	if ref.Name != tenant.Untenanted {
		where = ref.Name + "/" + where
	}
	if t.artifact != "" {
		where += " (artifact " + t.artifact + ")"
	}
	if err := g.authzUnavailable(ctx, p, a); err != nil {
		return err
	}
	if !g.permitted(ctx, p, a, t.application, t.artifact, t.environment, ref) {
		err := fmt.Errorf("%w: %s cannot perform %s on %s", engine.ErrForbidden, actor(p), a, where)
		g.record(ctx, a, t, audit.OutcomeDenied, err)
		return err
	}
	if g.Authorizer == nil || g.Policy.Disabled || !IsWrite(a) {
		return nil
	}
	reasons, err := g.Authorizer.Deny(ctx, DenyRequest{Principal: p, Action: a, Application: t.application, Environment: t.environment, Tenant: ref, Deployment: t.dep})
	_, rev := g.Authorizer.Info()
	switch {
	case errors.Is(err, governance.ErrUnavailable):
		return fmt.Errorf("%w: permission policy unavailable: %v", engine.ErrUnavailable, err)
	case err != nil:
		g.Logger.Error("evaluation of the policy denials failed", "action", a, "error", err)
		err = fmt.Errorf("%w: %s cannot perform %s on %s: policy evaluation failed: %v (policy %s)", engine.ErrForbidden, actor(p), a, where, err, rev)
	case len(reasons) > 0:
		err = fmt.Errorf("%w: %s cannot perform %s on %s: %s (policy %s)", engine.ErrForbidden, actor(p), a, where, strings.Join(reasons, "; "), rev)
	default:
		return nil
	}
	g.record(ctx, a, t, audit.OutcomeDenied, err)
	return err
}

// grantName returns the name of the first grant that allows the action on the target, for the
// audit ("" if none: actions recorded with another action name, such as the approvals).
func (g *Guard) grantName(ctx context.Context, a Action, t target) string {
	p := auth.FromContext(ctx)
	if p == nil {
		return ""
	}
	gr, _ := GrantFor(g.allGrants(ctx, p), a, t.application, t.artifact, t.environment, g.scope(ctx, t.tenant, t.application), g.now())
	return gr.Name
}

// authzUnavailable returns ErrUnavailable when the permission policy is not loaded (or older
// than max_staleness): a temporary condition, not a denial. The static actions are unaffected.
func (g *Guard) authzUnavailable(ctx context.Context, p *auth.Principal, a Action) error {
	if g.Authorizer == nil || g.Policy.Disabled || IsStatic(a) || p == nil {
		return nil
	}
	if _, err := g.Authorizer.Grants(ctx, p); errors.Is(err, governance.ErrUnavailable) {
		return fmt.Errorf("%w: permission policy unavailable: %v", engine.ErrUnavailable, err)
	}
	return nil
}

// allowed checks a permission on a deploy of the tenant's artifact, with the tags read from the
// tenant registry. It looks only at the grants: it is for reads and lists.
func (g *Guard) allowed(ctx context.Context, p *auth.Principal, a Action, tenantName, application, artifact, environment string) bool {
	return g.permitted(ctx, p, a, application, artifact, environment, g.scope(ctx, tenantName, application))
}

// permitted reports whether the grants of the identity allow the action. policy.* are decided
// by the static configuration; without an authorizer every action is (the fast path of the
// builtin engine).
func (g *Guard) permitted(ctx context.Context, p *auth.Principal, a Action, application, artifact, environment string, t *tenant.Ref) bool {
	if g.Policy.Disabled || IsStatic(a) {
		return g.Policy.AllowedFor(p, a, application, artifact, environment, t)
	}
	if p == nil {
		return false
	}
	if g.Authorizer == nil && g.Policy.AllowedFor(p, a, application, artifact, environment, t) {
		return true
	}
	return GrantsAllow(g.allGrants(ctx, p), a, application, artifact, environment, t, g.now())
}

// allGrants are the grants of the identity (bindings or permission policy) plus the implicit
// ones of the tenants it is a member or admin of.
func (g *Guard) allGrants(ctx context.Context, p *auth.Principal) []Grant {
	return append(g.grantsOf(ctx, p), g.tenantGrants(ctx, p)...)
}

// permittedAnywhere reports whether the identity has the action on some application: it is
// for the platform actions.
func (g *Guard) permittedAnywhere(ctx context.Context, p *auth.Principal, a Action) bool {
	if g.Authorizer == nil || g.Policy.Disabled || IsStatic(a) {
		return g.Policy.AllowedAnywhere(p, a)
	}
	if p == nil {
		return false
	}
	now := g.now()
	return slices.ContainsFunc(g.grantsOf(ctx, p), func(gr Grant) bool { return gr.Anywhere(a, now) })
}

// grantsOf returns the grants of the identity; an error of the authorizer means none.
func (g *Guard) grantsOf(ctx context.Context, p *auth.Principal) []Grant {
	if g.Authorizer == nil {
		gs, _ := g.Policy.Grants(ctx, p)
		return gs
	}
	gs, err := g.Authorizer.Grants(ctx, p)
	if err != nil {
		g.Logger.Warn("permissions of the identity unavailable: everything denied", "subject", actor(p), "error", err)
		return nil
	}
	return gs
}

func (g *Guard) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// scope returns the tenant of the target with the tags of tenant and application: the named one
// (resolved deploys, inventory) or, for a request that names none, the one the engine resolves
// (tenant.Resolve). What cannot be resolved is Untenanted: the engine refuses those requests.
func (g *Guard) scope(ctx context.Context, tenantName, application string) *tenant.Ref {
	if tenantName == "" {
		if t, err := tenant.Resolve(ctx, g.Registry, application, "", true); err == nil {
			return t.Ref(application)
		}
		return tenant.UntenantedOf(application).Ref(application)
	}
	if tenantName != tenant.Untenanted && g.Registry != nil {
		if t, err := tenant.Find(ctx, g.Registry, tenantName); err == nil {
			return t.Ref(application)
		}
	}
	return &tenant.Ref{Name: tenantName, Application: application}
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
	if g.Authorizer != nil && !g.Policy.Disabled && !IsStatic(a) {
		_, e.AuthzRevision = g.Authorizer.Info()
		if outcome == audit.OutcomeAllowed {
			e.Grant = g.grantName(ctx, a, t)
		}
	}
	if rerr := g.Audit.Record(ctx, e); rerr != nil {
		g.Logger.Error("audit recording failed", "action", a, "error", rerr)
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
	if err := g.check(ctx, DeploymentRead, target{tenant: d.Tenant, application: d.Application, artifact: d.Artifact.Name, environment: d.Environment}); err != nil {
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
	t := target{tenant: d.Tenant, application: d.Application, artifact: d.Artifact.Name, environment: d.Environment, dryRun: d.DryRun, comment: d.FreezeOverride, dep: requestInfo(ctx, d, engine.KindDeploy)}
	act := submitAction(DeploymentSubmit, d.DryRun)
	if err := g.check(ctx, act, t); err != nil {
		return nil, err
	} else if err := g.checkOverride(ctx, d.FreezeOverride, t); err != nil {
		return nil, err
	}
	if err := g.checkDependencies(ctx, d.After); err != nil {
		return nil, err
	}
	if err := g.checkImports(ctx, d); err != nil {
		return nil, err
	}
	if d.Schedule != nil && d.Schedule.Cron != "" {
		t.schedule = d.ScheduleName()
		if err := g.check(ctx, ScheduleWrite, t); err != nil {
			return nil, err
		}
		if cur, err := g.Next.GetSchedule(ctx, t.schedule); err == nil && (cur.Application != d.Application || cur.Environment != d.Environment) {
			if err := g.check(ctx, ScheduleWrite, target{tenant: cur.Tenant, application: cur.Application, environment: cur.Environment, schedule: cur.Name}); err != nil {
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
	if err := g.check(ctx, DeploymentRead, target{tenant: st.Tenant, application: st.Application, artifact: st.Artifact, environment: st.Environment, deployment: id}); err != nil {
		return nil, err
	}
	remember(ctx, id, st.Application, st.Artifact, st.Environment)
	return st, nil
}

// List returns only the deploys the identity can read. The engine applies the filter while
// scanning, so the list still reaches the requested limit.
func (g *Guard) List(ctx context.Context, f engine.ListFilter) ([]engine.Status, error) {
	p := auth.FromContext(ctx)
	match := f.Match
	f.Match = func(st engine.Status) bool {
		return g.allowed(ctx, p, DeploymentRead, st.Tenant, st.Application, st.Artifact, st.Environment) && (match == nil || match(st))
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
	t := target{tenant: st.Tenant, application: st.Application, artifact: st.Artifact, environment: st.Environment, deployment: id, dep: statusInfo(st)}
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
		if g.allowed(ctx, p, ScheduleRead, s.Tenant, s.Application, "", s.Environment) {
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
	if err := g.check(ctx, ScheduleRead, target{tenant: s.Tenant, application: s.Application, environment: s.Environment, schedule: name}); err != nil {
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
	t := target{tenant: s.Tenant, application: s.Application, environment: s.Environment, schedule: name}
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
	// Across tenants only the inventory can be read, by whoever belongs to a tenant.
	cross := g.CrossTenantInventory && g.memberOfAny(ctx, p)
	out := all[:0]
	for _, ie := range all {
		if cross || g.allowed(ctx, p, DeploymentRead, ie.Tenant, ie.Application, ie.Artifact, ie.Environment) {
			out = append(out, ie)
		}
	}
	return out, nil
}

// Promote requires deployment.read on the source environment and deployment.submit (or
// deployment.dry-run) on the destination one.
func (g *Guard) Promote(ctx context.Context, r engine.PromoteRequest) (*engine.Submission, error) {
	if err := g.check(ctx, DeploymentRead, target{tenant: r.Tenant, application: r.Application, artifact: r.Artifact, environment: r.From}); err != nil {
		return nil, err
	}
	t := target{tenant: r.Tenant, application: r.Application, artifact: r.Artifact, environment: r.To, dryRun: r.DryRun, comment: r.FreezeOverride,
		dep: &DeploymentInfo{Kind: engine.KindPromotion, DryRun: r.DryRun, Version: r.Version, RequestedBy: actor(auth.FromContext(ctx))}}
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
	t := target{tenant: r.Tenant, application: r.Application, artifact: r.Artifact, environment: r.Environment, dryRun: r.DryRun, comment: r.FreezeOverride,
		dep: &DeploymentInfo{Kind: engine.KindRollback, DryRun: r.DryRun, Version: r.Version, RequestedBy: actor(auth.FromContext(ctx))}}
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
	t := target{tenant: st.Tenant, application: st.Application, artifact: st.Artifact, environment: st.Environment, deployment: id, comment: r.Comment, dep: statusInfo(st)}
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

// requestInfo describes a deploy request for Deny.
func requestInfo(ctx context.Context, d *descriptor.Descriptor, kind string) *DeploymentInfo {
	return &DeploymentInfo{
		ID: d.ID, Kind: kind, DryRun: d.DryRun, Artifact: d.Artifact.Name, Type: d.Artifact.Type,
		Version: d.Artifact.Version, RequestedBy: actor(auth.FromContext(ctx)),
	}
}

// statusInfo describes an existing deploy for Deny.
func statusInfo(st *engine.Status) *DeploymentInfo {
	return &DeploymentInfo{ID: st.ID, Kind: st.Kind, DryRun: st.DryRun, Artifact: st.Artifact, Version: st.Version, RequestedBy: st.RequestedBy}
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

// Freezes requires deployment.read somewhere: every window is shown, also those on other
// applications, because a window on "*" blocks the caller's ones too, but an identity without
// permissions (any user of the identity provider) does not read names and reasons.
func (g *Guard) Freezes(ctx context.Context, application, environment string) ([]governance.WindowStatus, error) {
	if err := g.checkPlatform(ctx, DeploymentRead, target{application: application, environment: environment}); err != nil {
		return nil, err
	}
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
		return g.allowed(ctx, p, DeploymentRead, st.Tenant, st.Application, st.Artifact, st.Environment) && (match == nil || match(st))
	}
	rep, err := g.Next.DORA(ctx, f)
	if err != nil {
		return nil, err
	}
	// A remote service ignores Match: unreadable environments are removed.
	envs := rep.Environments[:0]
	for _, m := range rep.Environments {
		if g.allowed(ctx, p, DeploymentRead, m.Tenant, m.Application, m.Artifact, m.Environment) {
			envs = append(envs, m)
		}
	}
	rep.Environments = envs
	return rep, nil
}

// Validate requires read on the application and environment: it shows the effective
// configuration, which may include the environment's targets and parameters. The artifact
// check also requires the dry-run permission: the API server contacts the artifact source
// with its credentials, as the worker does in a dry-run.
func (g *Guard) Validate(ctx context.Context, d *descriptor.Descriptor, opts engine.ValidateOptions) (*engine.Validation, error) {
	t := target{tenant: d.Tenant, application: d.Application, artifact: d.Artifact.Name, environment: d.Environment}
	if err := g.check(ctx, DeploymentRead, t); err != nil {
		return nil, err
	}
	if opts.CheckArtifact {
		if err := g.check(ctx, DeploymentDryRun, t); err != nil {
			return nil, err
		}
	}
	// The effective deploy shows the imported targets and parameters.
	if err := g.checkDependencies(ctx, d.After); err != nil {
		return nil, err
	}
	if err := g.checkImports(ctx, d); err != nil {
		return nil, err
	}
	return g.Next.Validate(ctx, d, opts)
}

// ValidateEnvironment requires reading the deploys of the environment: the check reveals its
// effective configuration, as Validate does.
func (g *Guard) ValidateEnvironment(ctx context.Context, c engine.EnvironmentCheck) (*engine.Validation, error) {
	if err := g.check(ctx, DeploymentRead, target{tenant: c.Tenant, application: c.Application, artifact: c.Artifact, environment: c.Environment}); err != nil {
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
	if g.Authorizer != nil {
		id.AuthzEngine, id.AuthzRevision = g.Authorizer.Info()
		// policy.* come from the static configuration whatever the engine.
		for _, gr := range g.staticGrants(ctx, p) {
			id.Grants = append(id.Grants, toServiceGrant(gr))
		}
	}
	now := g.now()
	for _, gr := range g.allGrants(ctx, p) {
		if gr.Until != nil && !now.Before(*gr.Until) {
			continue
		}
		id.Grants = append(id.Grants, toServiceGrant(gr))
	}
	return id, nil
}

// staticGrants are the bindings of the static configuration limited to the static actions.
func (g *Guard) staticGrants(ctx context.Context, p *auth.Principal) []Grant {
	gs, _ := g.Policy.Grants(ctx, p)
	var out []Grant
	for _, gr := range gs {
		var acts []Action
		for _, a := range gr.Actions {
			if a == All {
				acts = append(acts, StaticActions...)
			} else if IsStatic(a) {
				acts = append(acts, a)
			}
		}
		if len(acts) > 0 {
			gr.Actions, gr.Name = acts, gr.Name+" (static)"
			out = append(out, gr)
		}
	}
	return out
}

// toServiceGrant converts a grant into the format of the service.
func toServiceGrant(gr Grant) engine.Grant { return engine.ServiceGrant(gr) }

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
		if !g.allowed(ctx, p, DeploymentApprove, st.Tenant, st.Application, st.Artifact, st.Environment) {
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
	if !g.permittedAnywhere(ctx, p, PlatformRead) {
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
			if g.allowed(ctx, p, DeploymentRead, t.Tenant, t.Application, "", t.Environment) {
				tasks = append(tasks, t)
			} else {
				ws[i].HiddenTasks++
			}
		}
		ws[i].Tasks = tasks
	}
	return ws, nil
}

// Queues requires platform.read, like Workers: queues hold no application data beyond counts.
func (g *Guard) Queues(ctx context.Context) ([]engine.QueueInfo, error) {
	p := auth.FromContext(ctx)
	if !g.permittedAnywhere(ctx, p, PlatformRead) {
		err := fmt.Errorf("%w: %s cannot perform %s", engine.ErrForbidden, actor(p), PlatformRead)
		g.record(ctx, PlatformRead, target{}, audit.OutcomeDenied, err)
		return nil, err
	}
	return g.Next.Queues(ctx)
}

// Activity returns only the deploys the identity can read.
func (g *Guard) Activity(ctx context.Context, f engine.ActivityFilter) (*engine.Activity, error) {
	p := auth.FromContext(ctx)
	match := f.Match
	f.Match = func(st engine.Status) bool {
		return g.allowed(ctx, p, DeploymentRead, st.Tenant, st.Application, st.Artifact, st.Environment) && (match == nil || match(st))
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

// checkImports requires deployment.read on the releases in service the descriptor imports
// (imports with current): their exports become targets and parameters of the deploy. The
// imports of given deploys are among the dependencies, already checked by checkDependencies.
func (g *Guard) checkImports(ctx context.Context, d *descriptor.Descriptor) error {
	for _, name := range slices.Sorted(maps.Keys(d.Imports)) {
		c := d.Imports[name].Current
		if c == nil {
			continue
		}
		env := cmp.Or(c.Environment, d.Environment)
		// As the engine: the importing deploy's tenant if it lists the application.
		chosen := c.Tenant
		if chosen == "" && d.Tenant != "" && g.Registry != nil {
			if t, err := tenant.Find(ctx, g.Registry, d.Tenant); err == nil && t.Owns(c.Application) {
				chosen = d.Tenant
			}
		}
		if err := g.check(ctx, DeploymentRead, target{tenant: chosen, application: c.Application, artifact: c.Artifact, environment: env}); err != nil {
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
		t := target{tenant: d.Tenant, application: d.Application, artifact: d.Artifact.Name, environment: d.Environment, comment: d.FreezeOverride, dep: requestInfo(ctx, d, engine.KindDeploy)}
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
		if err := g.checkImports(ctx, d); err != nil {
			return nil, err
		}
	}
	rs, err := g.Next.SubmitRelease(ctx, r)
	if err != nil {
		g.done(ctx, DeploymentSubmit, target{deployment: r.ID, comment: "release"}, err)
		return nil, err
	}
	for _, d := range rs.Deploys {
		g.done(ctx, DeploymentSubmit, target{tenant: d.Tenant, application: d.Application, artifact: d.Artifact, environment: d.Environment, deployment: d.DeploymentID, comment: "release " + rs.ID}, nil)
	}
	return rs, nil
}

// readableRelease reports whether the identity can read all the deploys of the release.
func (g *Guard) readableRelease(ctx context.Context, rs *engine.ReleaseStatus, a Action) error {
	for _, d := range rs.Deploys {
		if err := g.check(ctx, a, target{tenant: d.Tenant, application: d.Application, artifact: d.Artifact, environment: d.Environment, deployment: d.DeploymentID}); err != nil {
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
			return !g.allowed(ctx, p, DeploymentRead, d.Tenant, d.Application, d.Artifact, d.Environment)
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
