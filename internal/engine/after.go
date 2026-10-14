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
	// afterWorkflowName è il workflow che attende le dipendenze di un deploy. Il nome non va
	// cambiato: DBOS lo usa per il recovery.
	afterWorkflowName = "manent.after"
	// afterSuffix distingue l'ID del workflow di attesa da quello del deploy.
	afterSuffix = ".after"
	// afterEvent è la chiave dell'evento con lo stato dell'attesa.
	afterEvent = "after"
	// attrAfter segna i workflow (attesa, approvazione, deploy) di un deploy con dipendenze.
	attrAfter = "after"
	// attrRelease è la release composta a cui appartiene un deploy.
	attrRelease = "release"
	// attrWaitsFor sono gli ID dei deploy seguiti da un'attesa o da una release, separati da
	// spazi: un deploy che si conclude avvisa i workflow che lo seguono (notifyWaiters).
	attrWaitsFor = "waits_for"
	// chainTopic è l'argomento dei messaggi con cui si sveglia un'attesa o una release.
	chainTopic = "chain"
)

// Intervalli di lettura dello stato delle dipendenze e delle release: si parte da
// chainPollMin e si raddoppia fino a chainPollMax. Di norma l'attesa è svegliata prima da un
// messaggio di chi si conclude (notifyWaiters): la lettura periodica copre i casi senza
// messaggio (workflow vecchi, MAX_RECOVERY_ATTEMPTS_EXCEEDED, retention).
const (
	defaultChainPoll = 5 * time.Second
	chainPollMax     = 5 * time.Minute
)

// afterInput è l'input del workflow di attesa delle dipendenze.
type afterInput struct {
	DeploymentID string  `json:"deployment_id"`
	Request      Request `json:"request"`
	// QueueName è la coda dei deploy.
	QueueName string `json:"queue_name"`
}

// checkAfter verifica le dipendenze all'accodamento: devono esistere, non essere dry-run né
// concluse senza successo. Restituisce true se sono già tutte riuscite e non serve attendere.
func (e *Engine) checkAfter(ctx context.Context, d *descriptor.Descriptor) (bool, error) {
	if _, err := d.AfterWait(); err != nil {
		return false, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	done := true
	for _, id := range d.After {
		st, err := e.Status(ctx, id)
		switch {
		case errors.Is(err, ErrNotFound):
			return false, fmt.Errorf("%w: after: il deploy %s non esiste", ErrInvalidRequest, id)
		case err != nil:
			return false, err
		case st.DryRun:
			return false, fmt.Errorf("%w: after: %s è un dry-run", ErrInvalidRequest, id)
		case st.State == StateSuccess:
		case Terminal(st.State):
			return false, fmt.Errorf("%w: after: il deploy %s è concluso in %s", ErrInvalidRequest, id, st.State)
		default:
			done = false
		}
	}
	return done, nil
}

// enqueueAfter accoda l'attesa delle dipendenze del deploy id, sulla coda delle approvazioni:
// come un'approvazione può durare a lungo e non deve occupare uno slot dell'ambiente.
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

// afterWorkflow attende che tutte le dipendenze riescano. Poi accoda l'approvazione, se la
// regola la richiede, altrimenti il deploy, con l'ID già comunicato al richiedente. Se una
// dipendenza non riesce o l'attesa scade il deploy non viene eseguito.
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
			st.Status, st.Reason = service.AfterExpired, "attesa delle dipendenze scaduta"
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
	// L'esito si registra dopo l'accodamento: negli elenchi l'attesa soddisfatta è sostituita
	// dall'approvazione o dal deploy.
	attrs := maps.Clone(attributes(in.Request))
	attrs[attrOutcome] = st.Status
	if err := dbos.SetWorkflowAttributes(ctx, in.DeploymentID+afterSuffix, attrs); err != nil {
		return st, err
	}
	if st.Status != service.AfterSatisfied {
		// Il deploy non verrà eseguito: chi lo attende può concludersi subito.
		w.notifyWaiters(ctx, in.DeploymentID)
	}
	w.logger.Info("attesa delle dipendenze conclusa", "deployment", in.DeploymentID, "status", st.Status, "reason", st.Reason)
	w.recordAfter(ctx, in, st)
	return st, nil
}

// afterCheck è il risultato di una lettura delle dipendenze, persistito come step.
type afterCheck struct {
	Dependencies []DependencyState `json:"dependencies"`
	Done         bool              `json:"done"`
	// Failed spiega perché una dipendenza non potrà più riuscire.
	Failed string    `json:"failed,omitempty"`
	Now    time.Time `json:"now"`
}

