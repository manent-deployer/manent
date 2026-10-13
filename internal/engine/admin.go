package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/joeg-ita/manent/internal/audit"
	"github.com/joeg-ita/manent/internal/cluster"
	"github.com/joeg-ita/manent/internal/telemetry"
)

// workflowNames sono tutti i workflow registrati da manent.
var workflowNames = []string{workflowName, scheduledWorkflowName, approvalWorkflowName, passWorkflowName, gateWorkflowName, afterWorkflowName, releaseWorkflowName}

// Admin raccoglie le operazioni della piattaforma: stato degli executor, recovery dei
// workflow di un nodo perso, migrazione a una nuova versione dei workflow e retention dello
// storico. Lavora sul database di sistema: non passa da API e permessi, è riservato agli
// operatori della piattaforma.
type Admin struct {
	client   dbos.Client
	registry cluster.Registry
	version  string
	audit    audit.Recorder
	logger   *slog.Logger
	now      func() time.Time
}

// AdminOptions configura Admin.
type AdminOptions struct {
	Client dbos.Client
	// Registry è il registro degli executor; senza, la recovery richiede sempre Force.
	Registry cluster.Registry
	// Version è la versione corrente dei workflow (app.version).
	Version string
	// Audit, se presente, registra recovery e retention.
	Audit  audit.Recorder
	Logger *slog.Logger
}

// NewAdmin crea Admin.
func NewAdmin(opts AdminOptions) *Admin {
	return &Admin{
		client: opts.Client, registry: opts.Registry, version: opts.Version, audit: opts.Audit,
		logger: opts.Logger.With("component", "admin"), now: time.Now,
	}
}

// ExecutorStatus è lo stato di un executor: quello registrato più i workflow in sospeso
// (PENDING) assegnati, che solo lui o una recovery possono riprendere.
type ExecutorStatus struct {
	cluster.Executor
	// Registered è false per un executor che ha workflow in sospeso ma non è nel registro.
	Registered bool `json:"registered"`
	Alive      bool `json:"alive"`
	// Pending conta i workflow in sospeso per versione dei workflow.
	Pending map[string]int64 `json:"pending,omitempty"`
}

// Executors elenca gli executor registrati e quelli con workflow in sospeso. staleAfter è il
// ritardo di heartbeat oltre il quale un executor è considerato perso.
func (a *Admin) Executors(ctx context.Context, staleAfter time.Duration) ([]ExecutorStatus, error) {
	byID := map[string]*ExecutorStatus{}
	if a.registry != nil {
		es, err := a.registry.List(ctx)
		if err != nil {
			return nil, err
		}
		now := a.now()
		for _, e := range es {
			byID[e.ID] = &ExecutorStatus{Executor: e, Registered: true, Alive: !e.Stale(now, staleAfter)}
		}
	}
	rows, err := dbos.GetWorkflowAggregates(a.client, dbos.GetWorkflowAggregatesInput{
		GroupByExecutorID: true, GroupByApplicationVersion: true, SelectCount: true,
		Status: []dbos.WorkflowStatusType{dbos.WorkflowStatusPending}, Name: workflowNames,
	})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		id, ver := group(r.Group, "executor_id"), group(r.Group, "application_version")
		es := byID[id]
		if es == nil {
			es = &ExecutorStatus{Executor: cluster.Executor{ID: id}}
			byID[id] = es
		}
		if es.Pending == nil {
			es.Pending = map[string]int64{}
		}
		if r.Count != nil {
			es.Pending[ver] += *r.Count
		}
	}
	out := make([]ExecutorStatus, 0, len(byID))
	for _, id := range slices.Sorted(maps.Keys(byID)) {
		out = append(out, *byID[id])
	}
	return out, nil
}

// group legge una colonna di raggruppamento di un aggregato DBOS.
func group(g map[string]*string, key string) string {
	if v := g[key]; v != nil {
		return *v
	}
	return ""
}

