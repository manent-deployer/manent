package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/joeg-ita/manent/internal/audit"
	"github.com/joeg-ita/manent/internal/auth"
	"github.com/joeg-ita/manent/internal/change"
	"github.com/joeg-ita/manent/internal/chat"
	"github.com/joeg-ita/manent/internal/pipeline"
)

const (
	// approvalSuffix tells the approval workflow ID apart from the deploy's.
	approvalSuffix = ".approval"
	// approvalTopic is the topic of the decision messages.
	approvalTopic = "approval"
	// approvalEvent is the key of the event with the approval state.
	approvalEvent = "approval"
)

// approvalInput is the input of the approval workflow.
type approvalInput struct {
	DeploymentID string  `json:"deployment_id"`
	Request      Request `json:"request"`
	// QueueName is the deploy queue, where the approved deploy is enqueued.
	QueueName string `json:"queue_name"`
}

// approvalQueue is the queue of approval workflows: separate from the deploy queue, because a
// wait can last days and must neither take an execution slot nor block the environment. It is
// not partitioned and has no concurrency limits.
func approvalQueue(deployQueue string) string {
	return deployQueue + "-approvals"
}

// enqueueApproval enqueues the approval workflow of deploy id.
func enqueueApproval(client dbos.Client, deployQueue string, req Request, id string) error {
	opts := []dbos.EnqueueOption{
		dbos.WithEnqueueWorkflowID(id + approvalSuffix),
		dbos.WithEnqueueAttributes(attributes(req)),
	}
	if req.RequestedBy != "" {
		opts = append(opts, dbos.WithEnqueueAuthenticatedUser(req.RequestedBy))
	}
	_, err := dbos.Enqueue[ApprovalState](client, approvalQueue(deployQueue), approvalWorkflowName,
		approvalInput{DeploymentID: id, Request: req, QueueName: deployQueue}, opts...)
	return err
}

