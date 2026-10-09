// Package engine orchestra la pipeline di deploy come workflow durevole DBOS.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/pipeline"
)

// Nomi stabili con cui i workflow sono registrati in DBOS.
// Non vanno cambiati: i workflow in corso e le schedule li riferiscono per nome.
const (
	workflowName          = "manent.deploy"
	scheduledWorkflowName = "manent.scheduled-deploy"
)

// Stati di una Submission oltre a quelli DBOS.
const (
	// StateScheduled indica una schedule ricorrente creata o aggiornata.
	StateScheduled = "SCHEDULED"
)

var (
	// ErrNotFound indica un deploy o una schedule inesistente.
	ErrNotFound = errors.New("non trovato")
	// ErrInvalidRequest indica una richiesta non accettabile (es. orario nel passato).
	ErrInvalidRequest = errors.New("richiesta non valida")
)

// DeploymentService gestisce i deploy, immediati o pianificati.
type DeploymentService interface {
	// Plan calcola gli step che verrebbero eseguiti, senza avviare il deploy.
	Plan(d *descriptor.Descriptor) (pipeline.Plan, error)
	// Submit accoda un deploy, lo pianifica a un orario o crea una schedule ricorrente.
	Submit(ctx context.Context, d *descriptor.Descriptor) (*Submission, error)
	// Status restituisce lo stato corrente di un deploy.
	Status(ctx context.Context, id string) (*Status, error)
	// List elenca i deploy più recenti, opzionalmente solo quelli di una schedule.
	List(ctx context.Context, f ListFilter) ([]Status, error)
	// Wait attende la conclusione di un deploy e ne restituisce il risultato.
	Wait(ctx context.Context, id string) (*Result, error)
	// Cancel annulla un deploy in attesa o in esecuzione.
	Cancel(ctx context.Context, id string) error
}

// Service è il punto di ingresso usato da CLI e API.
type Service interface {
	DeploymentService
	ScheduleService
}

// Request è l'input del workflow, persistito da DBOS.
type Request struct {
	Descriptor descriptor.Descriptor `json:"descriptor"`
	Plan       pipeline.Plan         `json:"plan"`
}

// StepResult è l'esito di uno step eseguito.
type StepResult struct {
	Name   string          `json:"name"`
	Output pipeline.Output `json:"output"`
}

// Result è l'output del workflow di deploy.
type Result struct {
	DeploymentID string       `json:"deployment_id"`
	Steps        []StepResult `json:"steps"`
}

// Submission descrive l'esito di Submit.
type Submission struct {
	// ID del deploy; vuoto per le schedule ricorrenti.
	ID string `json:"id,omitempty"`
	// State è ENQUEUED, DELAYED (deploy singolo pianificato) o SCHEDULED (ricorrente).
	State       string     `json:"state"`
	ScheduledAt *time.Time `json:"scheduled_at,omitempty"`
	Schedule    string     `json:"schedule,omitempty"`
}

