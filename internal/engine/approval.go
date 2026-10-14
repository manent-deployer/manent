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
	"github.com/joeg-ita/manent/internal/service"
)

const (
	// approvalSuffix distingue l'ID del workflow di approvazione da quello del deploy.
	approvalSuffix = ".approval"
	// approvalTopic è l'argomento dei messaggi con le decisioni.
	approvalTopic = "approval"
	// approvalEvent è la chiave dell'evento con lo stato dell'approvazione.
	approvalEvent = "approval"
)

// approvalInput è l'input del workflow di approvazione.
type approvalInput struct {
	DeploymentID string  `json:"deployment_id"`
	Request      Request `json:"request"`
	// QueueName è la coda dei deploy, su cui accodare il deploy approvato.
	QueueName string `json:"queue_name"`
}

// approvalQueue è la coda dei workflow di approvazione: separata da quella dei deploy, perché
// un'attesa può durare giorni e non deve occupare uno slot di esecuzione né bloccare
// l'ambiente. Non è partizionata e non ha limiti di concorrenza.
func approvalQueue(deployQueue string) string {
	return deployQueue + "-approvals"
}

// enqueueApproval accoda il workflow di approvazione del deploy id.
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

// approvalWorkflow attende le decisioni sul deploy fino al numero richiesto di approvazioni,
// al primo rifiuto o alla scadenza. Con il change management che lo richiede, apre il ticket
// di change e attende anche la sua approvazione nel sistema ITSM, leggendola a intervalli
// crescenti (da integrations.change.poll_interval fino a 5 minuti).
// Se approvato, accoda il deploy con l'ID già comunicato al richiedente. Lo stato corrente è
// pubblicato come evento DBOS, letto da Status.
func (w *Worker) approvalWorkflow(ctx dbos.Context, in approvalInput) (ApprovalState, error) {
	rule := in.Request.Approval
	chg := in.Request.Change
	if chg != nil && !chg.Approval {
		chg = nil
	}
	if rule == nil && chg == nil {
		return ApprovalState{}, errors.New("approvazione senza regola")
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
		st.Required, st.AllowSelf = rule.Required, rule.AllowSelf
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
		// Il servizio verifica permessi e auto-approvazione prima di inviare la decisione:
		// qui si scartano comunque quelle non valide.
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
		// Il deploy non verrà eseguito: chi lo attende può concludersi subito.
		w.notifyWaiters(ctx, in.DeploymentID)
	}
	log.Info("approvazione conclusa", "status", st.Status, "decisions", len(st.Decisions))
	w.recordApproval(ctx, in, st)
	w.notifyChat(ctx, in.DeploymentID, in.Request.Notify, w.approvalMessage(in, st, approvalChatEvent(st.Status)), refs)
	return st, nil
}

// approvalOutcome calcola lo stato dell'approvazione dalle decisioni e dal change: un rifiuto
// la conclude subito; serve il numero richiesto di approvazioni e, se previsto, il change
// approvato nel sistema ITSM.
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

// approvalChatEvent è l'evento delle notifiche per l'esito di un'approvazione.
func approvalChatEvent(status string) string {
	switch status {
	case ApprovalApproved:
		return chat.EventApprovalApproved
	case ApprovalRejected:
		return chat.EventApprovalRejected
	}
	return chat.EventApprovalExpired
}

// approvalMessage prepara la notifica di un'approvazione.
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

// openApprovalChange apre il ticket di change da approvare, nello step change:open, e lo
// rende visibile nello stato dell'approvazione.
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

// refreshApprovalChange rilegge l'approvazione del change, nello step change:status. Un
// errore di lettura viene registrato e la lettura ripetuta al giro successivo. Un change
// respinto aggiunge una decisione negativa con il numero del ticket.
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

// changeDecision registra il rifiuto del change nel sistema ITSM come una decisione negativa,
// con il numero del ticket come approvatore: così compare con le altre decisioni.
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

// enqueueApproved accoda il deploy approvato. Un deploy pianificato con schedule.at resta in
// attesa fino all'orario, se non è già passato.
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

// recordApproval registra nell'audit l'esito dell'approvazione, in uno step DBOS.
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

// ActionApprovalFinish è l'azione di audit con l'esito di un'approvazione.
const ActionApprovalFinish = "approval.finish"

// approvalWorkflowState è lo stato esposto per un deploy che non ha ancora un workflow.
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
			// Il deploy sta per essere accodato.
			return string(dbos.WorkflowStatusEnqueued)
		}
	}
	return string(st)
}

// approval legge lo stato dell'approvazione w (letta con l'output): il risultato se conclusa,
// altrimenti l'ultimo evento pubblicato. Un'approvazione non ancora avviata non ha stato (nil).
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

// Decide registra la decisione dell'identità nel context sul deploy in attesa di approvazione.
// I permessi e il divieto di auto-approvazione sono verificati da authz.Guard; il workflow
// scarta comunque le decisioni non valide.
func (e *Engine) Decide(ctx context.Context, id string, r DecisionRequest) error {
	st, err := e.Status(ctx, id)
	if err != nil {
		return err
	}
	if st.State != StateAwaitingApproval {
		return fmt.Errorf("%w: il deploy %s non è in attesa di approvazione (stato %s)", ErrInvalidRequest, id, st.State)
	}
	approver := auth.FromContext(ctx).String()
	if st.Approval != nil && slices.ContainsFunc(st.Approval.Decisions, func(d Decision) bool { return d.Approver == approver }) {
		return fmt.Errorf("%w: %s ha già espresso una decisione sul deploy %s", ErrInvalidRequest, approver, id)
	}
	dec := Decision{Approver: approver, Approve: r.Approve, Comment: r.Comment, Time: e.now().UTC()}
	target, topic := id+approvalSuffix, approvalTopic
	if st.Rollout != nil && st.Rollout.Status == service.RolloutPausedApproval {
		// Approvazione tra due ondate: la decisione va al workflow del deploy.
		target, topic = id, rolloutApprovalTopic
	}
	if err := dbos.Send(e.client, target, dec, topic); err != nil {
		return err
	}
	e.logger.Info("decisione registrata", "deployment", id, "approver", approver, "approve", r.Approve)
	return nil
}
