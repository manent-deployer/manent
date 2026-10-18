package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/google/uuid"

	"github.com/joeg-ita/manent/internal/audit"
	"github.com/joeg-ita/manent/internal/auth"
	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/governance"
	"github.com/joeg-ita/manent/internal/labels"
	"github.com/joeg-ita/manent/internal/service"
	"github.com/joeg-ita/manent/internal/tenant"
)

// Tenant service types (contract in internal/service).
type (
	TenantService    = service.TenantService
	TenantFilter     = service.TenantFilter
	TenantInfo       = service.TenantInfo
	TenantSkip       = service.TenantSkip
	TenantsRunState  = service.TenantsRunState
	TenantGroupState = service.TenantGroupState
	TenantRunEntry   = service.TenantRunEntry
)

// KindTenants is the kind of a multi-tenant release.
const KindTenants = service.KindTenants

const (
	// tenantsWorkflowName follows a multi-tenant release. The name must not change: DBOS uses
	// it for the recovery.
	tenantsWorkflowName = "manent.tenants"
	// tenantsEvent is the key of the event with the state of the release.
	tenantsEvent = "tenants"
	// tenantsApprovalTopic is the topic of the decisions on the approvals between groups.
	tenantsApprovalTopic = "tenants-approval"
	// tenantsPollMax caps the interval between two reads of the tenant deploys: a cancelled
	// deploy does not wake the release.
	tenantsPollMax = 30 * time.Second
	// defaultGroup is the group of a release without groups.
	defaultGroup = "tenants"
)

// tenantsInput is the input of the manent.tenants workflow: the requests of the tenants are
// prepared (checked and governed) at submission, so a recovery never selects again.
type tenantsInput struct {
	Run       TenantsRunState `json:"run"`
	Requests  []Request       `json:"requests"`
	QueueName string          `json:"queue_name"`
	// Bakes are the bake durations per group; ApprovalsBetween the groups followed by an approval.
	Bakes            map[string]time.Duration `json:"bakes,omitempty"`
	ApprovalsBetween []string                 `json:"approvals_between,omitempty"`
	// Approval is the rule of the approvals between groups.
	Approval    governance.Approvals `json:"approval"`
	RequestedBy string               `json:"requested_by,omitempty"`
}

// Tenants implements TenantService.
func (e *Engine) Tenants(ctx context.Context, f TenantFilter) ([]TenantInfo, error) {
	if e.tenants == nil {
		return []TenantInfo{}, nil
	}
	all, err := e.tenants.Tenants(ctx)
	if err != nil {
		return nil, err
	}
	var sel *labels.Selector
	if f.Selector != "" {
		s, err := labels.Parse(f.Selector)
		if err != nil {
			return nil, fmt.Errorf("%w: selector: %w", ErrInvalidRequest, err)
		}
		sel = &s
	}
	out := []TenantInfo{}
	for _, t := range all {
		if f.Application != "" && t.Applications[f.Application] == nil {
			continue
		}
		if f.Application != "" && f.Environment != "" && !t.Connected(f.Application, f.Environment) {
			continue
		}
		if sel != nil && !sel.Matches(t.Labels()) {
			continue
		}
		out = append(out, tenantInfo(t))
	}
	return out, nil
}

