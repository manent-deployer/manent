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
	// releaseWorkflowName segue una release composta. Il nome non va cambiato: DBOS lo usa
	// per il recovery.
	releaseWorkflowName = "manent.release"
	// releaseEvent è la chiave dell'evento con lo stato della release.
	releaseEvent = "release"
	// releaseRollbackInfix compone l'ID dei rollback di una release: "<release>.rollback.<nome>".
	releaseRollbackInfix = ".rollback."
)

// releaseInput è l'input del workflow della release: lo stato iniziale, con i deploy in
// ordine topologico.
type releaseInput struct {
	Release ReleaseStatus `json:"release"`
}

// SubmitRelease implementa ReleaseService. Prima verifica piano e policy di tutti i deploy,
// poi li accoda in ordine topologico, ognuno con le dipendenze (after) dai propri
// predecessori, e infine accoda il workflow che segue la release. Se un accodamento fallisce
// annulla i deploy già accodati. Con lo stesso ID restituisce la release esistente.
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
	descs := make([]*descriptor.Descriptor, len(order))
	for i, rd := range order {
		d := *rd.Descriptor
		d.ID = ids[rd.Name]
		after := make([]string, 0, len(rd.After)+len(d.After))
		for _, a := range rd.After {
			after = append(after, ids[a])
		}
		d.After = append(after, d.After...)
		if err := d.Validate(); err != nil {
			return nil, fmt.Errorf("%w: deploy %s: %w", ErrInvalidRequest, rd.Name, err)
		}
		if _, err := e.planner.Plan(&d); err != nil {
			return nil, fmt.Errorf("%w: deploy %s: %w", ErrInvalidRequest, rd.Name, err)
		}
		if err := e.check(ctx, &d); err != nil {
			return nil, fmt.Errorf("deploy %s: %w", rd.Name, err)
		}
		descs[i] = &d
		rs.Deploys = append(rs.Deploys, ReleaseDeployState{
			Name: rd.Name, DeploymentID: d.ID, Application: d.Application, Environment: d.Environment,
			Version: d.Artifact.Version, After: rd.After,
		})
	}
	var submitted []string
	for i, d := range descs {
		sub, err := e.submit(ctx, d, submitMeta{kind: KindDeploy, release: r.ID})
		if err != nil {
			for _, id := range submitted {
				if cerr := e.Cancel(ctx, id); cerr != nil {
					e.logger.Warn("release: annullamento di un deploy già accodato fallito", "release", r.ID, "deployment", id, "error", cerr)
				}
			}
			return nil, fmt.Errorf("deploy %s: %w", rs.Deploys[i].Name, err)
		}
		submitted = append(submitted, sub.ID)
		rs.Deploys[i].State = sub.State
	}
	opts := []dbos.EnqueueOption{
		dbos.WithEnqueueWorkflowID(r.ID),
		dbos.WithEnqueueAttributes(map[string]any{attrRelease: r.ID}),
	}
	if rs.RequestedBy != "" {
		opts = append(opts, dbos.WithEnqueueAuthenticatedUser(rs.RequestedBy))
	}
	if _, err := dbos.Enqueue[ReleaseStatus](e.client, approvalQueue(e.queueName), releaseWorkflowName, releaseInput{Release: *rs}, opts...); err != nil {
		return nil, err
	}
	e.logger.Info("release accodata", "release", r.ID, "deploys", len(rs.Deploys), "on_failure", rs.OnFailure, "requested_by", rs.RequestedBy)
	return rs, nil
}

// releaseCheck è una lettura dello stato dei deploy della release, persistita come step.
type releaseCheck struct {
	Deploys []ReleaseDeployState `json:"deploys"`
	// Pending sono i deploy non ancora partiti, che stop e rollback annullano.
	Pending []string `json:"pending,omitempty"`
}

