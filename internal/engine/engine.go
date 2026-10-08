// Package engine orchestra la pipeline di deploy come workflow durevole DBOS.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	// Driver SQLite per DBOS, usato con database_url "sqlite:...".
	_ "github.com/dbos-inc/dbos-transact-golang/dbos/driver/sqlite"

	"github.com/joeg-ita/go-dart/internal/descriptor"
	"github.com/joeg-ita/go-dart/internal/pipeline"
)

// Nomi stabili con cui i workflow sono registrati in DBOS.
// Non vanno cambiati: i workflow in corso e le schedule li riferiscono per nome.
const (
	workflowName          = "go-dart.deploy"
	scheduledWorkflowName = "go-dart.scheduled-deploy"
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

// Options configura l'engine.
type Options struct {
	AppName     string
	AppVersion  string
	DatabaseURL string
	QueueName   string
	Concurrency int
	Planner     pipeline.Planner
	Steps       *pipeline.Registry
	Logger      *slog.Logger
	// SchedulePollInterval è ogni quanto DBOS ricarica le schedule dal database (default 30s).
	SchedulePollInterval time.Duration
	// Now restituisce l'ora corrente; sostituibile nei test.
	Now func() time.Time
}

// Engine implementa Service sopra DBOS.
type Engine struct {
	dbos    dbos.Context
	queue   dbos.Queue
	planner pipeline.Planner
	steps   *pipeline.Registry
	logger  *slog.Logger
	now     func() time.Time
}

// New crea il contesto DBOS, registra i workflow e la coda dei deploy. Va chiamato Launch prima dell'uso.
func New(opts Options) (*Engine, error) {
	ctx, err := dbos.NewContext(context.Background(), dbos.Config{
		AppName:                  opts.AppName,
		ApplicationVersion:       opts.AppVersion,
		DatabaseURL:              opts.DatabaseURL,
		Logger:                   opts.Logger.With("component", "dbos"),
		SchedulerPollingInterval: opts.SchedulePollInterval,
	})
	if err != nil {
		return nil, fmt.Errorf("inizializzazione DBOS: %w", err)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	e := &Engine{dbos: ctx, planner: opts.Planner, steps: opts.Steps, logger: opts.Logger.With("component", "engine"), now: opts.Now}
	dbos.RegisterWorkflow(ctx, e.deployWorkflow, dbos.WithWorkflowName(workflowName))
	dbos.RegisterWorkflow(ctx, e.scheduledWorkflow, dbos.WithWorkflowName(scheduledWorkflowName))

	qopts := []dbos.QueueOption{}
	if opts.Concurrency > 0 {
		qopts = append(qopts, dbos.WithWorkerConcurrency(opts.Concurrency))
	}
	if e.queue, err = dbos.RegisterQueue(ctx, opts.QueueName, qopts...); err != nil {
		return nil, fmt.Errorf("registrazione coda %q: %w", opts.QueueName, err)
	}
	return e, nil
}

// Launch avvia DBOS (recovery dei workflow interrotti, scheduler, coda).
func (e *Engine) Launch() error {
	return dbos.Launch(e.dbos)
}

// Shutdown arresta DBOS attendendo i workflow in corso fino al timeout.
func (e *Engine) Shutdown(timeout time.Duration) error {
	return dbos.Shutdown(e.dbos, timeout)
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

	opts := []dbos.WorkflowOption{dbos.WithQueue(e.queue)}
	if d.ID != "" {
		opts = append(opts, dbos.WithWorkflowID(d.ID))
	}
	sub := &Submission{State: string(dbos.WorkflowStatusEnqueued)}
	if d.Schedule != nil && d.Schedule.At != nil {
		delay := d.Schedule.At.Sub(e.now())
		if delay <= 0 {
			return nil, fmt.Errorf("%w: schedule.at %s è nel passato", ErrInvalidRequest, d.Schedule.At.Format(time.RFC3339))
		}
		opts = append(opts, dbos.WithDelay(delay))
		sub.State = string(dbos.WorkflowStatusDelayed)
		sub.ScheduledAt = d.Schedule.At
	}
	h, err := dbos.RunWorkflow(e.dbos, e.deployWorkflow, req, opts...)
	if err != nil {
		return nil, err
	}
	sub.ID = h.GetWorkflowID()
	e.logger.Info("deploy accodato", "deployment", sub.ID, "application", d.Application, "environment", d.Environment, "state", sub.State)
	return sub, nil
}

// Status legge lo stato del workflow dal database di sistema DBOS.
func (e *Engine) Status(_ context.Context, id string) (*Status, error) {
	h, err := dbos.RetrieveWorkflow[Result](e.dbos, id)
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
	wfs, err := dbos.ListWorkflows(e.dbos, opts...)
	if err != nil {
		return nil, err
	}
	out := make([]Status, len(wfs))
	for i, w := range wfs {
		out[i] = toStatus(w)
	}
	return out, nil
}

// Wait blocca fino alla conclusione del workflow.
func (e *Engine) Wait(_ context.Context, id string) (*Result, error) {
	h, err := dbos.RetrieveWorkflow[Result](e.dbos, id)
	if err != nil {
		return nil, fmt.Errorf("%w: deploy %s", ErrNotFound, id)
	}
	res, err := h.GetResult()
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
	if err := dbos.CancelWorkflow(e.dbos, id); err != nil {
		return err
	}
	e.logger.Info("deploy annullato", "deployment", id)
	return nil
}

// toStatus converte lo stato DBOS nel formato esposto da go-dart.
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

// deployWorkflow esegue gli step del piano in ordine, ciascuno come step DBOS.
// Deve restare deterministico: tutta la logica con effetti esterni è dentro gli step.
func (e *Engine) deployWorkflow(ctx dbos.Context, req Request) (Result, error) {
	id, err := dbos.GetWorkflowID(ctx)
	if err != nil {
		return Result{}, err
	}
	res := Result{DeploymentID: id}
	outputs := map[string]pipeline.Output{}

	for _, ps := range req.Plan {
		step, ok := e.steps.Get(ps.Name)
		if !ok {
			return res, fmt.Errorf("step %q non implementato", ps.Name)
		}
		in := pipeline.Input{DeploymentID: id, Descriptor: &req.Descriptor, Outputs: maps.Clone(outputs)}
		out, err := dbos.RunAsStep(ctx,
			func(c context.Context) (pipeline.Output, error) { return step.Run(c, in) },
			dbos.WithStepName(ps.Name),
			dbos.WithStepMaxRetries(ps.MaxRetries),
		)
		if err != nil {
			return res, fmt.Errorf("step %q: %w", ps.Name, err)
		}
		outputs[ps.Name] = out
		res.Steps = append(res.Steps, StepResult{Name: ps.Name, Output: out})
	}
	return res, nil
}
