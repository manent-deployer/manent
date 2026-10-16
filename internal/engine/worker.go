package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strconv"
	"sync"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	// SQLite driver for DBOS, used with database_url "sqlite:...".
	_ "github.com/dbos-inc/dbos-transact-golang/dbos/driver/sqlite"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/joeg-ita/manent/internal/audit"
	"github.com/joeg-ita/manent/internal/callback"
	"github.com/joeg-ita/manent/internal/change"
	"github.com/joeg-ita/manent/internal/chat"
	"github.com/joeg-ita/manent/internal/cluster"
	"github.com/joeg-ita/manent/internal/deploylog"
	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/governance"
	"github.com/joeg-ita/manent/internal/pipeline"
	"github.com/joeg-ita/manent/internal/rollout"
	"github.com/joeg-ita/manent/internal/telemetry"
)

// QueueOptions configures the partitioned deploy queue.
type QueueOptions struct {
	Name string
	// WorkerConcurrency limits the deploys running on this worker.
	WorkerConcurrency int
	// GlobalConcurrency limits the deploys running on all workers (0 = no limit).
	GlobalConcurrency int
	// PartitionConcurrency limits the concurrent deploys of the same application/environment on all workers.
	PartitionConcurrency int
}

// WorkerOptions configures a worker.
type WorkerOptions struct {
	AppName     string
	AppVersion  string
	DatabaseURL string
	// ExecutorID must be unique across workers and stable across restarts: at startup the
	// worker recovers only the pending workflows with this ID.
	ExecutorID string
	Queue      QueueOptions
	Steps      *pipeline.Registry
	// Custom builds the custom steps of the plan.
	Custom CustomSteps
	// Resolver, if set, produces the effective descriptor from the environment configuration.
	Resolver ConfigResolver
	// ResolverRetries are the extra attempts of the config step (e.g. Consul unreachable).
	ResolverRetries int
	// Cleaner, if set, frees local resources at the end of every deploy.
	Cleaner pipeline.Cleaner
	// Notifier, if set, sends the events to the descriptor's callbacks.
	Notifier callback.Notifier
	// CallbackRetries are the extra attempts of every callback send.
	CallbackRetries int
	// Chat, if set, sends chat notifications (destinations chosen at submission).
	Chat ChatNotifier
	// Change, if set, is the change management system of the deploys that require it.
	Change change.Provider
	// ChangePoll is the initial interval for reading the change approval (default 1m).
	ChangePoll time.Duration
	// PublicURL is the web interface address, for links in tickets.
	PublicURL string
	// Rollback, if set, applies the rollback policy to the effective descriptor.
	Rollback RollbackPolicy
	// Audit, if set, records the final outcome of every deploy.
	Audit audit.Recorder
	// Governance, if set, blocks at startup the deploys during a freeze window.
	Governance Governance
	Logger     *slog.Logger
	// SchedulePollInterval is how often DBOS reloads schedules from the database (default 30s).
	SchedulePollInterval time.Duration
	// Metrics, if set, records duration and outcome of deploys and steps.
	Metrics telemetry.Recorder
	// Registry, if set, registers the worker (heartbeats) and enables automatic recovery of lost
	// nodes and history retention.
	Registry cluster.Registry
	// Build is the binary version, registered with the worker.
	Build string
	// Hostname is registered with the worker.
	Hostname    string
	Maintenance MaintenanceOptions
	// Health runs the health gate of wave rollouts.
	Health rollout.Checker
	// Traffic, if set, integrates load balancers and routers: batch draining (traffic.pool) and
	// percentage rollouts (strategy: traffic).
	Traffic TrafficOps
	// HealthTimeout limits every health gate read (default 30s).
	HealthTimeout time.Duration
	// Region is the worker's region: with RegionRouting it runs that region's rollout passes
	// (queue <queue>.<region>) besides deploys.
	Region string
	// RegionRouting runs rollout passes on the workers of the targets' region.
	RegionRouting bool
	// ChainPollInterval is the first interval for reading the state of dependencies and of a
	// release's deploys (default 5s); it doubles at every read up to 1 minute.
	ChainPollInterval time.Duration
}

// ConfigResolver fetches the environment configuration and merges it with the descriptor.
type ConfigResolver interface {
	Resolve(ctx context.Context, d descriptor.Descriptor) (descriptor.Descriptor, map[string]string, error)
}

// RollbackPolicy decides a deploy's automatic rollback, writing the decision to d.Rollback.
type RollbackPolicy interface {
	Apply(d descriptor.Descriptor) (descriptor.Descriptor, error)
	// Allowed reports whether the artifact type allows rollback (automatic or manual).
	Allowed(artifactType string) error
}

// resolvedConfig is the persisted output of the config step.
type resolvedConfig struct {
	Descriptor descriptor.Descriptor `json:"descriptor"`
	Meta       pipeline.Output       `json:"meta"`
}

