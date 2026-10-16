package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/joeg-ita/manent/internal/audit"
	"github.com/joeg-ita/manent/internal/cluster"
	"github.com/joeg-ita/manent/internal/telemetry"
)

// workflowNames are all the workflows registered by manent.
var workflowNames = []string{workflowName, scheduledWorkflowName, approvalWorkflowName, passWorkflowName, gateWorkflowName, afterWorkflowName, releaseWorkflowName, retireWorkflowName}

// Admin groups the platform operations: executor state, recovery of a lost node's workflows,
// migration to a new workflow version and history retention. It works on the system
// database: it bypasses API and permissions and is reserved to platform operators.
type Admin struct {
	client   dbos.Client
	registry cluster.Registry
	version  string
	audit    audit.Recorder
	logger   *slog.Logger
	now      func() time.Time

	// countsMu guards the counts of finished workflows, read at most every finishedCountsTTL.
	countsMu       sync.Mutex
	finishedCounts []telemetry.WorkflowCount
	finishedAt     time.Time
}

// AdminOptions configures Admin.
type AdminOptions struct {
	Client dbos.Client
	// Registry is the executor registry; without it recovery always requires Force.
	Registry cluster.Registry
	// Version is the current workflow version (app.version).
	Version string
	// Audit, if set, records recoveries and retention.
	Audit  audit.Recorder
	Logger *slog.Logger
}

// NewAdmin creates Admin.
func NewAdmin(opts AdminOptions) *Admin {
	return &Admin{
		client: opts.Client, registry: opts.Registry, version: opts.Version, audit: opts.Audit,
		logger: opts.Logger.With("component", "admin"), now: time.Now,
	}
}

// ExecutorStatus is the state of an executor: the registered one plus the assigned pending
// (PENDING) workflows, which only it or a recovery can resume.
type ExecutorStatus struct {
	cluster.Executor
	// Registered is false for an executor with pending workflows that is not in the registry.
	Registered bool `json:"registered"`
	Alive      bool `json:"alive"`
	// Pending counts the pending workflows per workflow version.
	Pending map[string]int64 `json:"pending,omitempty"`
}

// Executors lists the registered executors and those with pending workflows. staleAfter is
// the heartbeat delay beyond which an executor is considered lost.
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

// group reads a grouping column of a DBOS aggregate.
func group(g map[string]*string, key string) string {
	if v := g[key]; v != nil {
		return *v
	}
	return ""
}

// RecoverOptions selects the workflows to recover and says how.
type RecoverOptions struct {
	// Executor selects the pending (PENDING) workflows of a lost executor.
	Executor string `json:"executor,omitempty"`
	// FromVersion selects the pending or queued (ENQUEUED) workflows of a workflow version no
	// worker runs any more, for example after an upgrade.
	FromVersion string `json:"from_version,omitempty"`
	// ToVersion is the version to resume them with (default the current version).
	ToVersion string `json:"to_version,omitempty"`
	// FromStart runs the workflows again from the beginning instead of from the last finished step.
	FromStart bool `json:"from_start,omitempty"`
	// StaleAfter is the heartbeat delay beyond which the executor is considered lost.
	StaleAfter time.Duration `json:"-"`
	// Force skips the checks on live, unknown or already recovered executors.
	Force bool `json:"force,omitempty"`
	// SameVersionOnly leaves the workflows of another version pending (automatic recovery).
	SameVersionOnly bool `json:"-"`
	DryRun          bool `json:"dry_run,omitempty"`
	// By is who runs the recovery (user or executor), recorded in the registry and the audit.
	By string `json:"by,omitempty"`
}

// Recovery actions on a workflow.
const (
	RecoverResumed = "resumed"
	RecoverRewound = "rewound"
	RecoverSkipped = "skipped"
	RecoverFailed  = "failed"
)

