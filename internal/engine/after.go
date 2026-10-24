package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/joeg-ita/manent/internal/audit"
	"github.com/joeg-ita/manent/internal/callback"
	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/service"
)

const (
	// afterWorkflowName is the workflow that waits for a deploy's dependencies. The name must not
	// change: DBOS uses it for recovery.
	afterWorkflowName = "manent.after"
	// afterSuffix tells the wait workflow ID apart from the deploy's.
	afterSuffix = ".after"
	// afterEvent is the key of the event with the wait state.
	afterEvent = "after"
	// attrAfter marks the workflows (wait, approval, deploy) of a deploy with dependencies.
	attrAfter = "after"
	// attrRelease is the composite release a deploy belongs to.
	attrRelease = "release"
	// attrWaitsFor are the IDs of the deploys followed by a wait or a release, space separated:
	// a deploy that ends notifies the workflows following it (notifyWaiters).
	attrWaitsFor = "waits_for"
	// chainTopic is the topic of the messages that wake up a wait or a release.
	chainTopic = "chain"
)

// Read intervals of the state of dependencies and releases: start at chainPollMin and double
// up to chainPollMax. Normally the wait is woken earlier by a message from whoever ends
// (notifyWaiters): periodic reads cover the cases without a message (old workflows,
// MAX_RECOVERY_ATTEMPTS_EXCEEDED, retention).
const (
	defaultChainPoll = 5 * time.Second
	chainPollMax     = 5 * time.Minute
)

// afterInput is the input of the dependency wait workflow.
type afterInput struct {
	DeploymentID string  `json:"deployment_id"`
	Request      Request `json:"request"`
	// QueueName is the deploy queue.
	QueueName string `json:"queue_name"`
}