// Worker runs the deploy workflows, dequeuing them from the shared queue.
// Several workers can share the same system database (Postgres) to scale horizontally.
type Worker struct {
	dbos            dbos.Context
	queue           dbos.Queue
	steps           *pipeline.Registry
	custom          CustomSteps
	resolver        ConfigResolver
	resolverRetries int
	cleaner         pipeline.Cleaner
	notifier        callback.Notifier
	callbackRetries int
	chat            ChatNotifier
	change          change.Provider
	changePoll      time.Duration
	publicURL       string
	rollback        RollbackPolicy
	audit           audit.Recorder
	governance      Governance
	health          rollout.Checker
	traffic         TrafficOps
	healthTimeout   time.Duration
	region          string
	regionRouting   bool
	queueName       string
	metrics         telemetry.Recorder
	registry        cluster.Registry
	admin           *Admin
	maintenance     MaintenanceOptions
	build, hostname string
	version         string
	startedAt       time.Time
	stop            chan struct{}
	stopOnce        sync.Once
	wg              sync.WaitGroup
	logger          *slog.Logger
	id              string
	// reader reads the state of deploys (dependencies, releases) on the worker's context.
	reader    *Engine
	chainPoll time.Duration
	// service is the full service used by releases for rollbacks (SetService).
	service Service
}

// NewWorker creates the DBOS context and registers workflows and queue. Call Launch before use.
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
		dbos:            ctx,
		steps:           opts.Steps,
		custom:          opts.Custom,
		resolver:        opts.Resolver,
		resolverRetries: opts.ResolverRetries,
		cleaner:         opts.Cleaner,
		notifier:        opts.Notifier,
		callbackRetries: opts.CallbackRetries,
		chat:            opts.Chat,
		change:          opts.Change,
		changePoll:      cmp.Or(opts.ChangePoll, time.Minute),
		publicURL:       opts.PublicURL,
		rollback:        opts.Rollback,
		audit:           opts.Audit,
		governance:      opts.Governance,
		health:          opts.Health,
		traffic:         opts.Traffic,
		healthTimeout:   cmp.Or(opts.HealthTimeout, 30*time.Second),
		region:          opts.Region,
		regionRouting:   opts.RegionRouting,
		queueName:       opts.Queue.Name,
		metrics:         opts.Metrics,
		registry:        opts.Registry,
		maintenance:     opts.Maintenance.withDefaults(),
		build:           opts.Build,
		hostname:        opts.Hostname,
		version:         dbos.GetApplicationVersion(ctx),
		stop:            make(chan struct{}),
		id:              dbos.GetExecutorID(ctx),
	}
	if w.metrics == nil {
		w.metrics = telemetry.Noop{}
	}
	w.logger = opts.Logger.With("component", "worker", "executor", w.id)
	w.admin = NewAdmin(AdminOptions{Client: ctx, Registry: opts.Registry, Version: w.version, Audit: opts.Audit, Logger: opts.Logger})
	w.reader = NewService(ServiceOptions{Client: ctx, QueueName: opts.Queue.Name, Logger: opts.Logger})
	w.chainPoll = cmp.Or(opts.ChainPollInterval, defaultChainPoll)
	dbos.RegisterWorkflow(ctx, w.deployWorkflow, dbos.WithWorkflowName(workflowName))
	dbos.RegisterWorkflow(ctx, w.scheduledWorkflow, dbos.WithWorkflowName(scheduledWorkflowName))
	dbos.RegisterWorkflow(ctx, w.approvalWorkflow, dbos.WithWorkflowName(approvalWorkflowName))
	dbos.RegisterWorkflow(ctx, w.passWorkflow, dbos.WithWorkflowName(passWorkflowName))
	dbos.RegisterWorkflow(ctx, w.gateWorkflow, dbos.WithWorkflowName(gateWorkflowName))
	dbos.RegisterWorkflow(ctx, w.afterWorkflow, dbos.WithWorkflowName(afterWorkflowName))
	dbos.RegisterWorkflow(ctx, w.releaseWorkflow, dbos.WithWorkflowName(releaseWorkflowName))
	dbos.RegisterWorkflow(ctx, w.retireWorkflow, dbos.WithWorkflowName(retireWorkflowName))

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
	if _, err = dbos.RegisterQueue(ctx, approvalQueue(q.Name)); err != nil {
		return nil, fmt.Errorf("registrazione coda %q: %w", approvalQueue(q.Name), err)
	}
	if w.region != "" {
		// The region's rollout passes: one at a time per application/environment/region.
		rq := regionQueue(q.Name, w.region)
		if _, err = dbos.RegisterQueue(ctx, rq, dbos.WithWorkerConcurrency(q.WorkerConcurrency),
			dbos.WithPartitionConcurrency(1)); err != nil {
			return nil, fmt.Errorf("registrazione coda %q: %w", rq, err)
		}
	}
	return w, nil
}