// readDependencies legge lo stato delle dipendenze, con una sola query. Un errore di lettura
// lascia le dipendenze in attesa: verranno rilette al giro successivo.
func (w *Worker) readDependencies(_ context.Context, ids []string) afterCheck {
	c := afterCheck{Done: true, Now: time.Now().UTC()}
	states, err := w.reader.states(ids)
	if err != nil {
		w.logger.Warn("lettura dello stato delle dipendenze fallita", "dependencies", ids, "error", err)
	}
	for _, id := range ids {
		state := "UNKNOWN"
		st, ok := states[id]
		switch {
		case err != nil:
		case !ok:
			c.Failed = cmpString(c.Failed, "il deploy "+id+" non esiste più")
		case st.State == StateSuccess && !st.DryRun:
			state = st.State
		case Terminal(st.State):
			state = st.State
			c.Failed = cmpString(c.Failed, fmt.Sprintf("il deploy %s è concluso in %s", id, st.State))
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

// states legge con una sola query lo stato essenziale dei deploy ids (senza risultato, rollout
// né approvazioni): basta per seguire dipendenze e release. Un workflow che ha già registrato
// l'esito (attributo outcome) è considerato concluso anche se DBOS non lo ha ancora segnato:
// è il momento in cui avvisa chi lo attende. I deploy senza workflow mancano dalla mappa.
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

// settled applica a un workflow ancora in esecuzione l'esito che ha già registrato negli
// attributi (deploy, approvazione, attesa).
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

// waitChain attende un messaggio di chi si conclude (notifyWaiters) o lo scadere di d.
func waitChain(ctx dbos.Context, d time.Duration) error {
	_, err := dbos.Recv[string](ctx, chainTopic, d)
	if errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}

// waitersOf restituisce gli ID dei workflow di attesa e di release non conclusi che seguono il
// deploy id (attributo waits_for).
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

// wakeMessages sono i messaggi che svegliano i workflow ids.
func wakeMessages(id string, ids []string) []dbos.SendMessage {
	msgs := make([]dbos.SendMessage, len(ids))
	for i, dst := range ids {
		msgs[i] = dbos.SendMessage{DestinationID: dst, Message: id, Topic: chainTopic}
	}
	return msgs
}

// notifyWaiters sveglia, dal workflow di un deploy concluso, le attese e le release che lo
// seguono. Va chiamato dopo aver registrato l'esito negli attributi, che chi viene svegliato
// legge prima che DBOS segni il workflow come concluso. Un errore non ferma il workflow: chi
// attende rilegge comunque lo stato a intervalli.
func (w *Worker) notifyWaiters(ctx dbos.Context, id string) {
	ids, err := dbos.RunAsStep(ctx, func(context.Context) ([]string, error) {
		return waitersOf(w.dbos, id)
	}, dbos.WithStepName("waiters"))
	if err == nil && len(ids) > 0 {
		err = dbos.SendBulk(ctx, wakeMessages(id, ids))
	}
	if err != nil {
		w.logger.Warn("avviso a chi attende il deploy fallito", "deployment", id, "error", err)
	}
}

// notifyWaiters sveglia le attese e le release che seguono il deploy id, dopo un annullamento.
func (e *Engine) notifyWaiters(id string) {
	ids, err := waitersOf(e.client, id)
	if err == nil && len(ids) > 0 {
		err = dbos.SendBulk(e.client, wakeMessages(id, ids))
	}
	if err != nil {
		e.logger.Warn("avviso a chi attende il deploy fallito", "deployment", id, "error", err)
	}
}

// enqueueSatisfied accoda ciò che segue l'attesa: l'approvazione, se richiesta, o il deploy.
func (w *Worker) enqueueSatisfied(ctx dbos.Context, in afterInput) error {
	if needsApproval(in.Request) {
		return enqueueApproval(ctx, in.QueueName, in.Request, in.DeploymentID)
	}
	return w.enqueueApproved(ctx, approvalInput{DeploymentID: in.DeploymentID, Request: in.Request, QueueName: in.QueueName})
}

// recordAfter registra nell'audit l'esito dell'attesa, in uno step DBOS.
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
		w.logger.Error("registrazione dell'attesa nell'audit fallita", "deployment", in.DeploymentID, "error", err)
	}
}

// ActionAfterFinish è l'azione di audit con l'esito dell'attesa delle dipendenze.
const ActionAfterFinish = "after.finish"

// afterWorkflowState è lo stato esposto per un deploy che attende le dipendenze.
func afterWorkflowState(st dbos.WorkflowStatusType, outcome string) string {
	switch st {
	case dbos.WorkflowStatusPending, dbos.WorkflowStatusEnqueued, dbos.WorkflowStatusDelayed:
		return StateWaitingDependency
	case dbos.WorkflowStatusSuccess:
		if outcome == service.AfterSatisfied {
			// Approvazione o deploy stanno per essere accodati.
			return string(dbos.WorkflowStatusEnqueued)
		}
		return StateDependencyFailed
	}
	return string(st)
}

// afterState legge lo stato dell'attesa w (letta con l'output): il risultato se conclusa,
// altrimenti l'ultimo evento pubblicato.
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

// pendingStatus restituisce lo stato di un deploy che non ha ancora un workflow: in attesa
// delle dipendenze o di un'approvazione, oppure non eseguito per una delle due. Se esistono
// entrambe, l'approvazione (accodata dopo l'attesa) rappresenta il deploy.
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

// trimPendingSuffix restituisce l'ID del deploy da quello di un workflow di attesa o di approvazione.
func trimPendingSuffix(id string) string {
	return strings.TrimSuffix(strings.TrimSuffix(id, approvalSuffix), afterSuffix)
}
