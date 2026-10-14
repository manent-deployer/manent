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
	// Driver SQLite per DBOS, usato con database_url "sqlite:...".
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
	"github.com/joeg-ita/manent/internal/pipeline"
	"github.com/joeg-ita/manent/internal/rollout"
	"github.com/joeg-ita/manent/internal/telemetry"
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
	// Custom costruisce gli step personalizzati del piano.
	Custom CustomSteps
	// Resolver, se presente, produce il descrittore effettivo dalla configurazione di ambiente.
	Resolver ConfigResolver
	// ResolverRetries sono i tentativi aggiuntivi dello step config (es. Consul non raggiungibile).
	ResolverRetries int
	// Cleaner, se presente, libera le risorse locali al termine di ogni deploy.
	Cleaner pipeline.Cleaner
	// Notifier, se presente, invia gli eventi ai callback del descrittore.
	Notifier callback.Notifier
	// CallbackRetries sono i tentativi aggiuntivi di ogni invio a un callback.
	CallbackRetries int
	// Chat, se presente, invia le notifiche in chat (destinazioni scelte all'accodamento).
	Chat ChatNotifier
	// Change, se presente, è il sistema di change management dei deploy che lo richiedono.
	Change change.Provider
	// ChangePoll è l'intervallo iniziale di lettura dell'approvazione del change (default 1m).
	ChangePoll time.Duration
	// PublicURL è l'indirizzo dell'interfaccia web, per i link nei ticket.
	PublicURL string
	// Rollback, se presente, applica la policy di rollback al descrittore effettivo.
	Rollback RollbackPolicy
	// Audit, se presente, registra l'esito finale di ogni deploy.
	Audit audit.Recorder
	// Governance, se presente, blocca all'avvio i deploy durante una finestra di freeze.
	Governance Governance
	Logger     *slog.Logger
	// SchedulePollInterval è ogni quanto DBOS ricarica le schedule dal database (default 30s).
	SchedulePollInterval time.Duration
	// Metrics, se presente, registra durata ed esito di deploy e step.
	Metrics telemetry.Recorder
	// Registry, se presente, registra il worker (heartbeat) e abilita la recovery automatica
	// dei nodi persi e la retention dello storico.
	Registry cluster.Registry
	// Build è la versione del binario, registrata con il worker.
	Build string
	// Hostname è registrato con il worker.
	Hostname    string
	Maintenance MaintenanceOptions
	// Health esegue l'health gate dei rollout a ondate.
	Health rollout.Checker
	// HealthTimeout limita ogni lettura dell'health gate (default 30s).
	HealthTimeout time.Duration
	// Region è la region del worker: con RegionRouting esegue i passi dei rollout di quella
	// region (coda <coda>.<region>) oltre ai deploy.
	Region string
	// RegionRouting esegue i passi dei rollout sui worker della region dei target.
	RegionRouting bool
	// ChainPollInterval è il primo intervallo di lettura dello stato delle dipendenze e dei
	// deploy di una release (default 5s); raddoppia a ogni lettura fino a 1 minuto.
	ChainPollInterval time.Duration
}

// ConfigResolver recupera la configurazione di ambiente e la fonde con il descrittore.
type ConfigResolver interface {
	Resolve(ctx context.Context, d descriptor.Descriptor) (descriptor.Descriptor, map[string]string, error)
}

// RollbackPolicy decide il rollback automatico di un deploy, scrivendo la decisione in d.Rollback.
type RollbackPolicy interface {
	Apply(d descriptor.Descriptor) (descriptor.Descriptor, error)
	// Allowed indica se il tipo di artefatto ammette il rollback (automatico o manuale).
	Allowed(artifactType string) error
}

// resolvedConfig è l'output persistito dello step config.
type resolvedConfig struct {
	Descriptor descriptor.Descriptor `json:"descriptor"`
	Meta       pipeline.Output       `json:"meta"`
}