// initBackoff are the waits between DBOS initialization attempts.
var initBackoff = []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second}

// newContextWithRetry creates the DBOS context, retrying on error: on an empty database
// several workers started together run the schema migration concurrently and all but one
// fail with "duplicate key" on CREATE SCHEMA.
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

// Migrate creates or upgrades the DBOS schema of the database, with the same attempts as
// newContextWithRetry: it is for processes that use only a DBOS client (API, admin), which
// verify the schema but do not create it, when they start before the workers on a new database.
func Migrate(databaseURL string, log *slog.Logger) error {
	for attempt := 0; ; attempt++ {
		err := dbos.Migrate(context.Background(), databaseURL, dbos.WithMigrateLogger(log))
		if err == nil || attempt == len(initBackoff) {
			return err
		}
		log.Warn("migrazione dello schema DBOS fallita, nuovo tentativo", "attempt", attempt+1, "error", err)
		time.Sleep(initBackoff[attempt])
	}
}

// Client returns the worker's DBOS context, usable as client for an Engine.
func (w *Worker) Client() dbos.Client { return w.dbos }

// ExecutorID returns the worker's actual ID (DBOS__VMID takes precedence over the configuration).
func (w *Worker) ExecutorID() string { return w.id }

// Version returns the version of the workflows run by the worker.
func (w *Worker) Version() string { return w.version }

// SetService sets the full service (with planner and policies) used by releases to roll back
// successful deploys. Call it before Launch.
func (w *Worker) SetService(s Service) { w.service = s }

// Admin returns the platform operations on the worker's database.
func (w *Worker) Admin() *Admin { return w.admin }

// Launch registers the worker and starts DBOS: it recovers this executor's pending workflows
// and starts dequeuing. Registration precedes DBOS recovery: if another worker is recovering
// this executor's workflows (lost earlier), it waits until it is done, so no workflow is
// resumed twice.
func (w *Worker) Launch() error {
	if w.registry != nil {
		if err := w.register(); err != nil {
			return err
		}
	}
	if err := dbos.Launch(w.dbos); err != nil {
		return err
	}
	switch {
	case w.region != "":
		// A region worker runs deploys, approvals and only its region's passes.
		dbos.ListenQueues(w.dbos, w.queueName, approvalQueue(w.queueName), regionQueue(w.queueName, w.region))
	case w.regionRouting:
		// Without a region, with routing enabled: orchestration only, no passes.
		dbos.ListenQueues(w.dbos, w.queueName, approvalQueue(w.queueName))
	}
	if w.registry != nil {
		w.wg.Add(1)
		go w.maintain()
	}
	w.logger.Info("worker avviato", "queue", w.queue.GetName(), "version", w.version)
	return nil
}

// register records the worker's startup, waiting for a running recovery of its workflows to
// end (at most cluster.ClaimTimeout). If the same executor_id is in use by another live
// process, it waits for it to be declared lost (StaleAfter: the case of a restart after a
// crash) and then gives up: two processes with the same ID would resume each other's deploys.
func (w *Worker) register() error {
	w.startedAt = time.Now().UTC()
	e := cluster.Executor{ID: w.id, Version: w.version, Build: w.build, Hostname: w.hostname, Region: w.region, StartedAt: w.startedAt}
	deadline := time.Now().Add(cluster.ClaimTimeout + 10*time.Second)
	inUseDeadline := time.Now().Add(w.maintenance.StaleAfter + w.maintenance.HeartbeatInterval)
	warned := false
	for {
		ok, err := w.registry.Register(context.Background(), e, w.maintenance.StaleAfter)
		var inUse *cluster.InUseError
		switch {
		case errors.As(err, &inUse):
			if time.Now().After(inUseDeadline) {
				return fmt.Errorf("registrazione del worker: %w", err)
			}
			if !warned {
				w.logger.Warn("executor_id registrato da un altro processo non arrestato: attesa che risulti perso (se è ancora attivo, assegnare un dbos.executor_id diverso)",
					"executor", w.id, "host", inUse.Hostname, "heartbeat_at", inUse.HeartbeatAt)
				warned = true
			}
		case err != nil:
			return fmt.Errorf("registrazione del worker: %w", err)
		case ok:
			return nil
		case time.Now().After(deadline):
			return errors.New("registrazione del worker: recovery dei suoi workflow ancora in corso")
		default:
			w.logger.Warn("recovery dei workflow di questo executor in corso su un altro worker: attesa")
		}
		time.Sleep(min(2*time.Second, w.maintenance.HeartbeatInterval))
	}
}

// drainQueue is a nonexistent queue: listening to it means dequeuing no more deploys.
const drainQueue = "manent-draining"