// approvalWorkflow waits for decisions on the deploy until the required number of approvals,
// the first rejection or the expiry. When change management requires it, it opens the change
// ticket and also waits for its approval in the ITSM, reading it at growing intervals (from
// integrations.change.poll_interval up to 5 minutes).
// If approved, it enqueues the deploy with the ID already given to the requester. The current
// state is published as a DBOS event, read by Status.
func (w *Worker) approvalWorkflow(ctx dbos.Context, in approvalInput) (ApprovalState, error) {
	rule := in.Request.Approval
	if rule.InRollout() {
		// manent approvals are asked during the rollout: only the change is left here.
		rule = nil
	}
	chg := in.Request.Change
	if chg != nil && !chg.Approval {
		chg = nil
	}
	if rule == nil && chg == nil {
		return ApprovalState{}, errors.New("approval without rule")
	}
	timeout := time.Duration(0)
	if rule != nil {
		timeout = rule.Timeout
	}
	if chg != nil {
		timeout = max(timeout, chg.Timeout)
	}
	expires, err := dbos.RunAsStep(ctx, func(context.Context) (time.Time, error) {
		return time.Now().UTC().Add(timeout), nil
	}, dbos.WithStepName("expires"))
	if err != nil {
		return ApprovalState{}, err
	}
	st := ApprovalState{Status: ApprovalPending, ExpiresAt: expires}
	if rule != nil {
		st.Required, st.AllowSelf, st.Approvers = rule.Required, rule.AllowSelf, rule.Approvers
	}
	log := w.logger.With("deployment", in.DeploymentID)
	if chg != nil {
		t, err := w.openApprovalChange(ctx, in)
		if err != nil {
			return st, err
		}
		st.Change = t
		w.changeDecision(&st, t, expires.Add(-timeout))
		st.Status = approvalOutcome(st)
	}
	refs := w.notifyChat(ctx, in.DeploymentID, in.Request.Notify, w.approvalMessage(in, st, chat.EventApprovalRequested), nil)
	poll := w.changePoll
	for st.Status == ApprovalPending {
		if err := dbos.SetEvent(ctx, approvalEvent, st); err != nil {
			return st, err
		}
		now, err := dbos.RunAsStep(ctx, func(context.Context) (time.Time, error) { return time.Now(), nil }, dbos.WithStepName("now"))
		if err != nil {
			return st, err
		}
		remaining := max(expires.Sub(now), time.Millisecond)
		wait := remaining
		polling := st.Change != nil && st.Change.Approval == change.ApprovalPending
		if polling && poll < remaining {
			wait = poll
		}
		dec, err := dbos.Recv[Decision](ctx, approvalTopic, wait)
		switch {
		case errors.Is(err, context.DeadlineExceeded) && wait == remaining:
			st.Status = ApprovalExpired
			continue
		case errors.Is(err, context.DeadlineExceeded):
			w.refreshApprovalChange(ctx, in, &st)
			poll = min(poll*2, chainPollMax)
			continue
		case err != nil:
			return st, err
		}
		// The service checks permissions and self-approval before sending the decision: invalid ones
		// are discarded here anyway.
		switch {
		case dec.Approver == "":
			continue
		case !st.AllowSelf && dec.Approver == in.Request.RequestedBy:
			log.Warn("decisione scartata: auto-approvazione non ammessa", "approver", dec.Approver)
			continue
		case slices.ContainsFunc(st.Decisions, func(d Decision) bool { return d.Approver == dec.Approver }):
			log.Warn("decisione scartata: approvatore già registrato", "approver", dec.Approver)
			continue
		}
		st.Decisions = append(st.Decisions, dec)
		st.Status = approvalOutcome(st)
	}
	if err := dbos.SetEvent(ctx, approvalEvent, st); err != nil {
		return st, err
	}
	attrs := maps.Clone(attributes(in.Request))
	attrs[attrOutcome] = st.Status
	if st.Change != nil {
		attrs[attrChange], attrs[attrChangeURL] = st.Change.Number, st.Change.URL
	}
	if err := dbos.SetWorkflowAttributes(ctx, in.DeploymentID+approvalSuffix, attrs); err != nil {
		return st, err
	}
	if st.Status == ApprovalApproved {
		if err := w.enqueueApproved(ctx, in); err != nil {
			return st, err
		}
	} else {
		// The deploy will not run: whoever waits for it can end at once.
		w.notifyWaiters(ctx, in.DeploymentID)
	}
	log.Info("approvazione conclusa", "status", st.Status, "decisions", len(st.Decisions))
	w.recordApproval(ctx, in, st)
	w.notifyChat(ctx, in.DeploymentID, in.Request.Notify, w.approvalMessage(in, st, approvalChatEvent(st.Status)), refs)
	return st, nil
}

// approvalOutcome computes the approval state from decisions and change: a rejection ends it
// at once; it needs the required number of approvals and, if configured, the change approved
// in the ITSM.
func approvalOutcome(st ApprovalState) string {
	if slices.ContainsFunc(st.Decisions, func(d Decision) bool { return !d.Approve }) {
		return ApprovalRejected
	}
	if st.Change != nil {
		switch st.Change.Approval {
		case change.ApprovalRejected:
			return ApprovalRejected
		case change.ApprovalPending:
			return ApprovalPending
		}
	}
	if len(st.Decisions) >= st.Required {
		return ApprovalApproved
	}
	return ApprovalPending
}

// approvalChatEvent is the notification event for the outcome of an approval.
func approvalChatEvent(status string) string {
	switch status {
	case ApprovalApproved:
		return chat.EventApprovalApproved
	case ApprovalRejected:
		return chat.EventApprovalRejected
	}
	return chat.EventApprovalExpired
}