// Worker esegue i workflow di deploy prelevandoli dalla coda condivisa.
// Più worker possono condividere lo stesso database di sistema (Postgres) per scalare orizzontalmente.
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
	// reader legge lo stato dei deploy (dipendenze, release) sul contesto del worker.
	reader    *Engine
	chainPoll time.Duration
	// service è il servizio completo usato dalle release per i rollback (SetService).
	service Service
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
		// Passi dei rollout della region: uno alla volta per applicazione/ambiente/region.
		rq := regionQueue(q.Name, w.region)
		if _, err = dbos.RegisterQueue(ctx, rq, dbos.WithWorkerConcurrency(q.WorkerConcurrency),
			dbos.WithPartitionConcurrency(1)); err != nil {
			return nil, fmt.Errorf("registrazione coda %q: %w", rq, err)
		}
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

// Version restituisce la versione dei workflow eseguiti dal worker.
func (w *Worker) Version() string { return w.version }

// SetService imposta il servizio completo (con planner e policy) usato dalle release per i
// rollback dei deploy riusciti. Va chiamato prima di Launch.
func (w *Worker) SetService(s Service) { w.service = s }

// Admin restituisce le operazioni di piattaforma sul database del worker.
func (w *Worker) Admin() *Admin { return w.admin }

// Launch registra il worker e avvia DBOS: recupera i workflow in sospeso di questo executor e
// inizia a prelevare dalla coda. La registrazione precede la recovery di DBOS: se un altro
// worker sta recuperando i workflow di questo executor (perso in precedenza), attende che
// abbia finito, così nessun workflow viene ripreso due volte.
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
		// Il worker di una region esegue deploy, approvazioni e solo i passi della sua region.
		dbos.ListenQueues(w.dbos, w.queueName, approvalQueue(w.queueName), regionQueue(w.queueName, w.region))
	case w.regionRouting:
		// Senza region, con l'instradamento attivo: solo orchestrazione, nessun passo.
		dbos.ListenQueues(w.dbos, w.queueName, approvalQueue(w.queueName))
	}
	if w.registry != nil {
		w.wg.Add(1)
		go w.maintain()
	}
	w.logger.Info("worker avviato", "queue", w.queue.GetName(), "version", w.version)
	return nil
}

// register registra l'avvio del worker, attendendo la fine di un'eventuale recovery in corso
// dei suoi workflow (al più cluster.ClaimTimeout). Se lo stesso executor_id risulta in uso da
// un altro processo attivo, attende che risulti perso (StaleAfter: il caso di un riavvio dopo un
// crash) e poi rinuncia: due processi con lo stesso ID riprenderebbero l'uno i deploy dell'altro.
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

// drainQueue è una coda inesistente: ascoltarla equivale a non prelevare più deploy.
const drainQueue = "manent-draining"

// Drain smette di prelevare deploy dalla coda e attende, fino a timeout, che quelli in
// esecuzione su questo worker si concludano. Le approvazioni in attesa non vengono attese:
// riprendono al riavvio, o su un altro worker con la recovery.
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

// Shutdown arresta la manutenzione e DBOS, attendendo i workflow in corso fino al timeout, e
// registra l'arresto. I workflow interrotti restano in sospeso e vengono ripresi al riavvio di
// un worker con lo stesso ExecutorID o, con la recovery, da un altro worker.
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

// deployWorkflow risolve la configurazione di ambiente, esegue gli step del piano in ordine
// (ciascuno come step DBOS) e al termine, con successo o errore, libera le risorse locali
// e notifica l'esito ai callback.
// Deve restare deterministico: tutta la logica con effetti esterni è dentro gli step.
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

// recordMetrics registra esito e durata del deploy. La durata parte dall'avvio sul worker
// (registrato da DBOS), quindi esclude l'attesa in coda e le approvazioni. Non è uno step: le
// metriche sono del processo e non vanno ripetute né conservate.
func (w *Worker) recordMetrics(id string, req Request, final callback.Event) {
	var dur time.Duration
	// Il context radice, non quello del workflow: la lettura non diventa uno step.
	if wfs, err := dbos.ListWorkflows(w.dbos, dbos.WithFilterWorkflowIDs(id), dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false)); err == nil && len(wfs) == 1 && !wfs[0].StartedAt.IsZero() {
		dur = time.Since(wfs[0].StartedAt)
	}
	d := req.Descriptor
	w.metrics.DeploymentFinished(d.Application, d.Environment, cmp.Or(req.Kind, KindDeploy), final.Status, d.DryRun, dur)
}