// Drain stops dequeuing deploys and waits, up to timeout, for those running on this worker to
// end. Waiting approvals are not waited for: they resume on restart, or on another worker
// through recovery.
func (w *Worker) Drain(timeout time.Duration) {
	if timeout <= 0 {
		return
	}
	dbos.ListenQueues(w.dbos, drainQueue)
	deadline := time.Now().Add(timeout)
	for {
		wfs, err := dbos.ListWorkflows(w.dbos, dbos.WithFilterName(workflowName, passWorkflowName, gateWorkflowName), dbos.WithFilterExecutorIDs(w.id),
			dbos.WithFilterStatus(dbos.WorkflowStatusPending), dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false))
		switch {
		case err != nil:
			w.logger.Warn("drain: lettura dei deploy in corso fallita", "error", err)
			return
		case len(wfs) == 0:
			w.logger.Info("drain completato: nessun deploy in corso")
			return
		case time.Now().After(deadline):
			w.logger.Warn("drain: timeout, i deploy in corso riprenderanno al riavvio o con la recovery", "running", len(wfs))
			return
		}
		w.logger.Info("drain: attesa dei deploy in corso", "running", len(wfs))
		time.Sleep(time.Second)
	}
}

// Shutdown stops maintenance and DBOS, waiting for running workflows up to the timeout, and
// records the shutdown. Interrupted workflows stay pending and are resumed when a worker with
// the same ExecutorID restarts or, through recovery, by another worker.
func (w *Worker) Shutdown(timeout time.Duration) error {
	w.stopOnce.Do(func() { close(w.stop) })
	w.wg.Wait()
	err := dbos.Shutdown(w.dbos, timeout)
	if w.registry != nil && !w.startedAt.IsZero() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if serr := w.registry.Stopped(ctx, w.id, w.startedAt); serr != nil {
			w.logger.Warn("registrazione dell'arresto fallita", "error", serr)
		}
	}
	return err
}

// deployWorkflow resolves the environment configuration, runs the plan steps in order (each
// as a DBOS step) and at the end, on success or error, frees local resources and notifies
// the outcome to the callbacks.
// It must stay deterministic: all logic with external effects is inside steps.
func (w *Worker) deployWorkflow(ctx dbos.Context, req Request) (Result, error) {
	id, err := dbos.GetWorkflowID(ctx)
	if err != nil {
		return Result{}, err
	}
	d0 := req.Descriptor
	_, span := telemetry.Tracer().Start(telemetry.WithTraceParent(context.Background(), req.TraceParent), "deployment",
		trace.WithAttributes(
			attribute.String("manent.deployment.id", id), attribute.String("manent.application", d0.Application),
			attribute.String("manent.environment", d0.Environment), attribute.String("manent.kind", cmp.Or(req.Kind, KindDeploy)),
			attribute.String("manent.artifact.version", d0.Artifact.Version), attribute.Bool("manent.dry_run", d0.DryRun),
		))
	defer span.End()
	ev := &events{w: w, ctx: ctx, id: id, requestedBy: req.RequestedBy, span: span.SpanContext()}
	ev.send(&req.Descriptor, callback.Event{Type: descriptor.CallbackEventDeployment, Status: callback.StatusStarted})
	chatRefs := w.notifyChat(ctx, id, req.Notify, w.chatMessage(id, req, chat.EventStarted), nil)
	res, d, runErr := w.run(ctx, id, req, ev)
	if w.cleaner != nil {
		if _, err := dbos.RunAsStep(ctx, func(c context.Context) (string, error) {
			return "", w.cleaner.Cleanup(c, id)
		}, dbos.WithStepName("cleanup")); err != nil {
			w.logger.Warn("pulizia del deploy fallita", "deployment", id, "error", err)
		}
	}
	final := callback.Event{Type: descriptor.CallbackEventDeployment, Status: callback.StatusSucceeded}
	if runErr != nil {
		final.Status, final.Error = callback.StatusFailed, runErr.Error()
		if rb, ok := errors.AsType[*RollbackError](runErr); ok {
			final.Rollback = rb.Status
		}
	}
	ev.send(d, final)
	if res.Change != nil {
		w.closeChange(ctx, id, res.Change, final)
	}
	msg := w.chatMessage(id, req, chat.EventSucceeded)
	if runErr != nil {
		msg.Event, msg.Error, msg.Rollback = chat.EventFailed, runErr.Error(), final.Rollback
	}
	msg.Change = chatChange(res.Change)
	w.notifyChat(ctx, id, req.Notify, msg, chatRefs)
	w.recordOutcome(ctx, id, req.RequestedBy, d, final)
	w.recordAttributes(ctx, id, req, res, final)
	if !req.Descriptor.DryRun {
		w.notifyWaiters(ctx, id)
	}
	w.recordMetrics(id, req, final)
	span.SetAttributes(attribute.String("manent.outcome", final.Status))
	if runErr != nil {
		span.SetStatus(codes.Error, runErr.Error())
	}
	return res, runErr
}