// checkAfter checks the dependencies at submission: they must exist and be neither dry-runs
// nor ended without success. It returns true if all have already succeeded and no wait is needed.
func (e *Engine) checkAfter(ctx context.Context, d *descriptor.Descriptor) (bool, error) {
	if _, err := d.AfterWait(); err != nil {
		return false, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	done := true
	mine := tenantOf(d)
	imported := map[string]bool{}
	for _, imp := range d.Imports {
		imported[imp.Deployment] = true
	}
	for _, id := range d.After {
		st, err := e.Status(ctx, id)
		switch {
		case err == nil && e.tenants != nil && st.Tenant != mine && !imported[id]:
			// Across tenants a deploy can only wait for the producer of the public exports it imports.
			return false, fmt.Errorf("%w: after: deploy %s belongs to another tenant: across tenants only the producers of imported public exports are allowed", ErrInvalidRequest, id)
		case errors.Is(err, ErrNotFound):
			return false, fmt.Errorf("%w: after: deploy %s does not exist", ErrInvalidRequest, id)
		case err != nil:
			return false, err
		case st.DryRun:
			return false, fmt.Errorf("%w: after: %s is a dry-run", ErrInvalidRequest, id)
		case st.State == StateSuccess:
		case Terminal(st.State):
			return false, fmt.Errorf("%w: after: deploy %s ended in %s", ErrInvalidRequest, id, st.State)
		default:
			done = false
		}
	}
	return done, nil
}

// enqueueAfter enqueues the dependency wait of deploy id on the approvals queue: like an
// approval it can last long and must not take an environment slot.
func enqueueAfter(client dbos.Client, deployQueue string, req Request, id string) error {
	attrs := attributes(req)
	attrs[attrWaitsFor] = strings.Join(req.Descriptor.After, " ")
	opts := []dbos.EnqueueOption{
		dbos.WithEnqueueWorkflowID(id + afterSuffix),
		dbos.WithEnqueueAttributes(attrs),
	}
	if req.RequestedBy != "" {
		opts = append(opts, dbos.WithEnqueueAuthenticatedUser(req.RequestedBy))
	}
	_, err := dbos.Enqueue[AfterState](client, approvalQueue(deployQueue), afterWorkflowName,
		afterInput{DeploymentID: id, Request: req, QueueName: deployQueue}, opts...)
	return err
}

// afterWorkflow waits for all dependencies to succeed. Then it enqueues the approval, if the
// rule requires one, otherwise the deploy, with the ID already given to the requester. If a
// dependency fails or the wait expires the deploy does not run.
func (w *Worker) afterWorkflow(ctx dbos.Context, in afterInput) (AfterState, error) {
	d := in.Request.Descriptor
	wait, err := d.AfterWait()
	if err != nil {
		return AfterState{}, err
	}
	expires, err := dbos.RunAsStep(ctx, func(context.Context) (time.Time, error) {
		return time.Now().UTC().Add(wait), nil
	}, dbos.WithStepName("expires"))
	if err != nil {
		return AfterState{}, err
	}
	st := AfterState{Status: service.AfterWaiting, ExpiresAt: expires}
	var published []DependencyState
	for poll := w.chainPoll; ; poll = min(poll*2, chainPollMax) {
		check, err := dbos.RunAsStep(ctx, func(c context.Context) (afterCheck, error) {
			return w.readDependencies(c, d.After), nil
		}, dbos.WithStepName("dependencies"))
		if err != nil {
			return st, err
		}
		st.Dependencies = check.Dependencies
		switch {
		case check.Failed != "":
			st.Status, st.Reason = service.AfterFailed, check.Failed
		case check.Done:
			st.Status = service.AfterSatisfied
		case !check.Now.Before(expires):
			st.Status, st.Reason = service.AfterExpired, "wait for dependencies expired"
		}
		if st.Status != service.AfterWaiting {
			break
		}
		if !slices.Equal(published, st.Dependencies) {
			if err := dbos.SetEvent(ctx, afterEvent, st); err != nil {
				return st, err
			}
			published = st.Dependencies
		}
		if err := waitChain(ctx, min(poll, max(expires.Sub(check.Now), time.Millisecond))); err != nil {
			return st, err
		}
	}
	if err := dbos.SetEvent(ctx, afterEvent, st); err != nil {
		return st, err
	}
	if st.Status == service.AfterSatisfied {
		if err := w.enqueueSatisfied(ctx, in); err != nil {
			return st, err
		}
	}
	// The outcome is recorded after enqueuing: in listings the satisfied wait is replaced by the
	// approval or the deploy.
	attrs := maps.Clone(attributes(in.Request))
	attrs[attrOutcome] = st.Status
	if err := dbos.SetWorkflowAttributes(ctx, in.DeploymentID+afterSuffix, attrs); err != nil {
		return st, err
	}
	if st.Status != service.AfterSatisfied {
		// The deploy will not run: whoever waits for it can end at once.
		w.notifyWaiters(ctx, in.DeploymentID)
	}
	w.logger.Info("dependency wait ended", "deployment", in.DeploymentID, "status", st.Status, "reason", st.Reason)
	w.recordAfter(ctx, in, st)
	return st, nil
}

// afterCheck is the result of a dependency read, persisted as a step.
type afterCheck struct {
	Dependencies []DependencyState `json:"dependencies"`
	Done         bool              `json:"done"`
	// Failed explains why a dependency can no longer succeed.
	Failed string    `json:"failed,omitempty"`
	Now    time.Time `json:"now"`
}

// readDependencies reads the state of the dependencies with a single query. A read error
// leaves the dependencies waiting: they are read again at the next round.
func (w *Worker) readDependencies(_ context.Context, ids []string) afterCheck {
	c := afterCheck{Done: true, Now: time.Now().UTC()}
	states, err := w.reader.states(ids)
	if err != nil {
		w.logger.Warn("reading the state of the dependencies failed", "dependencies", ids, "error", err)
	}
	for _, id := range ids {
		state := "UNKNOWN"
		st, ok := states[id]
		switch {
		case err != nil:
		case !ok:
			c.Failed = cmpString(c.Failed, "deploy "+id+" no longer exists")
		case st.State == StateSuccess && !st.DryRun:
			state = st.State
		case Terminal(st.State):
			state = st.State
			c.Failed = cmpString(c.Failed, fmt.Sprintf("deploy %s ended in %s", id, st.State))
		default:
			state = st.State
		}
		if state != StateSuccess {
			c.Done = false
		}
		c.Dependencies = append(c.Dependencies, DependencyState{ID: id, State: state})
	}
	return c
}

// states reads with a single query the essential state of deploys ids (no result, rollout or
// approvals): enough to follow dependencies and releases. A workflow that has already
// recorded its outcome (outcome attribute) counts as ended even if DBOS has not marked it yet:
// that is when it notifies its waiters. Deploys without a workflow are missing from the map.
func (e *Engine) states(ids []string) (map[string]Status, error) {
	all := make([]string, 0, 3*len(ids))
	for _, id := range ids {
		all = append(all, id, id+approvalSuffix, id+afterSuffix)
	}
	ws, err := dbos.ListWorkflows(e.client, dbos.WithFilterWorkflowIDs(all...),
		dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false))
	if err != nil {
		return nil, err
	}
	out := make(map[string]Status, len(ids))
	for _, id := range ids {
		g := groupDeploy(id, ws)
		if w := cmp.Or(g.deploy, g.approval, g.after); w != nil {
			out[id] = toStatus(settled(*w))
		}
	}
	return out, nil
}