// Tenant implements TenantService.
func (e *Engine) Tenant(ctx context.Context, name string) (*TenantInfo, error) {
	if e.tenants == nil {
		return nil, fmt.Errorf("%w: tenant %s (no tenant registry)", ErrNotFound, name)
	}
	t, err := e.tenants.Tenant(ctx, name)
	if errors.Is(err, tenant.ErrNotFound) {
		return nil, fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	if err != nil {
		return nil, err
	}
	ti := tenantInfo(*t)
	return &ti, nil
}

// tenantInfo hides the secret references of a tenant.
func tenantInfo(t tenant.Tenant) TenantInfo {
	ti := TenantInfo{Name: t.Name, Description: t.Description, Tags: t.Tags, Applications: t.Applications, Params: t.Params,
		Disabled: t.Disabled, Source: t.Source}
	for app, ss := range t.Secrets {
		if ti.Secrets == nil {
			ti.Secrets = map[string][]string{}
		}
		for _, s := range ss {
			ti.Secrets[app] = append(ti.Secrets[app], s.Name)
		}
	}
	return ti
}

// selectTenants returns the tenants of a multi-tenant descriptor.
func (e *Engine) selectTenants(ctx context.Context, application, environment string, sel *descriptor.Tenants) ([]tenant.Tenant, error) {
	if e.tenants == nil {
		return nil, fmt.Errorf("%w: tenants: no tenant registry configured (envconfig)", ErrInvalidRequest)
	}
	all, err := e.tenants.Tenants(ctx)
	if err != nil {
		return nil, err
	}
	ts, err := tenant.Select(all, application, environment, sel.Names, sel.Selector)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	if len(ts) > descriptor.MaxTenantsPerRun {
		return nil, fmt.Errorf("%w: %d tenants selected, at most %d per release", ErrInvalidRequest, len(ts), descriptor.MaxTenantsPerRun)
	}
	return ts, nil
}

// assignGroups assigns every tenant to the first group whose select matches it; a group
// without select takes the remaining ones. Without groups all tenants are in one group.
func assignGroups(ts []tenant.Tenant, sel *descriptor.Tenants) (map[string]string, []TenantGroupState, error) {
	groups := sel.Groups
	if len(groups) == 0 {
		groups = []descriptor.TenantGroup{{Name: defaultGroup}}
	}
	out := map[string]string{}
	var states []TenantGroupState
	for _, g := range groups {
		states = append(states, TenantGroupState{Name: g.Name, Bake: g.Bake, State: "pending"})
		var s labels.Selector
		if g.Select != "" {
			var err error
			if s, err = labels.Parse(g.Select); err != nil {
				return nil, nil, err
			}
		}
		for _, t := range ts {
			if _, done := out[t.Name]; done {
				continue
			}
			if g.Select == "" || s.Matches(t.Labels()) {
				out[t.Name] = g.Name
			}
		}
	}
	var left []string
	for _, t := range ts {
		if _, ok := out[t.Name]; !ok {
			left = append(left, t.Name)
		}
	}
	if len(left) > 0 {
		return nil, nil, fmt.Errorf("tenants not in any group: %s (add a last group without select)", strings.Join(left, ", "))
	}
	return out, states, nil
}

// tenantDeployID is the ID of the deploy of a tenant in a multi-tenant release.
func tenantDeployID(run, name string) string { return run + "." + name }

// submitTenants prepares a deploy per selected tenant (plan, environment and tenant
// configuration, governance) and enqueues the manent.tenants workflow that releases them. A
// tenant denied by the governance policy is left out (onDenied: skip) or refuses everything
// (reject); any other error refuses everything.
func (e *Engine) submitTenants(ctx context.Context, d *descriptor.Descriptor, meta submitMeta) (*Submission, error) {
	sel := d.Tenants
	ts, err := e.selectTenants(ctx, d.Application, d.Environment, sel)
	if err != nil {
		return nil, err
	}
	if d.Schedule != nil && d.Schedule.Cron != "" {
		// The selection is checked now and repeated at every run.
		plan, err := e.planner.Plan(d)
		if err != nil {
			return nil, err
		}
		p := auth.FromContext(ctx)
		req := Request{Descriptor: *d, Plan: plan, RequestedBy: p.String(), Kind: meta.kind}
		if p != nil {
			req.RequestedGroups = p.Groups
		}
		return e.applySchedule(req)
	}
	runID := d.ID
	if runID == "" {
		runID = uuid.NewString()
	}
	descs := make([]descriptor.Descriptor, len(ts))
	for i, t := range ts {
		dd := *d
		dd.Tenant, dd.Tenants, dd.Schedule, dd.ID = t.Name, nil, nil, tenantDeployID(runID, t.Name)
		descs[i] = dd
	}
	return e.submitRun(ctx, runID, d, descs, meta, sel)
}

// submitRun prepares the deploys of the tenants and enqueues the release; descs have one
// descriptor per tenant, already with Tenant and ID.
func (e *Engine) submitRun(ctx context.Context, runID string, template *descriptor.Descriptor, descs []descriptor.Descriptor,
	meta submitMeta, sel *descriptor.Tenants) (*Submission, error) {
	var ts []tenant.Tenant
	for _, dd := range descs {
		t := tenant.Tenant{Name: dd.Tenant}
		if e.tenants != nil {
			if full, err := e.tenants.Tenant(ctx, dd.Tenant); err == nil {
				t = *full
			}
		}
		ts = append(ts, t)
	}
	assigned, groups, err := assignGroups(ts, sel)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	// The ID was validated with the descriptor (or is a schedule run "<schedule>@<time>"): only
	// the length of the tenant deploys' IDs is left to check.
	if len(tenantDeployID(runID, strings.Repeat("x", 40))) > 128 {
		return nil, fmt.Errorf("%w: id %q too long for the tenant deploys (<id>.<tenant>, at most 128)", ErrInvalidRequest, runID)
	}
	run := TenantsRunState{
		ID: runID, Application: template.Application, Environment: template.Environment, Artifact: template.Artifact.Name,
		Version: template.Artifact.Version, Kind: meta.kind, Status: service.TenantsRunning, MaxParallel: sel.Parallel(),
		OnFailure: sel.OnFailureOrDefault(), Groups: groups,
	}
	var reqs []Request
	sub := &Submission{ID: runID, State: string(dbos.WorkflowStatusEnqueued)}
	rule := governance.Approvals{Required: 1, Timeout: governance.DefaultApprovalTimeout}
	runMeta := meta
	runMeta.run = runID
	for i := range descs {
		dd := &descs[i]
		entry := TenantRunEntry{Tenant: dd.Tenant, Group: assigned[dd.Tenant], DeploymentID: dd.ID, State: service.TenantPending}
		// The ID is the release's plus the tenant (checked above): the rest of the descriptor is
		// validated as any other.
		check := *dd
		check.ID = ""
		if err := check.Validate(); err != nil {
			return nil, fmt.Errorf("%w: tenant %s: %w", ErrInvalidRequest, dd.Tenant, err)
		}
		r, err := e.prepare(ctx, dd, runMeta)
		if err != nil {
			if denied(err) && sel.OnDeniedOrDefault() == descriptor.TenantsOnDeniedSkip {
				entry.State, entry.DeploymentID, entry.Reason = service.TenantDenied, "", err.Error()
				run.Tenants = append(run.Tenants, entry)
				sub.Skipped = append(sub.Skipped, TenantSkip{Tenant: dd.Tenant, Reason: err.Error()})
				continue
			}
			return nil, fmt.Errorf("tenant %s: %w", dd.Tenant, err)
		}
		if a := r.Approval; a != nil && a.Required > rule.Required {
			rule = *a
		}
		reqs = append(reqs, *r)
		run.Tenants = append(run.Tenants, entry)
		sub.Tenants = append(sub.Tenants, dd.Tenant)
	}
	if len(reqs) == 0 {
		return nil, fmt.Errorf("%w: no tenant to release: all were denied by the governance policy", ErrInvalidRequest)
	}
	in := tenantsInput{Run: run, Requests: reqs, QueueName: e.queueName, Approval: rule, RequestedBy: auth.FromContext(ctx).String(),
		Bakes: map[string]time.Duration{}}
	for _, g := range sel.Groups {
		in.Bakes[g.Name] = g.BakeDuration()
	}
	in.ApprovalsBetween = sel.ApprovalsBetween
	attrs := attributes(Request{Descriptor: *template, Kind: KindTenants})
	attrs[attrDryRun] = nil
	delete(attrs, attrDryRun)
	opts := []dbos.EnqueueOption{dbos.WithEnqueueWorkflowID(runID), dbos.WithEnqueueAttributes(attrs)}
	if in.RequestedBy != "" {
		opts = append(opts, dbos.WithEnqueueAuthenticatedUser(in.RequestedBy))
	}
	if s := template.Schedule; s != nil && s.At != nil {
		delay := s.At.Sub(e.now())
		if delay <= 0 {
			return nil, fmt.Errorf("%w: schedule.at %s is in the past", ErrInvalidRequest, s.At.Format(time.RFC3339))
		}
		opts = append(opts, dbos.WithEnqueueDelay(delay))
		sub.State, sub.ScheduledAt = string(dbos.WorkflowStatusDelayed), s.At
	}
	if _, err := dbos.Enqueue[TenantsRunState](e.client, approvalQueue(e.queueName), tenantsWorkflowName, in, opts...); err != nil {
		return nil, err
	}
	e.logger.Info("rilascio multi-tenant accodato", "run", runID, "application", template.Application, "environment", template.Environment,
		"tenants", len(reqs), "skipped", len(sub.Skipped), "kind", meta.kind, "requested_by", in.RequestedBy)
	return sub, nil
}

// denied reports a refusal of the governance (policy, freeze, mandatory promotion), which
// onDenied: skip turns into a skipped tenant.
func denied(err error) bool {
	if !errors.Is(err, ErrInvalidRequest) {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "denied by the governance policy") || strings.Contains(msg, "blocked by") ||
		strings.Contains(msg, "(governance)")
}

// promoteTenants promotes every tenant matching the selector from its own release in r.From;
// tenants without a release there are left out.
func (e *Engine) promoteTenants(ctx context.Context, r PromoteRequest) (*Submission, error) {
	sel := &descriptor.Tenants{Selector: r.Tenants}
	ts, err := e.selectTenants(ctx, r.Application, r.To, sel)
	if err != nil {
		return nil, err
	}
	runID := cmp.Or(r.ID, uuid.NewString())
	var descs []descriptor.Descriptor
	var skipped []TenantSkip
	var template *descriptor.Descriptor
	for _, t := range ts {
		rr := r
		rr.Tenant, rr.Tenants, rr.ID = t.Name, "", tenantDeployID(runID, t.Name)
		d, _, err := e.promoteDescriptor(ctx, rr)
		if err != nil {
			skipped = append(skipped, TenantSkip{Tenant: t.Name, Reason: err.Error()})
			continue
		}
		if template == nil {
			template = d
		}
		descs = append(descs, *d)
	}
	if len(descs) == 0 {
		return nil, fmt.Errorf("%w: no tenant matching %q has a release to promote in %s", ErrInvalidRequest, r.Tenants, r.From)
	}
	tmpl := *template
	tmpl.Tenant = ""
	sub, err := e.submitRun(ctx, runID, &tmpl, descs, submitMeta{kind: KindPromotion}, sel)
	if sub != nil {
		sub.Skipped = append(sub.Skipped, skipped...)
	}
	return sub, err
}

// rollbackRun rolls back, in reverse order, the tenants that succeeded in multi-tenant release
// r.Run, each to the release before it, as a new multi-tenant release <run>.rollback that goes
// on if one fails.
func (e *Engine) rollbackRun(ctx context.Context, r RollbackRequest) (*Submission, error) {
	st, err := e.Status(ctx, r.Run)
	if err != nil {
		return nil, err
	}
	if st.TenantsRun == nil {
		return nil, fmt.Errorf("%w: %s is not a multi-tenant release", ErrInvalidRequest, r.Run)
	}
	runID := cmp.Or(r.ID, r.Run+".rollback")
	var descs []descriptor.Descriptor
	var template *descriptor.Descriptor
	tenants := slices.Clone(st.TenantsRun.Tenants)
	slices.Reverse(tenants)
	var names []string
	var skipped []TenantSkip
	for _, t := range tenants {
		if t.State != StateSuccess {
			continue
		}
		rr := RollbackRequest{Application: st.Application, Environment: st.Environment, Tenant: t.Tenant, Version: r.Version, Redeploy: r.Redeploy}
		rr.ID = tenantDeployID(runID, t.Tenant)
		d, _, err := e.rollbackDescriptor(ctx, rr)
		if err != nil {
			// E.g. the first release of the tenant: there is nothing to go back to.
			skipped = append(skipped, TenantSkip{Tenant: t.Tenant, Reason: err.Error()})
			continue
		}
		if template == nil {
			template = d
		}
		descs = append(descs, *d)
		names = append(names, t.Tenant)
	}
	if len(descs) == 0 {
		return nil, fmt.Errorf("%w: no tenant succeeded in %s", ErrInvalidRequest, r.Run)
	}
	tmpl := *template
	tmpl.Tenant = ""
	sel := &descriptor.Tenants{Names: names, MaxParallel: st.TenantsRun.MaxParallel, OnFailure: descriptor.TenantsOnFailureContinue}
	sub, err := e.submitRun(ctx, runID, &tmpl, descs, submitMeta{kind: KindRollback, source: r.Run}, sel)
	if sub != nil {
		sub.Skipped = append(sub.Skipped, skipped...)
	}
	return sub, err
}

// runStatus is the status of a multi-tenant release.
func (e *Engine) runStatus(w dbos.WorkflowStatus) *Status {
	s := toStatus(w)
	s.Kind = KindTenants
	var in tenantsInput
	run := TenantsRunState{ID: w.ID}
	if ev, err := dbos.GetEvent[TenantsRunState](e.client, w.ID, tenantsEvent, 0); err == nil && ev.ID != "" {
		run = ev
	} else if decodeInput(w.Input, &in) == nil {
		run = in.Run
	}
	if w.Status == dbos.WorkflowStatusSuccess {
		var out TenantsRunState
		if decodeInput(w.Output, &out) == nil && out.ID != "" {
			run = out
		}
		if run.Status == service.TenantsFailed {
			s.State, s.Error = StateError, run.Error
		}
	}
	if run.Status == service.TenantsPausedApproval && w.Status == dbos.WorkflowStatusPending {
		s.State, s.Approval = StateAwaitingApproval, run.Approval
	}
	s.TenantsRun = &run
	return &s
}

// runStatusWithInput is runStatus reading the input too, needed before the first event.
func (e *Engine) runStatusWithInput(w dbos.WorkflowStatus) (*Status, error) {
	if ws, err := dbos.ListWorkflows(e.client, dbos.WithFilterWorkflowIDs(w.ID), dbos.WithFilterLoadInput(true), dbos.WithFilterLoadOutput(true)); err == nil && len(ws) == 1 {
		w = ws[0]
	}
	return e.runStatus(w), nil
}

// cancelRun cancels the release and its tenant deploys not yet ended.
func (e *Engine) cancelRun(ctx context.Context, st *Status) error {
	if err := dbos.CancelWorkflow(e.client, st.ID); err != nil {
		return err
	}
	var ids []string
	for _, t := range st.TenantsRun.Tenants {
		if t.DeploymentID != "" {
			ids = append(ids, t.DeploymentID)
		}
	}
	states, err := e.states(ids)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if s, ok := states[id]; ok && !Terminal(s.State) {
			if err := e.Cancel(ctx, id); err != nil && !errors.Is(err, ErrNotFound) {
				return fmt.Errorf("deploy %s: %w", id, err)
			}
		}
	}
	e.logger.Info("rilascio multi-tenant annullato", "run", st.ID)
	return nil
}