// recordMetrics records outcome and duration of the deploy. Duration starts from the start
// on the worker (recorded by DBOS), so it excludes queueing and approvals. It is not a step:
// metrics belong to the process and must be neither repeated nor persisted.
func (w *Worker) recordMetrics(id string, req Request, final callback.Event) {
	var dur time.Duration
	// The root context, not the workflow's: the read does not become a step.
	if wfs, err := dbos.ListWorkflows(w.dbos, dbos.WithFilterWorkflowIDs(id), dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false)); err == nil && len(wfs) == 1 && !wfs[0].StartedAt.IsZero() {
		dur = time.Since(wfs[0].StartedAt)
	}
	d := req.Descriptor
	w.metrics.DeploymentFinished(d.Application, d.Environment, cmp.Or(req.Kind, KindDeploy), final.Status, d.DryRun, dur)
}

// recordAttributes adds to the workflow attributes the outcome, the sha256 of the released
// artifact and the outcome of a possible rollback, used by inventory and promotions.
func (w *Worker) recordAttributes(ctx dbos.Context, id string, req Request, res Result, final callback.Event) {
	attrs := attributes(req)
	attrs[attrOutcome] = final.Status
	if res.Rollout != nil {
		attrs[attrRolloutProgress] = rolloutProgress(res.Rollout)
	}
	if final.Rollback != "" {
		attrs[attrRollback] = final.Rollback
	}
	for _, s := range res.Steps {
		if v := s.Output[outSHA256]; s.Name == stepArtifacts && v != "" {
			attrs[attrSHA256] = v
		}
	}
	if res.Change != nil {
		attrs[attrChange], attrs[attrChangeURL] = res.Change.Number, res.Change.URL
	}
	if err := dbos.SetWorkflowAttributes(ctx, id, attrs); err != nil {
		w.logger.Error("aggiornamento degli attributi del deploy fallito", "deployment", id, "error", err)
	}
}

// Step and output the released artifact's sha256 is read from (internal/steps).
const (
	stepArtifacts = "artifacts"
	outSHA256     = "sha256"
)

// recordOutcome records the final deploy outcome in the audit, in a DBOS step: after a
// recovery the entry is not repeated.
func (w *Worker) recordOutcome(ctx dbos.Context, id, requestedBy string, d *descriptor.Descriptor, final callback.Event) {
	if w.audit == nil {
		return
	}
	e := audit.Entry{
		Actor: requestedBy, Action: ActionDeploymentFinish, Outcome: audit.OutcomeSucceeded,
		Application: d.Application, Environment: d.Environment, Deployment: id,
		DryRun: d.DryRun, Rollback: final.Rollback, Reason: final.Error,
	}
	if final.Status == callback.StatusFailed {
		e.Outcome = audit.OutcomeFailed
	}
	if _, err := dbos.RunAsStep(ctx, func(c context.Context) (string, error) {
		e.Time = time.Now().UTC()
		return "", w.audit.Record(c, e)
	}, dbos.WithStepName("audit")); err != nil {
		w.logger.Error("registrazione dell'esito nell'audit fallita", "deployment", id, "error", err)
	}
}

// ActionDeploymentFinish is the audit action with the final outcome of a deploy.
const ActionDeploymentFinish = "deployment.finish"

// events sends a deploy's events to the descriptor's callbacks, each as a DBOS step: after a
// recovery the sends already completed are not repeated. Numbering depends only on the
// workflow input, so it stays deterministic.
type events struct {
	w           *Worker
	ctx         dbos.Context
	id          string
	requestedBy string
	seq         int
	// span is the deploy span, parent of the step spans.
	span trace.SpanContext
	// silent disables sending: in pass workflows the orchestrator sends the events.
	silent bool
}

// send notifies the event to the interested callbacks. A send failed after the retries is
// logged but does not fail the deploy.
func (e *events) send(d *descriptor.Descriptor, ev callback.Event) {
	if e.silent || e.w.notifier == nil || len(d.Callbacks) == 0 {
		return
	}
	e.seq++
	ev.Sequence = e.seq
	ev.ID = fmt.Sprintf("%s:%d", e.id, e.seq)
	ev.DeploymentID, ev.Application, ev.Environment, ev.DryRun = e.id, d.Application, d.Environment, d.DryRun
	ev.RequestedBy = e.requestedBy
	for i, cb := range d.Callbacks {
		if !cb.Wants(ev.Type) {
			continue
		}
		_, err := dbos.RunAsStep(e.ctx, func(c context.Context) (string, error) {
			out := ev
			out.Timestamp = time.Now().UTC()
			return "", e.w.notifier.Notify(c, d, cb, out)
		}, dbos.WithStepName(fmt.Sprintf("callback:%s:%d", ev.Type, i)), dbos.WithStepMaxRetries(e.w.callbackRetries))
		if err != nil {
			e.w.logger.Warn("callback non consegnato", "deployment", e.id, "delivery", ev.ID, "callback", i, "error", err)
		}
	}
}