// recordAttributes aggiunge agli attributi del workflow l'esito, lo sha256 dell'artefatto
// rilasciato e l'esito dell'eventuale rollback, usati da inventario e promozioni.
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

// Step e output da cui si legge lo sha256 dell'artefatto rilasciato (internal/steps).
const (
	stepArtifacts = "artifacts"
	outSHA256     = "sha256"
)

// recordOutcome registra nell'audit l'esito finale del deploy, in uno step DBOS: dopo una
// recovery la voce non viene ripetuta.
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

// ActionDeploymentFinish è l'azione di audit con l'esito finale di un deploy.
const ActionDeploymentFinish = "deployment.finish"

// events invia gli eventi di un deploy ai callback del descrittore, ciascuno come step DBOS:
// dopo una recovery gli invii già completati non vengono ripetuti. La numerazione dipende solo
// dall'input del workflow, quindi resta deterministica.
type events struct {
	w           *Worker
	ctx         dbos.Context
	id          string
	requestedBy string
	seq         int
	// span è la span del deploy, genitore di quelle degli step.
	span trace.SpanContext
	// silent disattiva l'invio: nei workflow dei passi gli eventi li invia l'orchestratore.
	silent bool
}

// send notifica l'evento ai callback interessati. Un invio fallito dopo i retry viene
// registrato ma non fa fallire il deploy.
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

// stepEvent costruisce l'evento con l'esito di uno step.
func stepEvent(name string, out pipeline.Output, err error) callback.Event {
	ev := callback.Event{Type: descriptor.CallbackEventStep, Step: name, Status: callback.StatusSucceeded, Output: out}
	if err != nil {
		ev.Status, ev.Error = callback.StatusFailed, err.Error()
	}
	return ev
}

