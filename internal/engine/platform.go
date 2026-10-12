package engine

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/joeg-ita/manent/internal/service"
)

// ExecutorLister elenca gli executor registrati e quelli con workflow in sospeso (Admin).
type ExecutorLister interface {
	Executors(ctx context.Context, staleAfter time.Duration) ([]ExecutorStatus, error)
}

// maxActive limita i workflow non conclusi letti da Workers e Activity.
const maxActive = 1000

// Workers implementa PlatformService: gli executor del registro, con i workflow in esecuzione
// (PENDING) di ciascuno. Senza registro restano solo gli executor che hanno workflow in sospeso.
func (e *Engine) Workers(ctx context.Context) ([]WorkerInfo, error) {
	byID := map[string]*WorkerInfo{}
	if e.executors != nil {
		es, err := e.executors.Executors(ctx, e.staleAfter)
		if err != nil {
			return nil, err
		}
		for _, x := range es {
			byID[x.ID] = workerInfo(x)
		}
	}
	wfs, err := dbos.ListWorkflows(e.client,
		dbos.WithFilterName(workflowNames...),
		dbos.WithFilterStatus(dbos.WorkflowStatusPending),
		dbos.WithFilterSortDesc(), dbos.WithFilterLimit(maxActive),
		dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false))
	if err != nil {
		return nil, err
	}
	for _, w := range wfs {
		wi := byID[w.ExecutorID]
		if wi == nil {
			wi = &WorkerInfo{ID: w.ExecutorID, State: service.WorkerUnregistered, Tasks: []WorkerTask{}}
			byID[w.ExecutorID] = wi
		}
		wi.Tasks = append(wi.Tasks, toTask(w))
	}
	out := make([]WorkerInfo, 0, len(byID))
	for _, id := range slices.Sorted(maps.Keys(byID)) {
		out = append(out, *byID[id])
	}
	return out, nil
}

// workerInfo converte lo stato di un executor nel formato esposto dal servizio.
func workerInfo(x ExecutorStatus) *WorkerInfo {
	wi := &WorkerInfo{
		ID: x.ID, Version: x.Version, Build: x.Build, Hostname: x.Hostname, Region: x.Region,
		StoppedAt: x.StoppedAt, RecoveredBy: x.RecoveredBy, RecoveredAt: x.RecoveredAt,
		Pending: x.Pending, Tasks: []WorkerTask{},
	}
	switch {
	case !x.Registered:
		wi.State = service.WorkerUnregistered
	case x.StoppedAt != nil:
		wi.State = service.WorkerStopped
	case x.Alive:
		wi.State = service.WorkerAlive
	default:
		wi.State = service.WorkerStale
	}
	if x.Registered {
		started, beat := x.StartedAt, x.HeartbeatAt
		wi.StartedAt, wi.HeartbeatAt = &started, &beat
	}
	return wi
}

// toTask descrive un workflow in esecuzione. I passi e le sonde di un rollout non hanno
// attributi: applicazione, ambiente e region si leggono dalla chiave di partizione
// ("<application>/<environment>/<region>[/gate]").
func toTask(w dbos.WorkflowStatus) WorkerTask {
	t := WorkerTask{
		ID: w.ID, DeploymentID: w.ID, Queue: w.QueueName, Attempts: w.Attempts,
		Application: attr(w, attrApplication), Environment: attr(w, attrEnvironment), Version: attr(w, attrVersion),
	}
	if !w.StartedAt.IsZero() {
		at := w.StartedAt.UTC()
		t.StartedAt = &at
	}
	parts := strings.Split(w.QueuePartitionKey, "/")
	if t.Application == "" && len(parts) >= 2 {
		t.Application, t.Environment = parts[0], parts[1]
	}
	if len(parts) >= 3 {
		t.Region = parts[2]
	}
	switch w.Name {
	case workflowName:
		t.Kind = service.TaskDeploy
	case approvalWorkflowName:
		t.Kind, t.DeploymentID = service.TaskApproval, strings.TrimSuffix(w.ID, approvalSuffix)
	case scheduledWorkflowName:
		t.Kind = service.TaskSchedule
	case passWorkflowName:
		t.Kind, t.DeploymentID = service.TaskPass, cmp.Or(w.ParentWorkflowID, parentID(w.ID, ".pass.", ".undo."))
	case gateWorkflowName:
		t.Kind, t.DeploymentID = service.TaskGate, cmp.Or(w.ParentWorkflowID, parentID(w.ID, ".gate."))
	}
	return t
}

// parentID ricava l'ID del deploy da quello di un workflow figlio ("<id>.pass.<passo>").
func parentID(id string, seps ...string) string {
	for _, s := range seps {
		if i := strings.LastIndex(id, s); i > 0 {
			return id[:i]
		}
	}
	return id
}

// Activity implementa PlatformService: i deploy e le approvazioni non conclusi. Dei deploy in
// esecuzione legge lo stato completo (rollout, approvazioni tra ondate) e l'ultimo step
// registrato, al più per maxPendingChecked deploy.
func (e *Engine) Activity(ctx context.Context, f ActivityFilter) (*Activity, error) {
	wfs, err := dbos.ListWorkflows(e.client,
		dbos.WithFilterName(workflowName, approvalWorkflowName),
		dbos.WithFilterStatus(dbos.WorkflowStatusPending, dbos.WorkflowStatusEnqueued, dbos.WorkflowStatusDelayed),
		dbos.WithFilterSortDesc(), dbos.WithFilterLimit(maxActive),
		dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false))
	if err != nil {
		return nil, err
	}
	a := &Activity{Running: []Status{}, AwaitingApproval: []Status{}, Queued: []Status{}, Delayed: []Status{}, Truncated: len(wfs) == maxActive}
	match := ListFilter{Application: f.Application, Environment: f.Environment, Match: f.Match}
	detailed := 0
	for _, w := range wfs {
		s := toStatus(w)
		if !match.Matches(s) {
			continue
		}
		if w.Status == dbos.WorkflowStatusPending && detailed < maxPendingChecked {
			detailed++
			if full, err := e.Status(ctx, s.ID); err == nil {
				s = *full
			}
			if s.State == StatePending {
				s.LastStep = e.lastStep(w.ID)
			}
		}
		switch s.State {
		case StatePending:
			a.Running = append(a.Running, s)
		case StateAwaitingApproval:
			a.AwaitingApproval = append(a.AwaitingApproval, s)
		case StateEnqueued:
			a.Queued = append(a.Queued, s)
		case StateDelayed:
			a.Delayed = append(a.Delayed, s)
		}
	}
	// In coda e pianificati: prima chi parte prima.
	slices.SortStableFunc(a.Queued, func(x, y Status) int { return x.CreatedAt.Compare(y.CreatedAt) })
	slices.SortStableFunc(a.Delayed, func(x, y Status) int { return startsAt(x).Compare(startsAt(y)) })
	return a, nil
}

func startsAt(s Status) time.Time {
	if s.ScheduledAt != nil {
		return *s.ScheduledAt
	}
	return s.CreatedAt
}

// lastStep restituisce il nome dell'ultimo step registrato del workflow, o "" se non ce ne sono.
func (e *Engine) lastStep(id string) string {
	steps, err := dbos.GetWorkflowSteps(e.client, id, dbos.WithStepsLoadOutput(false))
	if err != nil || len(steps) == 0 {
		return ""
	}
	return steps[len(steps)-1].StepName
}

// Stati dei workflow usati da Activity.
const (
	StatePending  = service.StatePending
	StateEnqueued = service.StateEnqueued
	StateDelayed  = service.StateDelayed
)

var _ service.PlatformService = (*Engine)(nil)