// approvalMessage prepares the notification of an approval.
func (w *Worker) approvalMessage(in approvalInput, st ApprovalState, event string) chat.Message {
	msg := w.chatMessage(in.DeploymentID, in.Request, event)
	a := &chat.Approval{Required: st.Required, ExpiresAt: st.ExpiresAt, Change: st.Change != nil}
	for _, d := range st.Decisions {
		a.Decisions = append(a.Decisions, chat.Decision{Approver: d.Approver, Approve: d.Approve, Comment: d.Comment})
	}
	msg.Approval = a
	msg.Change = chatChange(st.Change)
	return msg
}

// openApprovalChange opens the change ticket to approve, in the change:open step, and makes
// it visible in the approval state.
func (w *Worker) openApprovalChange(ctx dbos.Context, in approvalInput) (*ChangeTicket, error) {
	t, err := dbos.RunAsStep(ctx, func(c context.Context) (change.Ticket, error) {
		if w.change == nil {
			return change.Ticket{}, errNoChange
		}
		t, err := w.change.Open(c, w.changeRequest(in.DeploymentID, in.Request))
		if err == nil {
			w.logger.InfoContext(c, "change aperto, in attesa di approvazione", "deployment", in.DeploymentID, "change", t.Number, "system", w.change.Name())
		}
		return t, changeErr(err)
	}, dbos.WithStepName("change:open"), dbos.WithStepMaxRetries(3), dbos.WithStepRetryPredicate(pipeline.Retryable))
	if err != nil {
		return nil, fmt.Errorf("change management: %w", err)
	}
	ct := ticketOf(t)
	attrs := maps.Clone(attributes(in.Request))
	attrs[attrChange], attrs[attrChangeURL] = ct.Number, ct.URL
	if err := dbos.SetWorkflowAttributes(ctx, in.DeploymentID+approvalSuffix, attrs); err != nil {
		return ct, err
	}
	return ct, nil
}

// refreshApprovalChange reads the change approval again, in the change:status step. A read
// error is logged and the read repeated at the next round. A rejected change adds a negative
// decision with the ticket number.
func (w *Worker) refreshApprovalChange(ctx dbos.Context, in approvalInput, st *ApprovalState) {
	type read struct {
		Ticket change.Ticket `json:"ticket"`
		At     time.Time     `json:"at"`
	}
	r, err := dbos.RunAsStep(ctx, func(c context.Context) (read, error) {
		if w.change == nil {
			return read{}, errNoChange
		}
		t, err := w.change.Refresh(c, w.ticketFor(st.Change))
		return read{Ticket: t, At: time.Now().UTC()}, err
	}, dbos.WithStepName("change:status"))
	if err != nil {
		w.logger.Warn("lettura del change fallita", "deployment", in.DeploymentID, "change", st.Change.Number, "error", err)
		return
	}
	st.Change = ticketOf(r.Ticket)
	w.changeDecision(st, st.Change, r.At)
	st.Status = approvalOutcome(*st)
}

// changeDecision records the change rejection in the ITSM as a negative decision, with the
// ticket number as approver: so it shows up with the other decisions.
func (w *Worker) changeDecision(st *ApprovalState, t *ChangeTicket, at time.Time) {
	if t.Approval != change.ApprovalRejected {
		return
	}
	system := "itsm"
	if w.change != nil {
		system = w.change.Name()
	}
	st.Decisions = append(st.Decisions, Decision{
		Approver: system + ":" + t.Number, Approve: false, Comment: "change respinto nel sistema ITSM", Time: at,
	})
}

// enqueueApproved enqueues the approved deploy. A deploy scheduled with schedule.at waits for
// its time, if not already past.
func (w *Worker) enqueueApproved(ctx dbos.Context, in approvalInput) error {
	d := in.Request.Descriptor
	delay, err := dbos.RunAsStep(ctx, func(context.Context) (time.Duration, error) {
		if d.Schedule == nil || d.Schedule.At == nil {
			return 0, nil
		}
		return time.Until(*d.Schedule.At), nil
	}, dbos.WithStepName("delay"))
	if err != nil {
		return err
	}
	opts := deployOptions(in.Request, in.DeploymentID)
	if delay > 0 {
		opts = append(opts, dbos.WithEnqueueDelay(delay))
	}
	_, err = dbos.Enqueue[Result](ctx, in.QueueName, workflowName, in.Request, opts...)
	return err
}

