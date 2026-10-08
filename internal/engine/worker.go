package engine

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	// Driver SQLite per DBOS, usato con database_url "sqlite:...".
	_ "github.com/dbos-inc/dbos-transact-golang/dbos/driver/sqlite"

	"github.com/joeg-ita/go-dart/internal/pipeline"
)

// QueueOptions configura la coda partizionata dei deploy.
type QueueOptions struct {
	Name string
	// WorkerConcurrency limita i deploy in esecuzione su questo worker.
	WorkerConcurrency int
	// GlobalConcurrency limita i deploy in esecuzione su tutti i worker (0 = nessun limite).
	GlobalConcurrency int
	// PartitionConcurrency limita i deploy contemporanei della stessa application/environment su tutti i worker.
	PartitionConcurrency int
}

// WorkerOptions configura un worker.
type WorkerOptions struct {
	AppName     string
	AppVersion  string
	DatabaseURL string
	// ExecutorID deve essere univoco tra i worker e stabile tra i riavvii:
	// all'avvio il worker recupera solo i workflow rimasti in sospeso con questo ID.
	ExecutorID string
	Queue      QueueOptions
	Steps      *pipeline.Registry
	// Cleaner, se presente, libera le risorse locali al termine di ogni deploy.
	Cleaner pipeline.Cleaner
	Logger  *slog.Logger
	// SchedulePollInterval è ogni quanto DBOS ricarica le schedule dal database (default 30s).
	SchedulePollInterval time.Duration
}

// Worker esegue i workflow di deploy prelevandoli dalla coda condivisa.
// Più worker possono condividere lo stesso database di sistema (Postgres) per scalare orizzontalmente.
type Worker struct {
	dbos    dbos.Context
	queue   dbos.Queue
	steps   *pipeline.Registry
	cleaner pipeline.Cleaner
	logger  *slog.Logger
	id      string
}

// NewWorker crea il contesto DBOS, registra i workflow e la coda. Va chiamato Launch prima dell'uso.
func NewWorker(opts WorkerOptions) (*Worker, error) {
	if opts.ExecutorID == "" {
		return nil, fmt.Errorf("ExecutorID obbligatorio")
	}
	ctx, err := newContextWithRetry(dbos.Config{
		AppName:                  opts.AppName,
		ApplicationVersion:       opts.AppVersion,
		DatabaseURL:              opts.DatabaseURL,
		ExecutorID:               opts.ExecutorID,
		Logger:                   opts.Logger.With("component", "dbos"),
		SchedulerPollingInterval: opts.SchedulePollInterval,
	}, opts.Logger)
	if err != nil {
		return nil, fmt.Errorf("inizializzazione DBOS: %w", err)
	}
	w := &Worker{
		dbos:    ctx,
		steps:   opts.Steps,
		cleaner: opts.Cleaner,
		id:      dbos.GetExecutorID(ctx),
	}
	w.logger = opts.Logger.With("component", "worker", "executor", w.id)
	dbos.RegisterWorkflow(ctx, w.deployWorkflow, dbos.WithWorkflowName(workflowName))
	dbos.RegisterWorkflow(ctx, w.scheduledWorkflow, dbos.WithWorkflowName(scheduledWorkflowName))

	q := opts.Queue
	qopts := []dbos.QueueOption{
		dbos.WithWorkerConcurrency(q.WorkerConcurrency),
		dbos.WithPartitionConcurrency(q.PartitionConcurrency),
	}
	if q.GlobalConcurrency > 0 {
		qopts = append(qopts, dbos.WithGlobalConcurrency(q.GlobalConcurrency))
	}
	if w.queue, err = dbos.RegisterQueue(ctx, q.Name, qopts...); err != nil {
		return nil, fmt.Errorf("registrazione coda %q: %w", q.Name, err)
	}
	return w, nil
}

// initBackoff sono le attese tra i tentativi di inizializzazione di DBOS.
var initBackoff = []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second}

// newContextWithRetry crea il contesto DBOS riprovando in caso di errore: su un database
// vuoto più worker avviati insieme eseguono la migrazione dello schema in concorrenza
// e tutti tranne uno falliscono con "duplicate key" su CREATE SCHEMA.
func newContextWithRetry(cfg dbos.Config, log *slog.Logger) (dbos.Context, error) {
	for attempt := 0; ; attempt++ {
		ctx, err := dbos.NewContext(context.Background(), cfg)
		if err == nil || attempt == len(initBackoff) {
			return ctx, err
		}
		log.Warn("inizializzazione DBOS fallita, nuovo tentativo", "attempt", attempt+1, "error", err)
		time.Sleep(initBackoff[attempt])
	}
}

// Client restituisce il contesto DBOS del worker, utilizzabile come client per un Engine.
func (w *Worker) Client() dbos.Client { return w.dbos }

// ExecutorID restituisce l'ID effettivo del worker (DBOS__VMID ha la precedenza sulla configurazione).
func (w *Worker) ExecutorID() string { return w.id }

// Launch avvia DBOS: recupera i workflow in sospeso di questo executor e inizia a prelevare dalla coda.
func (w *Worker) Launch() error {
	if err := dbos.Launch(w.dbos); err != nil {
		return err
	}
	w.logger.Info("worker avviato", "queue", w.queue.GetName())
	return nil
}

// Shutdown arresta DBOS attendendo i workflow in corso fino al timeout.
// I workflow interrotti restano in sospeso e vengono ripresi al riavvio di un worker con lo stesso ExecutorID.
func (w *Worker) Shutdown(timeout time.Duration) error {
	return dbos.Shutdown(w.dbos, timeout)
}

// deployWorkflow esegue gli step del piano in ordine, ciascuno come step DBOS,
// e al termine (con successo o errore) libera le risorse locali del deploy.
// Deve restare deterministico: tutta la logica con effetti esterni è dentro gli step.
func (w *Worker) deployWorkflow(ctx dbos.Context, req Request) (Result, error) {
	id, err := dbos.GetWorkflowID(ctx)
	if err != nil {
		return Result{}, err
	}
	res, runErr := w.runPlan(ctx, id, req)
	if w.cleaner != nil {
		if _, err := dbos.RunAsStep(ctx, func(c context.Context) (string, error) {
			return "", w.cleaner.Cleanup(c, id)
		}, dbos.WithStepName("cleanup")); err != nil {
			w.logger.Warn("pulizia del deploy fallita", "deployment", id, "error", err)
		}
	}
	return res, runErr
}

// runPlan esegue gli step del piano fermandosi al primo errore.
func (w *Worker) runPlan(ctx dbos.Context, id string, req Request) (Result, error) {
	res := Result{DeploymentID: id}
	outputs := map[string]pipeline.Output{}
	for _, ps := range req.Plan {
		step, ok := w.steps.Get(ps.Name)
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