// releaseWorkflow segue i deploy della release fino alla loro conclusione. I deploy partono
// da soli (ognuno attende i propri predecessori); il workflow applica onFailure: con stop e
// rollback annulla quelli non ancora partiti, con rollback riporta poi i deploy riusciti alla
// versione precedente, dal più dipendente al meno dipendente.
func (w *Worker) releaseWorkflow(ctx dbos.Context, in releaseInput) (ReleaseStatus, error) {
	rs := in.Release
	log := w.logger.With("release", rs.ID)
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
		if _, err := dbos.Sleep(ctx, poll); err != nil {
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

// readRelease legge lo stato dei deploy della release. Un errore di lettura lascia il deploy
// nello stato precedente.
func (w *Worker) readRelease(ctx context.Context, deploys []ReleaseDeployState) releaseCheck {
	c := releaseCheck{Deploys: slices.Clone(deploys)}
	for i := range c.Deploys {
		d := &c.Deploys[i]
		st, err := w.reader.Status(ctx, d.DeploymentID)
		if err != nil {
			w.logger.Warn("release: lettura dello stato di un deploy fallita", "deployment", d.DeploymentID, "error", err)
			continue
		}
		d.State, d.Error = st.State, st.Error
		if notStarted(st) {
			c.Pending = append(c.Pending, d.DeploymentID)
		}
	}
	return c
}

// notStarted indica un deploy che non ha ancora iniziato a modificare i target.
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

// releaseError riassume i deploy non riusciti.
func releaseError(ds []ReleaseDeployState) string {
	var parts []string
	for _, d := range ds {
		if Terminal(d.State) && d.State != StateSuccess {
			parts = append(parts, d.Name+": "+d.State)
		}
	}
	return "deploy non riusciti: " + strings.Join(parts, ", ")
}

// rollbackRelease riporta i deploy riusciti alla versione precedente, in ordine inverso a
// quello topologico (prima chi dipende dagli altri). Ogni rollback è un rollback manuale con
// ID "<release>.rollback.<nome>", eseguito con l'identità di chi ha richiesto la release; ci
// si ferma al primo che non riesce, come per le compensazioni degli step.
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
				return "", errors.New("servizio non configurato sul worker")
			}
			c = auth.WithPrincipal(c, &auth.Principal{Method: method, Subject: subject})
			_, err := w.service.Rollback(c, RollbackRequest{
				Application: d.Application, Environment: d.Environment, Redeploy: service.Redeploy{ID: d.RollbackID},
			})
			return "", err
		}, dbos.WithStepName("rollback:"+d.Name))
		if err != nil {
			d.RollbackState, d.Error = string(dbos.WorkflowStatusError), "rollback non avviato: "+err.Error()
			log.Error("release: rollback non avviato", "deploy", d.Name, "error", err)
			return nil
		}
		d.RollbackState = string(dbos.WorkflowStatusEnqueued)
		for poll := w.chainPoll; !Terminal(d.RollbackState); poll = min(poll*2, chainPollMax) {
			if err := dbos.SetEvent(ctx, releaseEvent, rs); err != nil {
				return err
			}
			if _, err := dbos.Sleep(ctx, poll); err != nil {
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

// recordRelease registra nell'audit l'esito della release, in uno step DBOS.
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

// ActionReleaseFinish è l'azione di audit con l'esito di una release.
const ActionReleaseFinish = "release.finish"

// ReleaseStatus implementa ReleaseService: il risultato se la release è conclusa, altrimenti
// l'ultimo stato pubblicato o, prima della prima lettura, quello iniziale.
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
		return nil, fmt.Errorf("input della release %s non leggibile: %w", w.ID, err)
	}
	rs := in.Release
	if ev, err := dbos.GetEvent[ReleaseStatus](e.client, w.ID, releaseEvent, 0); err == nil && ev.ID != "" {
		rs = ev
	}
	rs.CreatedAt, rs.UpdatedAt = w.CreatedAt, w.UpdatedAt
	switch w.Status {
	case dbos.WorkflowStatusSuccess:
		// Lo stato finale (SUCCESS o ERROR) è nell'ultimo evento.
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

// maxReleases limita le release restituite da ListReleases.
const maxReleases = 100

// ListReleases implementa ReleaseService.
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

// CancelRelease implementa ReleaseService: annulla i deploy non conclusi (quelli in corso si
// fermano all'inizio dello step successivo) e il workflow della release.
func (e *Engine) CancelRelease(ctx context.Context, id string) error {
	rs, err := e.ReleaseStatus(ctx, id)
	if err != nil {
		return err
	}
	for _, d := range rs.Deploys {
		if st, err := e.Status(ctx, d.DeploymentID); err == nil && !Terminal(st.State) {
			if err := e.Cancel(ctx, d.DeploymentID); err != nil {
				return fmt.Errorf("deploy %s: %w", d.Name, err)
			}
		}
	}
	if err := dbos.CancelWorkflow(e.client, id); err != nil {
		return err
	}
	e.logger.Info("release annullata", "release", id)
	return nil
}

// decodeInput decodifica l'input di un workflow letto con ListWorkflows: con un client DBOS
// è la stringa JSON serializzata, nel processo che lo esegue può essere già decodificato.
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