// recordApproval records the approval outcome in the audit, in a DBOS step.
func (w *Worker) recordApproval(ctx dbos.Context, in approvalInput, st ApprovalState) {
	if w.audit == nil {
		return
	}
	d := in.Request.Descriptor
	e := audit.Entry{
		Actor: in.Request.RequestedBy, Action: ActionApprovalFinish, Outcome: st.Status,
		Application: d.Application, Environment: d.Environment, Deployment: in.DeploymentID,
	}
	if _, err := dbos.RunAsStep(ctx, func(c context.Context) (string, error) {
		e.Time = time.Now().UTC()
		return "", w.audit.Record(c, e)
	}, dbos.WithStepName("audit")); err != nil {
		w.logger.Error("registrazione dell'approvazione nell'audit fallita", "deployment", in.DeploymentID, "error", err)
	}
}

// ActionApprovalFinish is the audit action with the outcome of an approval.
const ActionApprovalFinish = "approval.finish"

// approvalWorkflowState is the state shown for a deploy that has no workflow yet.
func approvalWorkflowState(st dbos.WorkflowStatusType, outcome string) string {
	switch st {
	case dbos.WorkflowStatusPending, dbos.WorkflowStatusEnqueued, dbos.WorkflowStatusDelayed:
		return StateAwaitingApproval
	case dbos.WorkflowStatusSuccess:
		switch outcome {
		case ApprovalRejected:
			return StateRejected
		case ApprovalExpired:
			return StateExpired
		case ApprovalApproved:
			// The deploy is about to be enqueued.
			return string(dbos.WorkflowStatusEnqueued)
		}
	}
	return string(st)
}

// approval reads the state of approval w (read with its output): the result if ended,
// otherwise the last published event. An approval not yet started has no state (nil).
func (e *Engine) approval(w dbos.WorkflowStatus) *ApprovalState {
	if w.Status == dbos.WorkflowStatusSuccess {
		var a ApprovalState
		if err := decodeInput(w.Output, &a); err == nil {
			return &a
		}
		return nil
	}
	a, err := dbos.GetEvent[ApprovalState](e.client, w.ID, approvalEvent, 0)
	if err != nil || a.Status == "" {
		return nil
	}
	return &a
}

// Decide records the decision of the identity in the context on the deploy awaiting approval.
// Permissions and the self-approval ban are checked by authz.Guard; the workflow discards
// invalid decisions anyway.
func (e *Engine) Decide(ctx context.Context, id string, r DecisionRequest) error {
	st, err := e.Status(ctx, id)
	if err != nil {
		return err
	}
	if st.State != StateAwaitingApproval {
		return fmt.Errorf("%w: deploy %s is not awaiting approval (state %s)", ErrInvalidRequest, id, st.State)
	}
	approver := auth.FromContext(ctx).String()
	if st.Approval != nil && slices.ContainsFunc(st.Approval.Decisions, func(d Decision) bool { return d.Approver == approver }) {
		return fmt.Errorf("%w: %s has already given a decision on deploy %s", ErrInvalidRequest, approver, id)
	}
	dec := Decision{Approver: approver, Approve: r.Approve, Comment: r.Comment, Time: e.now().UTC()}
	target, topic := id+approvalSuffix, approvalTopic
	if st.Rollout != nil && awaitsDecision(st.Rollout.Status) {
		// Approval between two waves or steps: the decision goes to the deploy workflow.
		target, topic = id, rolloutApprovalTopic
	}
	if err := dbos.Send(e.client, target, dec, topic); err != nil {
		return err
	}
	e.logger.Info("decisione registrata", "deployment", id, "approver", approver, "approve", r.Approve)
	return nil
}
