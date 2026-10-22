package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/joeg-ita/manent/internal/audit"
	"github.com/joeg-ita/manent/internal/auth"
	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/service"
)

const (
	// releaseWorkflowName follows a composite release. The name must not change: DBOS uses it
	// for recovery.
	releaseWorkflowName = "manent.release"
	// releaseEvent is the key of the event with the release state.
	releaseEvent = "release"
	// releaseRollbackInfix builds the ID of a release's rollbacks: "<release>.rollback.<name>".
	releaseRollbackInfix = ".rollback."
)

// releaseInput is the input of the release workflow: the initial state, with the deploys in
// topological order.
type releaseInput struct {
	Release ReleaseStatus `json:"release"`
	// Descriptors are the deploy descriptors, in the order of Release.Deploys, with IDs and
	// dependencies (after) already assigned: the workflow enqueues them in its first step.
	// Releases enqueued by earlier versions lack them, because their deploys were already enqueued.
	Descriptors []descriptor.Descriptor `json:"descriptors,omitempty"`
}

// SubmitRelease implements ReleaseService. It checks plan, policy, governance and external
// dependencies of all deploys, then enqueues the release workflow, which in turn enqueues the
// deploys in topological order, each with dependencies (after) on its predecessors. So a
// process crash after enqueuing leaves no deploy without the release following it. With the
// same ID it returns the existing release.
func (e *Engine) SubmitRelease(ctx context.Context, r *descriptor.Release) (*ReleaseStatus, error) {
	if err := r.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	if rs, err := e.ReleaseStatus(ctx, r.ID); err == nil {
		return rs, nil
	}
	order, err := r.Order()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	ids := map[string]string{}
	for _, rd := range r.Deploys {
		ids[rd.Name] = r.DeploymentID(rd)
	}
	rs := &ReleaseStatus{
		ID: r.ID, State: string(dbos.WorkflowStatusPending), OnFailure: r.FailurePolicy(),
		RequestedBy: auth.FromContext(ctx).String(), CreatedAt: e.now().UTC(),
	}
	descs := make([]descriptor.Descriptor, len(order))
	waitsFor := make([]string, 0, 2*len(order))
	for i, rd := range order {
		d := *rd.Descriptor
		d.ID = ids[rd.Name]
		owner, err := e.owner(ctx, d.Application, d.Tenant)
		if err != nil {
			return nil, fmt.Errorf("%w: deploy %s: %w", ErrInvalidRequest, rd.Name, err)
		}
		d.Tenant = owner.Name
		after := make([]string, 0, len(rd.After)+len(d.After))
		for _, a := range rd.After {
			after = append(after, ids[a])
		}
		external := d.After
		d.After = append(after, d.After...)
		d.Imports = releaseImports(d.Imports, ids)
		if err := d.Validate(); err != nil {
			return nil, fmt.Errorf("%w: deploy %s: %w", ErrInvalidRequest, rd.Name, err)
		}
		if _, err := e.planner.Plan(&d); err != nil {
			return nil, fmt.Errorf("%w: deploy %s: %w", ErrInvalidRequest, rd.Name, err)
		}
		eff, err := e.check(ctx, &d)
		if err != nil {
			return nil, fmt.Errorf("deploy %s: %w", rd.Name, err)
		}
		// Freezes and governance rules reject the release at once, not after enqueuing.
		if err := e.govern(ctx, &Request{Descriptor: d, Kind: KindDeploy, Release: r.ID, RequestedBy: auth.FromContext(ctx).String()}, eff); err != nil {
			return nil, fmt.Errorf("deploy %s: %w", rd.Name, err)
		}
		// Internal dependencies do not exist yet: only external ones are checked.
		if len(external) > 0 {
			ext := d
			ext.After = external
			if _, err := e.checkAfter(ctx, &ext); err != nil {
				return nil, fmt.Errorf("deploy %s: %w", rd.Name, err)
			}
		}
		if i > 0 && d.Tenant != rs.Deploys[0].Tenant {
			return nil, fmt.Errorf("%w: deploy %s belongs to tenant %q, deploy %s to %q: a release contains the applications of one tenant",
				ErrInvalidRequest, rd.Name, d.Tenant, rs.Deploys[0].Name, rs.Deploys[0].Tenant)
		}
		descs[i] = d
		rs.Deploys = append(rs.Deploys, ReleaseDeployState{
			Name: rd.Name, DeploymentID: d.ID, Application: d.Application, Artifact: d.Artifact.Name, Environment: d.Environment, Tenant: d.Tenant,
			Version: d.Artifact.Version, After: rd.After, State: StateEnqueued,
		})
		waitsFor = append(waitsFor, d.ID)
		if rs.OnFailure == descriptor.ReleaseOnFailureRollback {
			waitsFor = append(waitsFor, r.ID+releaseRollbackInfix+rd.Name)
		}
	}
	opts := []dbos.EnqueueOption{
		dbos.WithEnqueueWorkflowID(r.ID),
		dbos.WithEnqueueAttributes(map[string]any{attrRelease: r.ID, attrWaitsFor: strings.Join(waitsFor, " ")}),
	}
	if rs.RequestedBy != "" {
		opts = append(opts, dbos.WithEnqueueAuthenticatedUser(rs.RequestedBy))
	}
	if _, err := dbos.Enqueue[ReleaseStatus](e.client, approvalQueue(e.queueName), releaseWorkflowName, releaseInput{Release: *rs, Descriptors: descs}, opts...); err != nil {
		return nil, err
	}
	e.logger.Info("release accodata", "release", r.ID, "deploys", len(rs.Deploys), "on_failure", rs.OnFailure, "requested_by", rs.RequestedBy)
	return rs, nil
}