// RecoverOptions seleziona i workflow da recuperare e dice come.
type RecoverOptions struct {
	// Executor seleziona i workflow in sospeso (PENDING) di un executor perso.
	Executor string `json:"executor,omitempty"`
	// FromVersion seleziona i workflow in sospeso o in coda (ENQUEUED) di una versione dei
	// workflow non più eseguita da alcun worker, ad esempio dopo un aggiornamento.
	FromVersion string `json:"from_version,omitempty"`
	// ToVersion è la versione con cui riprenderli (default la versione corrente).
	ToVersion string `json:"to_version,omitempty"`
	// FromStart riesegue i workflow dall'inizio invece che dall'ultimo step concluso.
	FromStart bool `json:"from_start,omitempty"`
	// StaleAfter è il ritardo di heartbeat oltre il quale l'executor è considerato perso.
	StaleAfter time.Duration `json:"-"`
	// Force salta le verifiche su executor attivi, sconosciuti o già recuperati.
	Force bool `json:"force,omitempty"`
	// SameVersionOnly lascia in sospeso i workflow di un'altra versione (recovery automatica).
	SameVersionOnly bool `json:"-"`
	DryRun          bool `json:"dry_run,omitempty"`
	// By è chi esegue la recovery (utente o executor), registrato nel registro e nell'audit.
	By string `json:"by,omitempty"`
}

// Azioni della recovery su un workflow.
const (
	RecoverResumed = "resumed"
	RecoverRewound = "rewound"
	RecoverSkipped = "skipped"
	RecoverFailed  = "failed"
)

// RecoveredWorkflow è l'esito della recovery di un workflow.
type RecoveredWorkflow struct {
	ID       string `json:"id"`
	Workflow string `json:"workflow"`
	State    string `json:"state"`
	Executor string `json:"executor,omitempty"`
	Version  string `json:"version"`
	Action   string `json:"action"`
	// FromStep è lo step da cui il workflow riprende, per un workflow riportato a un'altra versione.
	FromStep int    `json:"from_step,omitempty"`
	Note     string `json:"note,omitempty"`
}

// RecoverReport è l'esito di una recovery.
type RecoverReport struct {
	DryRun    bool                `json:"dry_run,omitempty"`
	ToVersion string              `json:"to_version"`
	Workflows []RecoveredWorkflow `json:"workflows"`
}

// ErrExecutorAlive indica che l'executor da recuperare risulta ancora attivo.
var ErrExecutorAlive = errors.New("executor attivo")

// Recover rimette in esecuzione i workflow selezionati, su qualsiasi worker.
//
//   - Stessa versione: il workflow viene ripreso (ResumeWorkflow) sulla sua coda e partizione,
//     dall'ultimo step concluso. Lo step in corso al momento della perdita viene rieseguito:
//     per questo Deploy e Rollback dei plugin devono essere idempotenti.
//   - Altra versione: il workflow viene annullato e riportato (RewindWorkflow) alla nuova
//     versione con lo stesso ID, mantenendo gli step conclusi (o da zero con FromStart).
//
// Se il registro è disponibile, l'executor viene rivendicato: due recovery dello stesso
// executor non possono sovrapporsi, né con il suo riavvio.
func (a *Admin) Recover(ctx context.Context, o RecoverOptions) (*RecoverReport, error) {
	if o.Executor == "" && o.FromVersion == "" {
		return nil, fmt.Errorf("%w: indicare l'executor o la versione dei workflow da recuperare", ErrInvalidRequest)
	}
	o.ToVersion = cmp.Or(o.ToVersion, a.version)
	if o.ToVersion == "" {
		return nil, fmt.Errorf("%w: versione di destinazione sconosciuta", ErrInvalidRequest)
	}
	if err := a.checkRecover(ctx, o); err != nil {
		return nil, err
	}
	claimed := false
	if o.Executor != "" && !o.DryRun && a.registry != nil {
		ok, err := a.registry.Claim(ctx, o.Executor, o.By, a.now().Add(-o.StaleAfter), o.Force)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("%w: executor %s attivo o già recuperato (verificare con manent admin executors; --force per procedere comunque)", ErrExecutorAlive, o.Executor)
		}
		claimed = true
	}
	wfs, err := a.selectWorkflows(o)
	if err != nil {
		return nil, err
	}
	rep := &RecoverReport{DryRun: o.DryRun, ToVersion: o.ToVersion, Workflows: []RecoveredWorkflow{}}
	for _, w := range wfs {
		rep.Workflows = append(rep.Workflows, a.recoverOne(w, o))
	}
	if claimed {
		if err := a.registry.ClaimDone(ctx, o.Executor); err != nil {
			a.logger.Warn("recovery: chiusura della rivendicazione fallita", "executor", o.Executor, "error", err)
		}
	}
	if !o.DryRun {
		a.recordRecover(ctx, o, rep)
	}
	return rep, nil
}