// Status descrive lo stato di un deploy.
type Status struct {
	ID          string     `json:"id"`
	State       string     `json:"state"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	ScheduledAt *time.Time `json:"scheduled_at,omitempty"`
	Result      *Result    `json:"result,omitempty"`
	Error       string     `json:"error,omitempty"`
}

// ListFilter filtra l'elenco dei deploy.
type ListFilter struct {
	Schedule string
	Limit    int
}

// ServiceOptions configura il servizio.
type ServiceOptions struct {
	// Client è il client DBOS: il contesto di un Worker oppure un client standalone (dbos.NewClient).
	Client    dbos.Client
	QueueName string
	Planner   pipeline.Planner
	Logger    *slog.Logger
	// Now restituisce l'ora corrente; sostituibile nei test.
	Now func() time.Time
}

// Engine implementa Service usando solo le API client di DBOS:
// accoda e interroga i workflow ma non li esegue (lo fanno i Worker).
type Engine struct {
	client    dbos.Client
	queueName string
	planner   pipeline.Planner
	logger    *slog.Logger
	now       func() time.Time
}

// NewService crea il servizio sopra un client DBOS.
func NewService(opts ServiceOptions) *Engine {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Engine{
		client:    opts.Client,
		queueName: opts.QueueName,
		planner:   opts.Planner,
		logger:    opts.Logger.With("component", "engine"),
		now:       opts.Now,
	}
}

// Plan delega al planner configurato.
func (e *Engine) Plan(d *descriptor.Descriptor) (pipeline.Plan, error) {
	return e.planner.Plan(d)
}

// Submit calcola il piano e, in base a schedule nel descrittore:
//   - nessuna schedule: accoda subito il deploy;
//   - schedule.at: accoda il deploy in stato DELAYED fino all'orario indicato;
//   - schedule.cron: crea o aggiorna la schedule ricorrente.
//
// Se il descrittore ha un ID, questo è usato come ID del workflow e rende la richiesta idempotente.
func (e *Engine) Submit(_ context.Context, d *descriptor.Descriptor) (*Submission, error) {
	plan, err := e.planner.Plan(d)
	if err != nil {
		return nil, err
	}
	req := Request{Descriptor: *d, Plan: plan}
	if d.Schedule != nil && d.Schedule.Cron != "" {
		return e.applySchedule(req)
	}

	opts := []dbos.EnqueueOption{dbos.WithEnqueueQueuePartitionKey(partitionKey(d))}
	if d.ID != "" {
		opts = append(opts, dbos.WithEnqueueWorkflowID(d.ID))
	}
	sub := &Submission{State: string(dbos.WorkflowStatusEnqueued)}
	if d.Schedule != nil && d.Schedule.At != nil {
		delay := d.Schedule.At.Sub(e.now())
		if delay <= 0 {
			return nil, fmt.Errorf("%w: schedule.at %s è nel passato", ErrInvalidRequest, d.Schedule.At.Format(time.RFC3339))
		}
		opts = append(opts, dbos.WithEnqueueDelay(delay))
		sub.State = string(dbos.WorkflowStatusDelayed)
		sub.ScheduledAt = d.Schedule.At
	}
	h, err := dbos.Enqueue[Result](e.client, e.queueName, workflowName, req, opts...)
	if err != nil {
		return nil, err
	}
	sub.ID = h.GetWorkflowID()
	e.logger.Info("deploy accodato", "deployment", sub.ID, "application", d.Application, "environment", d.Environment, "state", sub.State)
	return sub, nil
}

// Status legge lo stato del workflow dal database di sistema DBOS.
func (e *Engine) Status(_ context.Context, id string) (*Status, error) {
	h, err := dbos.RetrieveWorkflow[Result](e.client, id)
	if err != nil {
		return nil, fmt.Errorf("%w: deploy %s", ErrNotFound, id)
	}
	st, err := h.GetStatus()
	if err != nil {
		return nil, err
	}
	s := toStatus(st)
	if st.Status == dbos.WorkflowStatusSuccess {
		res, err := h.GetResult()
		if err != nil {
			return nil, err
		}
		s.Result = &res
	}
	return &s, nil
}

// List elenca i deploy dal più recente. Con f.Schedule restituisce solo le esecuzioni di quella schedule.
func (e *Engine) List(_ context.Context, f ListFilter) ([]Status, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	opts := []dbos.ListWorkflowsOption{
		dbos.WithFilterName(workflowName),
		dbos.WithFilterSortDesc(),
		dbos.WithFilterLimit(f.Limit),
		dbos.WithFilterLoadInput(false),
		dbos.WithFilterLoadOutput(false),
	}
	if f.Schedule != "" {
		opts = append(opts, dbos.WithFilterWorkflowIDPrefix(scheduledIDPrefix(f.Schedule)))
	}
	wfs, err := dbos.ListWorkflows(e.client, opts...)
	if err != nil {
		return nil, err
	}
	out := make([]Status, len(wfs))
	for i, w := range wfs {
		out[i] = toStatus(w)
	}
	return out, nil
}

// Wait blocca fino alla conclusione del workflow, o fino alla scadenza di ctx se ne ha una.
// Il deploy viene eseguito da un Worker: senza worker attivi resta in coda.
func (e *Engine) Wait(ctx context.Context, id string) (*Result, error) {
	h, err := dbos.RetrieveWorkflow[Result](e.client, id)
	if err != nil {
		return nil, fmt.Errorf("%w: deploy %s", ErrNotFound, id)
	}
	var opts []dbos.GetResultOption
	if deadline, ok := ctx.Deadline(); ok {
		opts = append(opts, dbos.WithHandleTimeout(time.Until(deadline)))
	}
	res, err := h.GetResult(opts...)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// Cancel annulla il deploy: se è in attesa viene rimosso dalla coda,
// se è in esecuzione si interrompe all'inizio dello step successivo.
func (e *Engine) Cancel(ctx context.Context, id string) error {
	if _, err := e.Status(ctx, id); err != nil {
		return err
	}
	if err := dbos.CancelWorkflow(e.client, id); err != nil {
		return err
	}
	e.logger.Info("deploy annullato", "deployment", id)
	return nil
}

// toStatus converte lo stato DBOS nel formato esposto da manent.
func toStatus(st dbos.WorkflowStatus) Status {
	s := Status{ID: st.ID, State: string(st.Status), CreatedAt: st.CreatedAt, UpdatedAt: st.UpdatedAt}
	if !st.DelayUntil.IsZero() {
		at := st.DelayUntil
		s.ScheduledAt = &at
	}
	if st.Status == dbos.WorkflowStatusError && st.Error != nil {
		s.Error = st.Error.Error()
	}
	return s
}

// partitionKey è la chiave di partizione della coda: i deploy della stessa
// application/environment rispettano il limite environment_concurrency su tutti i worker.
func partitionKey(d *descriptor.Descriptor) string {
	return d.Application + "/" + d.Environment
}