// stepEvent builds the event with the outcome of a step.
func stepEvent(name string, out pipeline.Output, err error) callback.Event {
	ev := callback.Event{Type: descriptor.CallbackEventStep, Step: name, Status: callback.StatusSucceeded, Output: out}
	if err != nil {
		ev.Status, ev.Error = callback.StatusFailed, err.Error()
	}
	return ev
}

// run runs the config step and then the plan steps, stopping at the first error.
// The environment configuration is read in a DBOS step: it is the one valid at execution time
// (scheduled deploys included) and, once recorded, stays the same even if the deploy is
// resumed after a crash. The same step applies the rollback policy, so the rollback decision
// also stays the recorded one.
// In dry-run every step runs DryRun, if it implements it; otherwise it is skipped, so a new
// step never has effects by mistake.
// With rollback enabled, before every pipeline.Compensator its checkpoint runs and, if a step
// listed in rollback.on fails, the steps already started are compensated.
// It also returns the effective descriptor, used for the final event.
func (w *Worker) run(ctx dbos.Context, id string, req Request, ev *events) (Result, *descriptor.Descriptor, error) {
	dryRun := req.Descriptor.DryRun
	res := Result{DeploymentID: id, DryRun: dryRun}
	if w.resolver != nil || w.rollback != nil || w.governance != nil {
		rc, err := dbos.RunAsStep(ctx, func(c context.Context) (resolvedConfig, error) {
			return instrument(w, c, ev, "config", func(c context.Context) (resolvedConfig, error) {
				return w.resolveConfig(c, req.Descriptor, req.Approval)
			})
		}, dbos.WithStepName("config"), dbos.WithStepMaxRetries(w.resolverRetries), dbos.WithStepRetryPredicate(pipeline.Retryable))
		ev.send(&req.Descriptor, stepEvent("config", rc.Meta, err))
		if err != nil {
			return res, &req.Descriptor, fmt.Errorf("step %q: %w", "config", err)
		}
		req.Descriptor = rc.Descriptor
		res.Steps = append(res.Steps, StepResult{Name: "config", Output: rc.Meta})
	}
	d := &req.Descriptor
	if req.Change != nil && !d.DryRun {
		t, err := w.openChange(ctx, id, req)
		if err != nil {
			return res, d, err
		}
		res.Change = t
	}
	if d.Rollout != nil {
		return res, d, w.runRollout(ctx, id, req, d, ev, &res)
	}
	outputs := map[string]pipeline.Output{}
	var started []startedStep
	for _, ps := range req.Plan {
		out, st, skipped, err := w.execStep(ctx, id, ev, d, ps, ps.Name, nil, outputs, &res)
		if st != nil {
			started = append(started, *st)
		}
		if err != nil {
			return res, d, w.compensate(ctx, id, ev, d, &res, started, ps.Name, err)
		}
		if !skipped {
			outputs[ps.Name] = out
		}
	}
	return res, d, nil
}

// execStep runs a plan step as a DBOS step and, if rollback is enabled and the step is
// compensable, its checkpoint first. label is the DBOS step name: in a rollout it tells apart
// the runs of the same step in different passes and batches ("deploy@eu-1/eu-1a#0");
// targets, if not nil, replaces the descriptor targets. It returns the started step to
// compensate on rollback (even if it failed: it may have changed part of the targets);
// skipped means a step skipped in dry-run.
func (w *Worker) execStep(ctx dbos.Context, id string, ev *events, d *descriptor.Descriptor, ps pipeline.PlannedStep, label string,
	targets []descriptor.Target, outputs map[string]pipeline.Output, res *Result) (out pipeline.Output, started *startedStep, skipped bool, err error) {
	step, err := w.stepFor(ps, ev.requestedBy)
	if err != nil {
		return nil, nil, false, err
	}
	exec := step.Run
	if d.DryRun {
		dr, ok := step.(pipeline.DryRunner)
		if !ok {
			w.logger.Info("dry-run: step saltato", "deployment", id, "step", label)
			out := pipeline.Output{"dry_run": "skipped"}
			res.Steps = append(res.Steps, StepResult{Name: label, Output: out})
			ev.send(d, callback.Event{Type: descriptor.CallbackEventStep, Step: label, Status: callback.StatusSkipped, Output: out})
			return nil, nil, true, nil
		}
		exec = dr.DryRun
	}
	rec := startedStep{Step: ps, Label: label, Targets: targets, Outputs: maps.Clone(outputs)}
	in := rec.input(id, d)
	_, compensable := step.(pipeline.Compensator)
	compensable = compensable && d.Rollback.IsEnabled()
	if ck, ok := step.(pipeline.Checkpointer); ok && compensable {
		name := checkpointPrefix + label
		cp, err := dbos.RunAsStep(ctx,
			func(c context.Context) (pipeline.Output, error) {
				return instrument(w, c, ev, name, func(c context.Context) (pipeline.Output, error) { return ck.Checkpoint(c, in) })
			},
			dbos.WithStepName(name),
			dbos.WithStepMaxRetries(ps.MaxRetries),
			dbos.WithStepRetryPredicate(pipeline.Retryable),
		)
		ev.send(d, stepEvent(name, cp, err))
		if err != nil {
			// The step did not start: there is nothing to compensate.
			return nil, nil, false, fmt.Errorf("step %q: %w", name, err)
		}
		rec.Checkpoint = cp
		res.Steps = append(res.Steps, StepResult{Name: name, Output: cp})
	}
	out, err = dbos.RunAsStep(ctx,
		func(c context.Context) (pipeline.Output, error) {
			return instrument(w, c, ev, label, func(c context.Context) (pipeline.Output, error) { return exec(c, in) })
		},
		dbos.WithStepName(label),
		dbos.WithStepMaxRetries(ps.MaxRetries),
		dbos.WithStepRetryPredicate(pipeline.Retryable),
	)
	// Custom step outputs do not go to the callbacks, defined by the descriptor: they stay in the
	// deploy state, visible with read permissions.
	if ps.IsCustom() {
		ev.send(d, stepEvent(label, nil, err))
	} else {
		ev.send(d, stepEvent(label, out, err))
	}
	if compensable {
		started = &rec
	}
	if err != nil {
		return nil, started, false, fmt.Errorf("step %q: %w", label, err)
	}
	res.Steps = append(res.Steps, StepResult{Name: label, Output: out})
	return out, started, false, nil
}