// submitDeploys enqueues the release deploys, in order, with the identity of whoever requested
// it. It is a step: after a crash it is repeated, and the fixed IDs make the enqueues already
// done idempotent. If a deploy is rejected (e.g. a freeze started after submission) or the
// release is cancelled, it cancels those already enqueued: the rejected one ends ERROR, those
// not enqueued CANCELLED.
func (w *Worker) submitDeploys(ctx context.Context, rs ReleaseStatus, descs []descriptor.Descriptor) []ReleaseDeployState {
	out := slices.Clone(rs.Deploys)
	eng, ok := w.service.(*Engine)
	method, subject, _ := strings.Cut(rs.RequestedBy, ":")
	pc := auth.WithPrincipal(ctx, &auth.Principal{Method: method, Subject: subject})
	var submitted []string
	for i := range descs {
		var err error
		switch {
		case !ok:
			err = errors.New("service not configured on the worker")
		case ctx.Err() != nil || w.cancelled(rs.ID):
			err = errors.New("release cancelled")
		default:
			var sub *Submission
			if sub, err = eng.submit(pc, &descs[i], submitMeta{kind: KindDeploy, release: rs.ID, prechecked: true}); err == nil {
				out[i].State = sub.State
				submitted = append(submitted, sub.ID)
				continue
			}
		}
		w.logger.Error("release: deploy non accodato, la release si ferma", "release", rs.ID, "deploy", out[i].Name, "error", err)
		out[i].State, out[i].Error = string(dbos.WorkflowStatusError), "deploy not enqueued: "+err.Error()
		for j := i + 1; j < len(out); j++ {
			out[j].State = string(dbos.WorkflowStatusCancelled)
		}
		for _, id := range submitted {
			if err := eng.Cancel(context.WithoutCancel(pc), id); err != nil && !errors.Is(err, ErrNotFound) {
				w.logger.Warn("release: annullamento di un deploy già accodato fallito", "release", rs.ID, "deployment", id, "error", err)
			}
		}
		break
	}
	return out
}

// releaseCheck is a read of the state of the release deploys, persisted as a step.
type releaseCheck struct {
	Deploys []ReleaseDeployState `json:"deploys"`
	// Pending are the deploys not yet started, which stop and rollback cancel.
	Pending []string `json:"pending,omitempty"`
}

