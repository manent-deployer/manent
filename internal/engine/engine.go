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

// workflowName è il nome stabile con cui il workflow è registrato in DBOS.
// Non va cambiato: i workflow in corso vengono recuperati per nome.
const workflowName = "go-dart.deploy"

// ErrNotFound indica un deploy inesistente.
var ErrNotFound = errors.New("deploy non trovato")

// Service è il punto di ingresso usato da CLI e API.
type Service interface {
	// Plan calcola gli step che verrebbero eseguiti, senza avviare il deploy.
	Plan(d *descriptor.Descriptor) (pipeline.Plan, error)
	// Submit accoda un deploy e restituisce il suo ID.
	Submit(ctx context.Context, d *descriptor.Descriptor) (string, error)
	// Status restituisce lo stato corrente di un deploy.
	Status(ctx context.Context, id string) (*Status, error)
	// Wait attende la conclusione di un deploy e ne restituisce il risultato.
	Wait(ctx context.Context, id string) (*Result, error)
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

// Status descrive lo stato di un deploy.
type Status struct {
	ID        string    `json:"id"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Result    *Result   `json:"result,omitempty"`
	Error     string    `json:"error,omitempty"`
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
}

// Engine implementa Service sopra DBOS.
type Engine struct {
	dbos    dbos.Context
	queue   dbos.Queue
	planner pipeline.Planner
	steps   *pipeline.Registry
	logger  *slog.Logger
}

// New crea il contesto DBOS e registra il workflow. Va chiamato Launch prima dell'uso.
func New(opts Options) (*Engine, error) {
	ctx, err := dbos.NewContext(context.Background(), dbos.Config{
		AppName:            opts.AppName,
		ApplicationVersion: opts.AppVersion,
		DatabaseURL:        opts.DatabaseURL,
		Logger:             opts.Logger.With("component", "dbos"),
	})
	if err != nil {
		return nil, fmt.Errorf("inizializzazione DBOS: %w", err)
	}
	e := &Engine{dbos: ctx, planner: opts.Planner, steps: opts.Steps, logger: opts.Logger.With("component", "engine")}
	dbos.RegisterWorkflow(ctx, e.deployWorkflow, dbos.WithWorkflowName(workflowName))

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

// Submit calcola il piano e accoda il workflow. Se il descrittore ha un ID,
// questo è usato come ID del workflow e rende la richiesta idempotente.
func (e *Engine) Submit(_ context.Context, d *descriptor.Descriptor) (string, error) {
	plan, err := e.planner.Plan(d)
	if err != nil {
		return "", err
	}
	opts := []dbos.WorkflowOption{dbos.WithQueue(e.queue)}
	if d.ID != "" {
		opts = append(opts, dbos.WithWorkflowID(d.ID))
	}
	h, err := dbos.RunWorkflow(e.dbos, e.deployWorkflow, Request{Descriptor: *d, Plan: plan}, opts...)
	if err != nil {
		return "", err
	}
	e.logger.Info("deploy accodato", "deployment", h.GetWorkflowID(), "application", d.Application, "environment", d.Environment)
	return h.GetWorkflowID(), nil
}

// Status legge lo stato del workflow dal database di sistema DBOS.
func (e *Engine) Status(_ context.Context, id string) (*Status, error) {
	h, err := dbos.RetrieveWorkflow[Result](e.dbos, id)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	st, err := h.GetStatus()
	if err != nil {
		return nil, err
	}
	s := &Status{ID: id, State: string(st.Status), CreatedAt: st.CreatedAt, UpdatedAt: st.UpdatedAt}
	switch st.Status {
	case dbos.WorkflowStatusSuccess:
		res, err := h.GetResult()
		if err != nil {
			return nil, err
		}
		s.Result = &res
	case dbos.WorkflowStatusError:
		if st.Error != nil {
			s.Error = st.Error.Error()
		}
	}
	return s, nil
}

// Wait blocca fino alla conclusione del workflow.
func (e *Engine) Wait(_ context.Context, id string) (*Result, error) {
	h, err := dbos.RetrieveWorkflow[Result](e.dbos, id)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	res, err := h.GetResult()
	if err != nil {
		return nil, err
	}
	return &res, nil
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