// checkRecover verifica, salvo Force, che i workflow da recuperare non siano in esecuzione.
func (a *Admin) checkRecover(ctx context.Context, o RecoverOptions) error {
	if o.Force {
		return nil
	}
	if a.registry == nil {
		return fmt.Errorf("%w: registro degli executor non disponibile: verificare che l'executor sia fermo e usare --force", ErrInvalidRequest)
	}
	es, err := a.registry.List(ctx)
	if err != nil {
		return err
	}
	now := a.now()
	known := false
	for _, e := range es {
		alive := !e.Stale(now, o.StaleAfter)
		if e.ID == o.Executor {
			known = true
			if alive {
				return fmt.Errorf("%w: %s ha inviato un heartbeat %s fa", ErrExecutorAlive, e.ID, now.Sub(e.HeartbeatAt).Round(time.Second))
			}
		}
		if o.FromVersion != "" && e.Version == o.FromVersion && alive {
			return fmt.Errorf("%w: %s esegue ancora la versione %s: va arrestato prima di migrarne i workflow", ErrExecutorAlive, e.ID, o.FromVersion)
		}
	}
	if o.Executor != "" && !known {
		return fmt.Errorf("%w: executor %s non registrato: verificare che sia fermo e usare --force", ErrInvalidRequest, o.Executor)
	}
	return nil
}

// selectWorkflows elenca i workflow da recuperare: in sospeso e, per una migrazione di
// versione, anche in coda. I workflow DELAYED restano dove sono: andrebbero ripianificati.
func (a *Admin) selectWorkflows(o RecoverOptions) ([]dbos.WorkflowStatus, error) {
	status := []dbos.WorkflowStatusType{dbos.WorkflowStatusPending}
	if o.FromVersion != "" {
		status = append(status, dbos.WorkflowStatusEnqueued)
	}
	opts := []dbos.ListWorkflowsOption{
		dbos.WithFilterName(workflowNames...), dbos.WithFilterStatus(status...),
		dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false),
	}
	if o.Executor != "" {
		opts = append(opts, dbos.WithFilterExecutorIDs(o.Executor))
	}
	if o.FromVersion != "" {
		opts = append(opts, dbos.WithFilterAppVersion(o.FromVersion))
	}
	return dbos.ListWorkflows(a.client, opts...)
}