// waitRun waits for the end of a multi-tenant release.
func (e *Engine) waitRun(id string, opts ...dbos.GetResultOption) (*Result, error) {
	h, err := dbos.RetrieveWorkflow[TenantsRunState](e.client, id)
	if err != nil {
		return nil, err
	}
	run, err := h.GetResult(opts...)
	if err != nil {
		return nil, err
	}
	if run.Status != service.TenantsSucceeded {
		return nil, fmt.Errorf("multi-tenant release %s %s: %s", id, run.Status, run.Error)
	}
	return &Result{DeploymentID: id}, nil
}

// notifyRun wakes the multi-tenant release of an ended tenant deploy.
func (w *Worker) notifyRun(ctx dbos.Context, run, id string) {
	if run == "" {
		return
	}
	if err := dbos.Send(ctx, run, id, chainTopic); err != nil {
		w.logger.Warn("avviso al rilascio multi-tenant fallito", "run", run, "deployment", id, "error", err)
	}
}

// runCheck is a read of the tenant deploys of a release, persisted as a step.
type runCheck struct {
	Tenants []TenantRunEntry `json:"tenants"`
	Now     time.Time        `json:"now"`
}

// readRun reads the state of the started tenant deploys with a single query.
func (w *Worker) readRun(ctx context.Context, entries []TenantRunEntry) runCheck {
	c := runCheck{Tenants: slices.Clone(entries), Now: time.Now().UTC()}
	var ids []string
	for _, t := range entries {
		if t.DeploymentID != "" && t.State != service.TenantPending && !Terminal(t.State) {
			ids = append(ids, t.DeploymentID)
		}
	}
	if len(ids) == 0 {
		return c
	}
	states, err := w.reader.states(ids)
	if err != nil {
		w.logger.Warn("rilascio multi-tenant: lettura dello stato dei deploy fallita", "error", err)
		return c
	}
	for i := range c.Tenants {
		t := &c.Tenants[i]
		st, ok := states[t.DeploymentID]
		if !ok || t.State == service.TenantPending {
			continue
		}
		if Terminal(st.State) && st.State != StateSuccess {
			if full, err := w.reader.Status(ctx, t.DeploymentID); err == nil {
				st.Error = full.Error
			}
		}
		t.State, t.Error = st.State, st.Error
	}
	return c
}