// instrument runs fn as the body of a DBOS step: logs emitted with the step context go to
// the deploy log (internal/deploylog) and every attempt is recorded in the metrics and in a
// span child of the deploy's. Steps already ended, read again after a recovery, do not run
// again and so are not repeated.
func instrument[R any](w *Worker, c context.Context, ev *events, name string, fn func(context.Context) (R, error)) (R, error) {
	c = deploylog.WithStep(c, name)
	c, span := telemetry.Tracer().Start(trace.ContextWithSpanContext(c, ev.span), "step "+name,
		trace.WithAttributes(attribute.String("manent.step", name), attribute.String("manent.deployment.id", ev.id)))
	defer span.End()
	start := time.Now()
	out, err := fn(c)
	outcome := "succeeded"
	if err != nil {
		outcome = "failed"
		span.SetStatus(codes.Error, err.Error())
	}
	w.metrics.StepAttempt(name, outcome, time.Since(start))
	return out, err
}

// resolveConfig produces the effective descriptor: environment configuration and rollback
// policy. A real deploy that starts during a freeze window fails, unless overridden; so does
// one with approvals moved into the rollout, if the effective rollout has none.
func (w *Worker) resolveConfig(ctx context.Context, d descriptor.Descriptor, approvals *governance.Approvals) (resolvedConfig, error) {
	meta := map[string]string{}
	if w.governance != nil && !d.DryRun {
		if d.FreezeOverride != "" {
			meta["freeze_override"] = d.FreezeOverride
		} else if win := w.governance.Frozen(d.Application, d.Environment, time.Now()); win != nil {
			return resolvedConfig{}, pipeline.Permanent(errors.New(freezeMessage(win)))
		}
	}
	if w.resolver != nil {
		var err error
		if d, meta, err = w.resolver.Resolve(ctx, d); err != nil {
			return resolvedConfig{}, err
		}
	}
	if w.rollback != nil {
		var err error
		if d, err = w.rollback.Apply(d); err != nil {
			return resolvedConfig{}, pipeline.Permanent(err)
		}
		meta["rollback"] = "disabled"
		if d.Rollback.IsEnabled() {
			meta["rollback"] = "enabled"
		}
	}
	if d.Traffic != nil && d.Traffic.Pool != nil && d.Rollout == nil {
		return resolvedConfig{}, pipeline.Permanent(errors.New("traffic.pool richiede un rilascio a lotti (rollout.waves con maxUnavailable): senza, tutti i target verrebbero svuotati insieme"))
	}
	if d.Rollout != nil {
		p, err := checkRollout(d, w.health, w.traffic)
		if err != nil {
			return resolvedConfig{}, pipeline.Permanent(fmt.Errorf("rollout: %w", err))
		}
		meta["rollout"] = strconv.Itoa(len(p.Stages)) + " stadi, " + strconv.Itoa(p.TotalPasses()) + " passi"
		if d.Rollout.IsTraffic() {
			meta["rollout"] = "traffico per gradini: " + fmt.Sprint(d.Rollout.Traffic.Steps)
		}
	} else if d.Traffic != nil && w.traffic != nil {
		if err := w.traffic.Check(&d); err != nil {
			return resolvedConfig{}, pipeline.Permanent(err)
		}
	}
	if err := checkRolloutApprovals(&d, approvals); err != nil {
		return resolvedConfig{}, pipeline.Permanent(err)
	}
	if m := trafficMeta(d); m != "" {
		meta["traffic"] = m
	}
	return resolvedConfig{Descriptor: d, Meta: meta}, nil
}