// recoverOne recupera un workflow.
func (a *Admin) recoverOne(w dbos.WorkflowStatus, o RecoverOptions) RecoveredWorkflow {
	r := RecoveredWorkflow{ID: w.ID, Workflow: w.Name, State: string(w.Status), Executor: w.ExecutorID, Version: w.ApplicationVersion}
	log := a.logger.With("workflow", w.ID, "name", w.Name, "executor", w.ExecutorID, "version", w.ApplicationVersion)
	sameVersion := w.ApplicationVersion == o.ToVersion
	switch {
	case !sameVersion && o.SameVersionOnly:
		r.Action, r.Note = RecoverSkipped, "versione diversa: va migrato con manent admin recover --from-version"
		return r
	case sameVersion && w.Status == dbos.WorkflowStatusEnqueued:
		r.Action, r.Note = RecoverSkipped, "già in coda per la versione corrente"
		return r
	case sameVersion && !o.FromStart:
		r.Action = RecoverResumed
		if o.DryRun {
			return r
		}
		var opts []dbos.ResumeWorkflowOption
		if w.QueueName != "" {
			// Stessa coda: la partizione resta quella registrata, quindi vale ancora il limite per ambiente.
			opts = append(opts, dbos.WithResumeQueue(w.QueueName))
		}
		if _, err := dbos.ResumeWorkflow[any](a.client, w.ID, opts...); err != nil {
			r.Action, r.Note = RecoverFailed, err.Error()
			log.Error("recovery: ripresa del workflow fallita", "error", err)
			return r
		}
		log.Info("recovery: workflow ripreso")
		return r
	}
	r.Action = RecoverRewound
	if w.Status == dbos.WorkflowStatusPending && !o.FromStart {
		steps, err := dbos.GetWorkflowSteps(a.client, w.ID, dbos.WithStepsLoadOutput(false))
		if err != nil {
			r.Action, r.Note = RecoverFailed, err.Error()
			return r
		}
		for _, s := range steps {
			r.FromStep = max(r.FromStep, s.StepID+1)
		}
	}
	if w.Name == approvalWorkflowName {
		r.Note = "le decisioni inviate dopo la perdita del nodo, non ancora elaborate, vanno ripetute"
	}
	if o.DryRun {
		return r
	}
	if err := dbos.CancelWorkflow(a.client, w.ID); err != nil {
		r.Action, r.Note = RecoverFailed, err.Error()
		log.Error("recovery: annullamento prima della migrazione fallito", "error", err)
		return r
	}
	opts := []dbos.RewindWorkflowOption{
		dbos.WithRewindApplicationVersion(o.ToVersion),
		dbos.WithRewindStartStep(uint(r.FromStep)),
	}
	if w.QueueName != "" {
		opts = append(opts, dbos.WithRewindQueue(w.QueueName))
		if w.QueuePartitionKey != "" {
			opts = append(opts, dbos.WithRewindQueuePartitionKey(w.QueuePartitionKey))
		}
	}
	if _, err := dbos.RewindWorkflow[any](a.client, w.ID, opts...); err != nil {
		r.Action, r.Note = RecoverFailed, "annullato ma non riportato alla nuova versione: "+err.Error()
		log.Error("recovery: migrazione del workflow fallita: il workflow è annullato", "error", err)
		return r
	}
	log.Info("recovery: workflow migrato", "to_version", o.ToVersion, "from_step", r.FromStep)
	return r
}

// ActionAdminRecover è l'azione di audit di una recovery.
const ActionAdminRecover = "admin.recover"

// recordRecover registra la recovery nell'audit.
func (a *Admin) recordRecover(ctx context.Context, o RecoverOptions, rep *RecoverReport) {
	counts := map[string]int{}
	for _, w := range rep.Workflows {
		counts[w.Action]++
	}
	outcome := audit.OutcomeSucceeded
	if counts[RecoverFailed] > 0 {
		outcome = audit.OutcomeFailed
	}
	a.logger.Warn("recovery eseguita", "executor", o.Executor, "from_version", o.FromVersion, "to_version", rep.ToVersion, "by", o.By, "result", counts)
	if a.audit == nil {
		return
	}
	_ = a.audit.Record(ctx, audit.Entry{
		Time: a.now().UTC(), Actor: o.By, Action: ActionAdminRecover, Outcome: outcome,
		Comment: fmt.Sprintf("executor=%s from_version=%s to_version=%s force=%v ripresi=%d migrati=%d saltati=%d falliti=%d",
			o.Executor, o.FromVersion, rep.ToVersion, o.Force, counts[RecoverResumed], counts[RecoverRewound], counts[RecoverSkipped], counts[RecoverFailed]),
	})
}

// WorkflowCounts implementa telemetry.StateSource: workflow di manent per tipo, stato e coda.
func (a *Admin) WorkflowCounts(_ context.Context) ([]telemetry.WorkflowCount, error) {
	rows, err := dbos.GetWorkflowAggregates(a.client, dbos.GetWorkflowAggregatesInput{
		GroupByName: true, GroupByStatus: true, GroupByQueueName: true, SelectCount: true, Name: workflowNames,
	})
	if err != nil {
		return nil, err
	}
	out := make([]telemetry.WorkflowCount, 0, len(rows))
	for _, r := range rows {
		c := telemetry.WorkflowCount{Workflow: group(r.Group, "name"), Status: group(r.Group, "status"), Queue: group(r.Group, "queue_name")}
		if r.Count != nil {
			c.Count = *r.Count
		}
		out = append(out, c)
	}
	return out, nil
}

// ExecutorList implementa telemetry.StateSource.
func (a *Admin) ExecutorList(ctx context.Context) ([]cluster.Executor, error) {
	if a.registry == nil {
		return nil, nil
	}
	return a.registry.List(ctx)
}