// settled applies to a still running workflow the outcome it already recorded in its
// attributes (deploy, approval, wait).
func settled(w dbos.WorkflowStatus) dbos.WorkflowStatus {
	outcome := attr(w, attrOutcome)
	if w.Status != dbos.WorkflowStatusPending || outcome == "" {
		return w
	}
	switch w.Name {
	case workflowName:
		w.Status = dbos.WorkflowStatusError
		if outcome == callback.StatusSucceeded {
			w.Status = dbos.WorkflowStatusSuccess
		}
	case approvalWorkflowName, afterWorkflowName:
		w.Status = dbos.WorkflowStatusSuccess
	}
	return w
}

// waitChain waits for a message from whoever ends (notifyWaiters) or for d to expire.
func waitChain(ctx dbos.Context, d time.Duration) error {
	_, err := dbos.Recv[string](ctx, chainTopic, d)
	if errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}

// waitersOf returns the IDs of the unfinished wait and release workflows following deploy id
// (waits_for attribute).
func waitersOf(c dbos.Client, id string) ([]string, error) {
	ws, err := dbos.ListWorkflows(c, dbos.WithFilterName(afterWorkflowName, releaseWorkflowName),
		dbos.WithFilterStatus(dbos.WorkflowStatusPending, dbos.WorkflowStatusEnqueued),
		dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, w := range ws {
		if slices.Contains(strings.Fields(attr(w, attrWaitsFor)), id) {
			out = append(out, w.ID)
		}
	}
	return out, nil
}

// wakeMessages are the messages that wake up workflows ids.
func wakeMessages(id string, ids []string) []dbos.SendMessage {
	msgs := make([]dbos.SendMessage, len(ids))
	for i, dst := range ids {
		msgs[i] = dbos.SendMessage{DestinationID: dst, Message: id, Topic: chainTopic}
	}
	return msgs
}

// notifyWaiters wakes up, from the workflow of an ended deploy, the waits and releases
// following it. Call it after recording the outcome in the attributes, which the woken
// workflows read before DBOS marks the workflow as ended. An error does not stop the workflow:
// waiters read the state periodically anyway.
func (w *Worker) notifyWaiters(ctx dbos.Context, id string) {
	ids, err := dbos.RunAsStep(ctx, func(context.Context) ([]string, error) {
		return waitersOf(w.dbos, id)
	}, dbos.WithStepName("waiters"))
	if err == nil && len(ids) > 0 {
		err = dbos.SendBulk(ctx, wakeMessages(id, ids))
	}
	if err != nil {
		w.logger.Warn("notifying whoever waits for the deploy failed", "deployment", id, "error", err)
	}
}

// notifyWaiters wakes up the waits and releases following deploy id, after a cancellation.
func (e *Engine) notifyWaiters(id string) {
	ids, err := waitersOf(e.client, id)
	if err == nil && len(ids) > 0 {
		err = dbos.SendBulk(e.client, wakeMessages(id, ids))
	}
	if err != nil {
		e.logger.Warn("notifying whoever waits for the deploy failed", "deployment", id, "error", err)
	}
}

// enqueueSatisfied enqueues what follows the wait: the approval, if required, or the deploy.
func (w *Worker) enqueueSatisfied(ctx dbos.Context, in afterInput) error {
	if needsApproval(in.Request) {
		return enqueueApproval(ctx, in.QueueName, in.Request, in.DeploymentID)
	}
	return w.enqueueApproved(ctx, approvalInput{DeploymentID: in.DeploymentID, Request: in.Request, QueueName: in.QueueName})
}

// recordAfter records the wait outcome in the audit, in a DBOS step.
func (w *Worker) recordAfter(ctx dbos.Context, in afterInput, st AfterState) {
	if w.audit == nil {
		return
	}
	d := in.Request.Descriptor
	e := audit.Entry{
		Actor: in.Request.RequestedBy, Action: ActionAfterFinish, Outcome: st.Status,
		Application: d.Application, Environment: d.Environment, Deployment: in.DeploymentID, Reason: st.Reason,
	}
	if _, err := dbos.RunAsStep(ctx, func(c context.Context) (string, error) {
		e.Time = time.Now().UTC()
		return "", w.audit.Record(c, e)
	}, dbos.WithStepName("audit")); err != nil {
		w.logger.Error("recording the wait in the audit failed", "deployment", in.DeploymentID, "error", err)
	}
}

// ActionAfterFinish is the audit action with the outcome of the dependency wait.
const ActionAfterFinish = "after.finish"

// afterWorkflowState is the state shown for a deploy waiting for its dependencies.
func afterWorkflowState(st dbos.WorkflowStatusType, outcome string) string {
	switch st {
	case dbos.WorkflowStatusPending, dbos.WorkflowStatusEnqueued, dbos.WorkflowStatusDelayed:
		return StateWaitingDependency
	case dbos.WorkflowStatusSuccess:
		if outcome == service.AfterSatisfied {
			// Approval or deploy are about to be enqueued.
			return string(dbos.WorkflowStatusEnqueued)
		}
		return StateDependencyFailed
	}
	return string(st)
}

// afterState reads the state of wait w (read with its output): the result if ended,
// otherwise the last published event.
func (e *Engine) afterState(w dbos.WorkflowStatus) *AfterState {
	if w.Status == dbos.WorkflowStatusSuccess {
		var a AfterState
		if err := decodeInput(w.Output, &a); err == nil {
			return &a
		}
	}
	a, err := dbos.GetEvent[AfterState](e.client, w.ID, afterEvent, 0)
	if err != nil || a.Status == "" {
		return nil
	}
	return &a
}

// pendingStatus returns the state of a deploy that has no workflow yet: waiting for its
// dependencies or an approval, or not run because of one of them. If both exist, the approval
// (enqueued after the wait) represents the deploy.
func (e *Engine) pendingStatus(id string, g deployWorkflows) (*Status, error) {
	w := cmp.Or(g.approval, g.after)
	if w == nil {
		return nil, fmt.Errorf("%w: deploy %s", ErrNotFound, id)
	}
	s := toStatus(*w)
	if g.approval != nil {
		s.Approval = e.approval(*g.approval)
	}
	if attr(*w, attrAfter) == "true" && g.after != nil {
		s.After = e.afterState(*g.after)
		if s.After != nil && s.State == StateDependencyFailed {
			s.Error = s.After.Reason
		}
	}
	return &s, nil
}

// trimPendingSuffix returns the deploy ID from that of a wait or approval workflow.
func trimPendingSuffix(id string) string {
	return strings.TrimSuffix(strings.TrimSuffix(id, approvalSuffix), afterSuffix)
}