// enqueueTenants enqueues the deploys of the given tenants (approval first if required). It is
// a step: after a crash it is repeated, and the fixed IDs make the enqueues idempotent.
func (w *Worker) enqueueTenants(in tenantsInput, ids []string) ([]string, error) {
	byID := map[string]Request{}
	for _, r := range in.Requests {
		byID[r.Descriptor.ID] = r
	}
	var started []string
	for _, id := range ids {
		req, ok := byID[id]
		if !ok {
			return started, fmt.Errorf("request of %s missing", id)
		}
		var err error
		if needsApproval(req) {
			err = enqueueApproval(w.dbos, in.QueueName, req, id)
		} else {
			_, err = dbos.Enqueue[Result](w.dbos, in.QueueName, workflowName, req, deployOptions(req, id)...)
		}
		if err != nil {
			return started, err
		}
		started = append(started, id)
	}
	return started, nil
}

// tenantsWorkflow releases the tenants group by group: at most MaxParallel deploys at a time,
// woken by the deploys that end (notifyRun) or at growing intervals. With onFailure stop a
// failed tenant stops the start of the others; after a group come its bake and, if asked,
// an approval. The state is published as a DBOS event.
func (w *Worker) tenantsWorkflow(ctx dbos.Context, in tenantsInput) (TenantsRunState, error) {
	rs := in.Run
	log := w.logger.With("run", rs.ID)
	stopped := ""
	publish := func() error { return dbos.SetEvent(ctx, tenantsEvent, rs) }
	for gi := range rs.Groups {
		g := &rs.Groups[gi]
		if stopped != "" {
			g.State = "skipped"
			continue
		}
		rs.Group, g.State, rs.Status = g.Name, "running", service.TenantsRunning
		if err := publish(); err != nil {
			return rs, err
		}
		n := 0
		for poll := w.chainPoll; ; poll = min(poll*2, tenantsPollMax) {
			check, err := dbos.RunAsStep(ctx, func(c context.Context) (runCheck, error) {
				return w.readRun(c, rs.Tenants), nil
			}, dbos.WithStepName(fmt.Sprintf("tenants@%s#%d", g.Name, n)))
			if err != nil {
				return rs, err
			}
			changed := !slices.Equal(rs.Tenants, check.Tenants)
			rs.Tenants = check.Tenants
			if stopped == "" && rs.OnFailure != descriptor.TenantsOnFailureContinue {
				if i := slices.IndexFunc(rs.Tenants, failedTenant); i >= 0 {
					stopped = "tenant " + rs.Tenants[i].Tenant + " failed"
					log.Warn("rilascio multi-tenant: un tenant non è riuscito, i tenant non ancora partiti non partono", "tenant", rs.Tenants[i].Tenant)
				}
			}
			running, next := 0, []string{}
			for _, t := range rs.Tenants {
				switch {
				case t.Group != g.Name:
				case t.State == service.TenantPending:
					next = append(next, t.DeploymentID)
				case !Terminal(t.State) && t.State != service.TenantSkipped && t.State != service.TenantDenied:
					running++
				}
			}
			if stopped == "" && len(next) > 0 && running < rs.MaxParallel {
				start := next[:min(len(next), rs.MaxParallel-running)]
				started, err := dbos.RunAsStep(ctx, func(context.Context) ([]string, error) {
					return w.enqueueTenants(in, start)
				}, dbos.WithStepName(fmt.Sprintf("enqueue@%s#%d", g.Name, n)))
				if err != nil {
					return rs, err
				}
				for i := range rs.Tenants {
					if slices.Contains(started, rs.Tenants[i].DeploymentID) {
						rs.Tenants[i].State = StateEnqueued
					}
				}
				changed = true
				running += len(started)
				next = next[len(started):]
			}
			if stopped != "" && len(next) > 0 {
				markSkipped(&rs, g.Name, stopped)
				next, changed = nil, true
			}
			n++
			if running == 0 && len(next) == 0 {
				break
			}
			if changed {
				if err := publish(); err != nil {
					return rs, err
				}
			}
			if err := waitChain(ctx, poll); err != nil {
				return rs, err
			}
		}
		if stopped != "" || gi == len(rs.Groups)-1 {
			g.State = "done"
			continue
		}
		if bake := in.Bakes[g.Name]; bake > 0 {
			g.State, rs.Status = "baking", service.TenantsBaking
			if err := publish(); err != nil {
				return rs, err
			}
			if _, err := dbos.Sleep(ctx, bake); err != nil {
				return rs, err
			}
		}
		if slices.Contains(in.ApprovalsBetween, g.Name) {
			if status, err := w.waitRunApproval(ctx, in, &rs, g.Name); err != nil {
				return rs, err
			} else if status != ApprovalApproved {
				stopped = "approval after group " + g.Name + " " + status
			}
		}
		g.State = "done"
	}
	for gi := range rs.Groups {
		if stopped != "" && rs.Groups[gi].State == "pending" {
			rs.Groups[gi].State = "skipped"
		}
	}
	if stopped != "" {
		markSkipped(&rs, "", stopped)
	}
	succeeded, failed, _ := rs.Counts()
	switch {
	case failed > 0:
		rs.Status = service.TenantsFailed
		var names []string
		for _, t := range rs.Tenants {
			if failedTenant(t) {
				names = append(names, t.Tenant)
			}
		}
		rs.Error = fmt.Sprintf("%d tenants failed: %s", failed, strings.Join(names, ", "))
	case stopped != "":
		rs.Status, rs.Error = service.TenantsStopped, stopped
	default:
		rs.Status = service.TenantsSucceeded
	}
	rs.Group, rs.Paused, rs.Approval = "", "", nil
	now, err := dbos.RunAsStep(ctx, func(context.Context) (time.Time, error) { return time.Now().UTC(), nil }, dbos.WithStepName("now"))
	if err != nil {
		return rs, err
	}
	rs.UpdatedAt = now
	if err := publish(); err != nil {
		return rs, err
	}
	log.Info("rilascio multi-tenant concluso", "status", rs.Status, "succeeded", succeeded, "failed", failed)
	w.recordRun(ctx, in, rs)
	return rs, nil
}