// releaseWorkflow follows the release deploys until they end. Deploys start by themselves
// (each waits for its predecessors); the workflow applies onFailure: with stop and rollback
// it cancels those not yet started, with rollback it then brings the successful deploys back
// to the previous version, from the most dependent to the least.
func (w *Worker) releaseWorkflow(ctx dbos.Context, in releaseInput) (ReleaseStatus, error) {
	rs := in.Release
	log := w.logger.With("release", rs.ID)
	if len(in.Descriptors) > 0 {
		deploys, err := dbos.RunAsStep(ctx, func(c context.Context) ([]ReleaseDeployState, error) {
			return w.submitDeploys(c, rs, in.Descriptors), nil
		}, dbos.WithStepName("submit"))
		if err != nil {
			return rs, err
		}
		rs.Deploys = deploys
	}
	stopped := false
	var published []ReleaseDeployState
	for poll := w.chainPoll; ; poll = min(poll*2, chainPollMax) {
		check, err := dbos.RunAsStep(ctx, func(c context.Context) (releaseCheck, error) {
			return w.readRelease(c, rs.Deploys), nil
		}, dbos.WithStepName("deploys"))
		if err != nil {
			return rs, err
		}
		rs.Deploys = check.Deploys
		failed := releaseFailed(rs.Deploys)
		if failed && rs.OnFailure != descriptor.ReleaseOnFailureContinue && !stopped && len(check.Pending) > 0 {
			log.Warn("release: un deploy non è riuscito, i deploy non ancora partiti vengono annullati", "deploys", check.Pending)
			if _, err := dbos.RunAsStep(ctx, func(c context.Context) (string, error) {
				for _, id := range check.Pending {
					if err := w.reader.Cancel(c, id); err != nil && !errors.Is(err, ErrNotFound) {
						log.Warn("release: annullamento fallito", "deployment", id, "error", err)
					}
				}
				return "", nil
			}, dbos.WithStepName("stop")); err != nil {
				return rs, err
			}
			stopped = true
			continue
		}
		if !slices.ContainsFunc(rs.Deploys, func(d ReleaseDeployState) bool { return !Terminal(d.State) }) {
			break
		}
		if !slices.EqualFunc(published, rs.Deploys, sameDeployState) {
			if err := dbos.SetEvent(ctx, releaseEvent, rs); err != nil {
				return rs, err
			}
			published = slices.Clone(rs.Deploys)
		}
		if err := waitChain(ctx, poll); err != nil {
			return rs, err
		}
	}
	if releaseFailed(rs.Deploys) && rs.OnFailure == descriptor.ReleaseOnFailureRollback {
		if err := w.rollbackRelease(ctx, &rs); err != nil {
			return rs, err
		}
	}
	rs.State, rs.Error = string(dbos.WorkflowStatusSuccess), ""
	if releaseFailed(rs.Deploys) {
		rs.State, rs.Error = string(dbos.WorkflowStatusError), releaseError(rs.Deploys)
	}
	now, err := dbos.RunAsStep(ctx, func(context.Context) (time.Time, error) { return time.Now().UTC(), nil }, dbos.WithStepName("now"))
	if err != nil {
		return rs, err
	}
	rs.UpdatedAt = now
	if err := dbos.SetEvent(ctx, releaseEvent, rs); err != nil {
		return rs, err
	}
	log.Info("release conclusa", "state", rs.State, "error", rs.Error)
	w.recordRelease(ctx, rs)
	return rs, nil
}

// readRelease reads the state of the release deploys with a single query. A read error leaves
// the deploys in their previous state. The error of a failed deploy is read from the full
// state, only once.
func (w *Worker) readRelease(ctx context.Context, deploys []ReleaseDeployState) releaseCheck {
	c := releaseCheck{Deploys: slices.Clone(deploys)}
	states, err := w.reader.states(releaseIDs(deploys))
	if err != nil {
		w.logger.Warn("release: lettura dello stato dei deploy fallita", "error", err)
		return c
	}
	for i := range c.Deploys {
		d := &c.Deploys[i]
		st, ok := states[d.DeploymentID]
		if !ok {
			continue
		}
		if Terminal(st.State) && st.State != StateSuccess {
			switch {
			case st.State == d.State && d.Error != "":
				st.Error = d.Error
			default:
				if full, err := w.reader.Status(ctx, d.DeploymentID); err == nil {
					st.Error = full.Error
				}
			}
		}
		d.State, d.Error = st.State, st.Error
		if notStarted(&st) {
			c.Pending = append(c.Pending, d.DeploymentID)
		}
	}
	return c
}

// cancelled says whether workflow id was cancelled (e.g. CancelRelease while enqueuing).
func (w *Worker) cancelled(id string) bool {
	ws, err := dbos.ListWorkflows(w.dbos, dbos.WithFilterWorkflowIDs(id), dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false))
	return err == nil && len(ws) == 1 && ws[0].Status == dbos.WorkflowStatusCancelled
}