// RecoveredWorkflow is the recovery outcome of a workflow.
type RecoveredWorkflow struct {
	ID       string `json:"id"`
	Workflow string `json:"workflow"`
	State    string `json:"state"`
	Executor string `json:"executor,omitempty"`
	Version  string `json:"version"`
	Action   string `json:"action"`
	// FromStep is the step the workflow resumes from, for a workflow moved to another version.
	FromStep int    `json:"from_step,omitempty"`
	Note     string `json:"note,omitempty"`
}

// RecoverReport is the outcome of a recovery.
type RecoverReport struct {
	DryRun    bool                `json:"dry_run,omitempty"`
	ToVersion string              `json:"to_version"`
	Workflows []RecoveredWorkflow `json:"workflows"`
}

// ErrExecutorAlive means the executor to recover still looks alive.
var ErrExecutorAlive = errors.New("executor attivo")

// Recover runs the selected workflows again, on any worker.
//
//   - Same version: the workflow is resumed (ResumeWorkflow) on its queue and partition, from
//     the last finished step. The step running when the node was lost runs again: this is
//     why plugin Deploy and Rollback must be idempotent.
//   - Another version: the workflow is cancelled and moved (RewindWorkflow) to the new
//     version with the same ID, keeping the finished steps (or from scratch with FromStart).
//
// If the registry is available, the executor is claimed: two recoveries of the same executor
// cannot overlap, nor with its restart.
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

// checkRecover checks, unless Force, that the workflows to recover are not running.
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

// selectWorkflows lists the workflows to recover: pending and, for a version migration, also
// queued. DELAYED workflows stay where they are: they would need rescheduling.
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

// recoverOne recovers one workflow.
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
			// Same queue: the partition stays the recorded one, so the per-environment limit still applies.
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

// ActionAdminRecover is the audit action of a recovery.
const ActionAdminRecover = "admin.recover"

// recordRecover records the recovery in the audit.
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

// States of unfinished workflows, counted at every metrics read.
var activeStatuses = []dbos.WorkflowStatusType{
	dbos.WorkflowStatusPending, dbos.WorkflowStatusEnqueued, dbos.WorkflowStatusDelayed,
}

// finishedCountsTTL is the refresh interval of the finished workflow counts: grouping them
// scans the whole history, while the unfinished ones use DBOS partial indexes and are cheap.
const finishedCountsTTL = 5 * time.Minute

// WorkflowCounts implements telemetry.StateSource: manent workflows by name, state and queue.
func (a *Admin) WorkflowCounts(_ context.Context) ([]telemetry.WorkflowCount, error) {
	out, err := a.workflowCounts(activeStatuses)
	if err != nil {
		return nil, err
	}
	a.countsMu.Lock()
	defer a.countsMu.Unlock()
	if a.finishedCounts == nil || a.now().Sub(a.finishedAt) >= finishedCountsTTL {
		finished, err := a.workflowCounts([]dbos.WorkflowStatusType{
			dbos.WorkflowStatusSuccess, dbos.WorkflowStatusError, dbos.WorkflowStatusCancelled,
			dbos.WorkflowStatusMaxRecoveryAttemptsExceeded,
		})
		if err != nil {
			return nil, err
		}
		a.finishedCounts, a.finishedAt = finished, a.now()
	}
	return append(out, a.finishedCounts...), nil
}

// workflowCounts counts manent workflows in the given states, by name, state and queue.
func (a *Admin) workflowCounts(status []dbos.WorkflowStatusType) ([]telemetry.WorkflowCount, error) {
	rows, err := dbos.GetWorkflowAggregates(a.client, dbos.GetWorkflowAggregatesInput{
		GroupByName: true, GroupByStatus: true, GroupByQueueName: true, SelectCount: true, Name: workflowNames, Status: status,
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

// ExecutorList implements telemetry.StateSource.
func (a *Admin) ExecutorList(ctx context.Context) ([]cluster.Executor, error) {
	if a.registry == nil {
		return nil, nil
	}
	return a.registry.List(ctx)
}