// failedTenant reports a tenant whose deploy ended without success.
func failedTenant(t TenantRunEntry) bool {
	return t.State != StateSuccess && t.State != service.TenantSkipped && t.State != service.TenantDenied &&
		t.State != service.TenantPending && Terminal(t.State)
}

// markSkipped marks as skipped the tenants not yet started (of the group, or all with group "").
func markSkipped(rs *TenantsRunState, group, reason string) {
	for i := range rs.Tenants {
		t := &rs.Tenants[i]
		if t.State == service.TenantPending && (group == "" || t.Group == group) {
			t.State, t.Reason = service.TenantSkipped, reason
		}
	}
}

// waitRunApproval waits for the approval asked after a group: the rule is the strictest of the
// tenants (at least one approval by another identity).
func (w *Worker) waitRunApproval(ctx dbos.Context, in tenantsInput, rs *TenantsRunState, group string) (string, error) {
	rule := in.Approval
	rule.Required = max(1, rule.Required)
	if rule.Timeout <= 0 {
		rule.Timeout = governance.DefaultApprovalTimeout
	}
	t, err := now(ctx)
	if err != nil {
		return "", err
	}
	st := ApprovalState{Status: ApprovalPending, Required: rule.Required, AllowSelf: rule.AllowSelf, Approvers: rule.Approvers, ExpiresAt: t.Add(rule.Timeout)}
	rs.Status, rs.Paused, rs.Approval = service.TenantsPausedApproval, "approvazione dopo il gruppo "+group, &st
	for st.Status == ApprovalPending {
		if err := dbos.SetEvent(ctx, tenantsEvent, *rs); err != nil {
			return "", err
		}
		t, err := now(ctx)
		if err != nil {
			return "", err
		}
		dec, err := dbos.Recv[Decision](ctx, tenantsApprovalTopic, max(st.ExpiresAt.Sub(t), time.Millisecond))
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			st.Status = ApprovalExpired
			continue
		case err != nil:
			return "", err
		}
		switch {
		case dec.Approver == "":
			continue
		case !rule.AllowSelf && dec.Approver == in.RequestedBy:
			w.logger.Warn("decisione scartata: auto-approvazione non ammessa", "run", rs.ID, "approver", dec.Approver)
			continue
		case slices.ContainsFunc(st.Decisions, func(x Decision) bool { return x.Approver == dec.Approver }):
			continue
		}
		st.Decisions = append(st.Decisions, dec)
		switch {
		case !dec.Approve:
			st.Status = ApprovalRejected
		case len(st.Decisions) >= rule.Required:
			st.Status = ApprovalApproved
		}
	}
	rs.Status, rs.Paused, rs.Approval = service.TenantsRunning, "", nil
	w.logger.Info("approvazione tra i gruppi conclusa", "run", rs.ID, "group", group, "status", st.Status)
	return st.Status, nil
}

// recordRun records the outcome of the release in the audit, in a step.
func (w *Worker) recordRun(ctx dbos.Context, in tenantsInput, rs TenantsRunState) {
	if w.audit == nil {
		return
	}
	succeeded, failed, _ := rs.Counts()
	_, _ = dbos.RunAsStep(ctx, func(c context.Context) (string, error) {
		e := audit.Entry{
			Actor: cmp.Or(in.RequestedBy, "unknown"), Action: "tenants.finish", Outcome: rs.Status,
			Application: rs.Application, Environment: rs.Environment, Deployment: rs.ID,
			Comment: fmt.Sprintf("%d succeeded, %d failed", succeeded, failed),
		}
		if err := w.audit.Record(c, e); err != nil {
			w.logger.Error("registrazione nell'audit fallita", "run", rs.ID, "error", err)
		}
		return "", nil
	}, dbos.WithStepName("audit"))
}