// releaseIDs returns the IDs of the release deploys.
func releaseIDs(deploys []ReleaseDeployState) []string {
	ids := make([]string, len(deploys))
	for i, d := range deploys {
		ids[i] = d.DeploymentID
	}
	return ids
}

// notStarted reports a deploy that has not started changing the targets yet.
func notStarted(st *Status) bool {
	switch st.State {
	case StateWaitingDependency, StateEnqueued, StateDelayed:
		return true
	case StateAwaitingApproval:
		return st.Rollout == nil
	}
	return false
}

func releaseFailed(ds []ReleaseDeployState) bool {
	return slices.ContainsFunc(ds, func(d ReleaseDeployState) bool { return Terminal(d.State) && d.State != StateSuccess })
}

func sameDeployState(a, b ReleaseDeployState) bool {
	return a.DeploymentID == b.DeploymentID && a.State == b.State && a.RollbackState == b.RollbackState
}

// releaseError summarizes the failed deploys.
func releaseError(ds []ReleaseDeployState) string {
	var parts []string
	for _, d := range ds {
		if Terminal(d.State) && d.State != StateSuccess {
			parts = append(parts, d.Name+": "+d.State)
		}
	}
	return "failed deploys: " + strings.Join(parts, ", ")
}

// rollbackRelease brings the successful deploys back to the previous version, in reverse
// topological order (dependents first). Every rollback is a manual rollback with ID
// "<release>.rollback.<name>", run with the identity of whoever requested the release; it
// stops at the first failure, as for step compensations.
func (w *Worker) rollbackRelease(ctx dbos.Context, rs *ReleaseStatus) error {
	log := w.logger.With("release", rs.ID)
	method, subject, _ := strings.Cut(rs.RequestedBy, ":")
	for i := len(rs.Deploys) - 1; i >= 0; i-- {
		d := &rs.Deploys[i]
		if d.State != StateSuccess {
			continue
		}
		d.RollbackID = rs.ID + releaseRollbackInfix + d.Name
		_, err := dbos.RunAsStep(ctx, func(c context.Context) (string, error) {
			if w.service == nil {
				return "", errors.New("service not configured on the worker")
			}
			c = auth.WithPrincipal(c, &auth.Principal{Method: method, Subject: subject})
			_, err := w.service.Rollback(c, RollbackRequest{
				Application: d.Application, Artifact: d.Artifact, Environment: d.Environment, Redeploy: service.Redeploy{ID: d.RollbackID},
			})
			return "", err
		}, dbos.WithStepName("rollback:"+d.Name))
		if err != nil {
			d.RollbackState, d.Error = string(dbos.WorkflowStatusError), "rollback not started: "+err.Error()
			log.Error("release: rollback non avviato", "deploy", d.Name, "error", err)
			return nil
		}
		d.RollbackState = string(dbos.WorkflowStatusEnqueued)
		for poll := w.chainPoll; !Terminal(d.RollbackState); poll = min(poll*2, chainPollMax) {
			if err := dbos.SetEvent(ctx, releaseEvent, rs); err != nil {
				return err
			}
			if err := waitChain(ctx, poll); err != nil {
				return err
			}
			state, err := dbos.RunAsStep(ctx, func(c context.Context) (string, error) {
				st, err := w.reader.Status(c, d.RollbackID)
				if err != nil {
					return d.RollbackState, nil
				}
				return st.State, nil
			}, dbos.WithStepName("rollback-status:"+d.Name))
			if err != nil {
				return err
			}
			d.RollbackState = state
		}
		log.Info("release: rollback concluso", "deploy", d.Name, "state", d.RollbackState)
		if d.RollbackState != StateSuccess {
			return nil
		}
	}
	return nil
}

// recordRelease records the release outcome in the audit, in a DBOS step.
func (w *Worker) recordRelease(ctx dbos.Context, rs ReleaseStatus) {
	if w.audit == nil {
		return
	}
	e := audit.Entry{Actor: rs.RequestedBy, Action: ActionReleaseFinish, Outcome: rs.State, Deployment: rs.ID, Reason: rs.Error}
	if _, err := dbos.RunAsStep(ctx, func(c context.Context) (string, error) {
		e.Time = time.Now().UTC()
		return "", w.audit.Record(c, e)
	}, dbos.WithStepName("audit")); err != nil {
		w.logger.Error("registrazione della release nell'audit fallita", "release", rs.ID, "error", err)
	}
}