// run esegue lo step config e poi gli step del piano, fermandosi al primo errore.
// La configurazione di ambiente è letta in uno step DBOS: è quella valida al momento
// dell'esecuzione (anche per i deploy pianificati) e, una volta registrata, resta la stessa
// anche se il deploy viene ripreso dopo un crash. Nello stesso step viene applicata la policy
// di rollback, così anche la decisione di rollback resta quella registrata.
// In dry-run ogni step esegue DryRun, se lo implementa; altrimenti viene saltato, così uno step
// nuovo non produce mai effetti per errore.
// Con il rollback attivo, prima di ogni pipeline.Compensator viene eseguito il suo checkpoint
// e, se uno step indicato in rollback.on fallisce, gli step già avviati vengono compensati.
// Restituisce anche il descrittore effettivo, usato per l'evento finale.
func (w *Worker) run(ctx dbos.Context, id string, req Request, ev *events) (Result, *descriptor.Descriptor, error) {
	dryRun := req.Descriptor.DryRun
	res := Result{DeploymentID: id, DryRun: dryRun}
	if w.resolver != nil || w.rollback != nil || w.governance != nil {
		rc, err := dbos.RunAsStep(ctx, func(c context.Context) (resolvedConfig, error) {
			return instrument(w, c, ev, "config", func(c context.Context) (resolvedConfig, error) { return w.resolveConfig(c, req.Descriptor) })
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

// execStep esegue uno step del piano come step DBOS e, se il rollback è attivo e lo step è
// compensabile, prima il suo checkpoint. label è il nome dello step DBOS: nel rollout
// distingue le esecuzioni dello stesso step in passi e lotti diversi ("deploy@eu-1/eu-1a#0");
// targets, se non nil, sostituisce i target del descrittore. Restituisce lo step avviato da
// compensare in caso di rollback (anche se è fallito: può aver modificato una parte dei
// target); skipped indica uno step saltato in dry-run.
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
			// Lo step non è partito: non c'è nulla da compensare.
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
	// Gli output degli step personalizzati non vanno ai callback, definiti dal descrittore:
	// restano nello stato del deploy, visibile con i permessi di lettura.
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

// instrument esegue fn come corpo di uno step DBOS: i log emessi con il context dello step
// finiscono nel log del deploy (internal/deploylog) e ogni tentativo è registrato nelle
// metriche e in una span figlia di quella del deploy. Gli step già conclusi, riletti dopo una
// recovery, non vengono rieseguiti e quindi non sono ripetuti.
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

// resolveConfig produce il descrittore effettivo: configurazione di ambiente e policy di rollback.
// Un deploy reale che parte durante una finestra di freeze fallisce, salvo deroga.
func (w *Worker) resolveConfig(ctx context.Context, d descriptor.Descriptor) (resolvedConfig, error) {
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
	if d.Rollout != nil {
		p, err := checkRollout(d, w.health)
		if err != nil {
			return resolvedConfig{}, pipeline.Permanent(fmt.Errorf("rollout: %w", err))
		}
		meta["rollout"] = strconv.Itoa(len(p.Stages)) + " stadi, " + strconv.Itoa(p.TotalPasses()) + " passi"
	}
	return resolvedConfig{Descriptor: d, Meta: meta}, nil
}

// Prefissi dei nomi degli step DBOS di checkpoint e di compensazione.
const (
	checkpointPrefix = "checkpoint:"
	rollbackPrefix   = "rollback:"
)

// startedStep è uno step avviato che può essere compensato. È serializzabile perché nel
// rollout multi-region viaggia tra l'orchestratore e i workflow dei passi.
type startedStep struct {
	Step  pipeline.PlannedStep `json:"step"`
	Label string               `json:"label"`
	// Targets, se valorizzato, sono i target del passo o del lotto.
	Targets    []descriptor.Target        `json:"targets,omitempty"`
	Outputs    map[string]pipeline.Output `json:"outputs,omitempty"`
	Checkpoint pipeline.Output            `json:"checkpoint,omitempty"`
}

// input ricostruisce l'input dello step.
func (s startedStep) input(id string, d *descriptor.Descriptor) pipeline.Input {
	dd := d
	if s.Targets != nil {
		c := *d
		c.Deploy.Targets = s.Targets
		dd = &c
	}
	return pipeline.Input{DeploymentID: id, Descriptor: dd, Outputs: s.Outputs}
}

// Esiti del rollback riportati in RollbackError e nell'evento finale dei callback.
const (
	RollbackCompleted = "completed"
	RollbackSkipped   = "skipped"
	RollbackFailed    = "failed"
)

// RollbackError è l'errore di un deploy fallito per cui è stato eseguito il rollback.
type RollbackError struct {
	// Err è l'errore dello step che ha avviato il rollback.
	Err error
	// Status è l'esito del rollback.
	Status string
	// Cause è l'errore della compensazione fallita, se Status è RollbackFailed.
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

// Unwrap espone sia l'errore dello step sia quello del rollback.
func (e *RollbackError) Unwrap() []error {
	if e.Cause != nil {
		return []error{e.Err, e.Cause}
	}
	return []error{e.Err}
}

// compensate esegue il rollback dopo il fallimento dello step failed, se la policy lo prevede:
// compensa gli step avviati in ordine inverso, ciascuno come step DBOS. Si ferma alla prima
// compensazione fallita, così lo stato resta noto per l'intervento manuale.
// Restituisce l'errore del deploy, arricchito con l'esito del rollback.
func (w *Worker) compensate(ctx dbos.Context, id string, ev *events, d *descriptor.Descriptor, res *Result, started []startedStep, failed string, stepErr error) error {
	if d.DryRun || !d.Rollback.Triggers(failed) {
		return stepErr
	}
	return w.undo(ctx, id, ev, d, res, started, stepErr)
}

// undo compensa gli step avviati in ordine inverso, ciascuno come step DBOS, fermandosi alla
// prima compensazione fallita. Restituisce l'errore del deploy arricchito con l'esito.
func (w *Worker) undo(ctx dbos.Context, id string, ev *events, d *descriptor.Descriptor, res *Result, started []startedStep, stepErr error) error {
	log := w.logger.With("deployment", id)
	if ctx.Err() != nil {
		// Deploy annullato o worker in arresto: le compensazioni non possono essere eseguite.
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