// Prefixes of the DBOS step names of checkpoints and compensations.
const (
	checkpointPrefix = "checkpoint:"
	rollbackPrefix   = "rollback:"
)

// startedStep is a started step that can be compensated. It is serializable because in the
// multi-region rollout it travels between the orchestrator and the pass workflows.
type startedStep struct {
	Step  pipeline.PlannedStep `json:"step"`
	Label string               `json:"label"`
	// Targets, if set, are the targets of the pass or batch.
	Targets    []descriptor.Target        `json:"targets,omitempty"`
	Outputs    map[string]pipeline.Output `json:"outputs,omitempty"`
	Checkpoint pipeline.Output            `json:"checkpoint,omitempty"`
}

// input rebuilds the step input.
func (s startedStep) input(id string, d *descriptor.Descriptor) pipeline.Input {
	dd := d
	if s.Targets != nil {
		c := *d
		c.Deploy.Targets = s.Targets
		dd = &c
	}
	return pipeline.Input{DeploymentID: id, Descriptor: dd, Outputs: s.Outputs}
}

// Rollback outcomes reported in RollbackError and in the final callback event.
const (
	RollbackCompleted = "completed"
	RollbackSkipped   = "skipped"
	RollbackFailed    = "failed"
)

// RollbackError is the error of a failed deploy for which the rollback was run.
type RollbackError struct {
	// Err is the error of the step that started the rollback.
	Err error
	// Status is the rollback outcome.
	Status string
	// Cause is the error of the failed compensation, if Status is RollbackFailed.
	Cause error
}

func (e *RollbackError) Error() string {
	switch e.Status {
	case RollbackFailed:
		return fmt.Sprintf("%v; rollback fallito: %v", e.Err, e.Cause)
	case RollbackSkipped:
		return fmt.Sprintf("%v; rollback: nessuna release da ripristinare", e.Err)
	}
	return fmt.Sprintf("%v; rollback completato", e.Err)
}

// Unwrap exposes both the step error and the rollback error.
func (e *RollbackError) Unwrap() []error {
	if e.Cause != nil {
		return []error{e.Err, e.Cause}
	}
	return []error{e.Err}
}

// compensate runs the rollback after step failed fails, if the policy provides for it: it
// compensates the started steps in reverse order, each as a DBOS step. It stops at the first
// failed compensation, so the state stays known for manual intervention.
// It returns the deploy error, enriched with the rollback outcome.
func (w *Worker) compensate(ctx dbos.Context, id string, ev *events, d *descriptor.Descriptor, res *Result, started []startedStep, failed string, stepErr error) error {
	if d.DryRun || !d.Rollback.Triggers(failed) {
		return stepErr
	}
	return w.undo(ctx, id, ev, d, res, started, stepErr)
}

// undo compensates the started steps in reverse order, each as a DBOS step, stopping at the
// first failed compensation. It returns the deploy error enriched with the outcome.
func (w *Worker) undo(ctx dbos.Context, id string, ev *events, d *descriptor.Descriptor, res *Result, started []startedStep, stepErr error) error {
	log := w.logger.With("deployment", id)
	if ctx.Err() != nil {
		// Deploy cancelled or worker stopping: compensations cannot run.
		log.Error("rollback non eseguito: deploy interrotto", "error", ctx.Err())
		return stepErr
	}
	log.Warn("rollback avviato", "steps", len(started))
	rbErr := &RollbackError{Err: stepErr, Status: RollbackSkipped}
	for i := len(started) - 1; i >= 0; i-- {
		s := started[i]
		step, _ := w.stepFor(s.Step, ev.requestedBy)
		comp, ok := step.(pipeline.Compensator)
		if !ok {
			continue
		}
		in := s.input(id, d)
		name := rollbackPrefix + s.Label
		out, err := dbos.RunAsStep(ctx,
			func(c context.Context) (pipeline.Output, error) {
				return instrument(w, c, ev, name, func(c context.Context) (pipeline.Output, error) { return comp.Compensate(c, in, s.Checkpoint) })
			},
			dbos.WithStepName(name),
			dbos.WithStepMaxRetries(s.Step.MaxRetries),
			dbos.WithStepRetryPredicate(pipeline.Retryable),
		)
		ev.send(d, stepEvent(name, out, err))
		if err != nil {
			rbErr.Status, rbErr.Cause = RollbackFailed, fmt.Errorf("step %q: %w", name, err)
			log.Error("rollback fallito: serve un intervento manuale", "step", name, "error", err)
			return rbErr
		}
		res.Steps = append(res.Steps, StepResult{Name: name, Output: out})
		if out[pipeline.OutRollback] == pipeline.RollbackCompleted {
			rbErr.Status = RollbackCompleted
		}
	}
	log.Warn("rollback terminato", "status", rbErr.Status)
	return rbErr
}