// ActionReleaseFinish is the audit action with the outcome of a release.
const ActionReleaseFinish = "release.finish"

// ReleaseStatus implements ReleaseService: the result if the release has ended, otherwise the
// last published state or, before the first read, the initial one.
func (e *Engine) ReleaseStatus(_ context.Context, id string) (*ReleaseStatus, error) {
	ws, err := dbos.ListWorkflows(e.client, dbos.WithFilterWorkflowIDs(id), dbos.WithFilterName(releaseWorkflowName),
		dbos.WithFilterLoadInput(true), dbos.WithFilterLoadOutput(false))
	if err != nil {
		return nil, err
	}
	if len(ws) == 0 {
		return nil, fmt.Errorf("%w: release %s", ErrNotFound, id)
	}
	return e.releaseStatus(ws[0])
}

func (e *Engine) releaseStatus(w dbos.WorkflowStatus) (*ReleaseStatus, error) {
	var in releaseInput
	if err := decodeInput(w.Input, &in); err != nil {
		return nil, fmt.Errorf("unreadable input of release %s: %w", w.ID, err)
	}
	rs := in.Release
	if ev, err := dbos.GetEvent[ReleaseStatus](e.client, w.ID, releaseEvent, 0); err == nil && ev.ID != "" {
		rs = ev
	}
	rs.CreatedAt, rs.UpdatedAt = w.CreatedAt, w.UpdatedAt
	switch w.Status {
	case dbos.WorkflowStatusSuccess:
		// The final state (SUCCESS or ERROR) is in the last event.
	case dbos.WorkflowStatusPending, dbos.WorkflowStatusEnqueued:
		rs.State = string(dbos.WorkflowStatusPending)
	default:
		rs.State = string(w.Status)
		if w.Error != nil {
			rs.Error = w.Error.Error()
		}
	}
	return &rs, nil
}

// maxReleases limits the releases returned by ListReleases.
const maxReleases = 100

// ListReleases implements ReleaseService.
func (e *Engine) ListReleases(_ context.Context, limit int) ([]ReleaseStatus, error) {
	if limit <= 0 || limit > maxReleases {
		limit = 20
	}
	ws, err := dbos.ListWorkflows(e.client, dbos.WithFilterName(releaseWorkflowName), dbos.WithFilterSortDesc(),
		dbos.WithFilterLimit(limit), dbos.WithFilterLoadInput(true), dbos.WithFilterLoadOutput(false))
	if err != nil {
		return nil, err
	}
	out := []ReleaseStatus{}
	for _, w := range ws {
		rs, err := e.releaseStatus(w)
		if err != nil {
			e.logger.Warn("release non leggibile", "release", w.ID, "error", err)
			continue
		}
		out = append(out, *rs)
	}
	return out, nil
}

// CancelRelease implements ReleaseService: it cancels the unfinished deploys (running ones
// stop at the start of the next step) and the release workflow.
func (e *Engine) CancelRelease(ctx context.Context, id string) error {
	rs, err := e.ReleaseStatus(ctx, id)
	if err != nil {
		return err
	}
	// The workflow first: if it is still enqueuing the deploys it stops (submitDeploys), then
	// the already enqueued ones are cancelled.
	if err := dbos.CancelWorkflow(e.client, id); err != nil {
		return err
	}
	states, err := e.states(releaseIDs(rs.Deploys))
	if err != nil {
		return err
	}
	for _, d := range rs.Deploys {
		if st, ok := states[d.DeploymentID]; ok && !Terminal(st.State) {
			if err := e.Cancel(ctx, d.DeploymentID); err != nil {
				return fmt.Errorf("deploy %s: %w", d.Name, err)
			}
		}
	}
	e.logger.Info("release annullata", "release", id)
	return nil
}

// decodeInput decodes the input of a workflow read with ListWorkflows: with a DBOS client it
// is the serialized JSON string, in the process running it it may already be decoded.
func decodeInput(input any, v any) error {
	data, ok := input.(string)
	if !ok {
		b, err := json.Marshal(input)
		if err != nil {
			return err
		}
		data = string(b)
	}
	return json.Unmarshal([]byte(data), v)
}

var _ service.ReleaseService = (*Engine)(nil)
