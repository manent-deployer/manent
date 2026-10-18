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

// ExecutorLister lists the registered executors and those with pending workflows (Admin).
type ExecutorLister interface {
	Executors(ctx context.Context, staleAfter time.Duration) ([]ExecutorStatus, error)
}

// maxActive limits the unfinished workflows read by Workers and Activity.
const maxActive = 1000

// Workers implements PlatformService: the registry executors, with the running (PENDING)
// workflows of each. Without a registry only executors with pending workflows remain.
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

// workerInfo converts the state of an executor into the format exposed by the service.
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

// toTask describes a running workflow. Rollout passes and probes have no attributes:
// application, environment and region are read from the partition key
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
	case afterWorkflowName:
		t.Kind, t.DeploymentID = service.TaskAfter, strings.TrimSuffix(w.ID, afterSuffix)
	case releaseWorkflowName:
		t.Kind = service.TaskRelease
	case passWorkflowName:
		t.Kind, t.DeploymentID = service.TaskPass, cmp.Or(w.ParentWorkflowID, parentID(w.ID, ".pass.", ".undo."))
	case gateWorkflowName:
		t.Kind, t.DeploymentID = service.TaskGate, cmp.Or(w.ParentWorkflowID, parentID(w.ID, ".gate."))
	}
	return t
}

// parentID derives the deploy ID from that of a child workflow ("<id>.pass.<pass>").
func parentID(id string, seps ...string) string {
	for _, s := range seps {
		if i := strings.LastIndex(id, s); i > 0 {
			return id[:i]
		}
	}
	return id
}

// Activity implements PlatformService: unfinished deploys and approvals. For running deploys
// it reads the rollout (approvals between waves) and the last recorded step, for approvals and
// waits the full state, at most for maxPendingChecked workflows.
func (e *Engine) Activity(ctx context.Context, f ActivityFilter) (*Activity, error) {
	wfs, err := dbos.ListWorkflows(e.client,
		dbos.WithFilterName(workflowName, approvalWorkflowName, afterWorkflowName),
		dbos.WithFilterStatus(dbos.WorkflowStatusPending, dbos.WorkflowStatusEnqueued, dbos.WorkflowStatusDelayed),
		dbos.WithFilterSortDesc(), dbos.WithFilterLimit(maxActive),
		dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false))
	if err != nil {
		return nil, err
	}
	a := &Activity{Running: []Status{}, AwaitingApproval: []Status{}, WaitingDependency: []Status{}, Queued: []Status{}, Delayed: []Status{}, Truncated: len(wfs) == maxActive}
	match := ListFilter{Application: f.Application, Environment: f.Environment, Tenant: f.Tenant, Match: f.Match}
	detailed := 0
	for _, w := range wfs {
		s := toStatus(w)
		if !match.Matches(s) {
			continue
		}
		switch {
		case detailed >= maxPendingChecked:
		case w.Name == workflowName && w.Status == dbos.WorkflowStatusPending:
			// Running deploy: the workflow is already read, only rollout and last step are needed.
			detailed++
			e.readRollout(&s, w)
			if s.State == StatePending {
				s.LastStep = e.lastStep(w.ID)
			}
		case w.Status == dbos.WorkflowStatusPending || w.Name == afterWorkflowName:
			// Approval or dependency wait: the full state reports the details.
			detailed++
			if full, err := e.Status(ctx, s.ID); err == nil {
				s = *full
			}
		}
		switch s.State {
		case StatePending:
			a.Running = append(a.Running, s)
		case StateAwaitingApproval:
			a.AwaitingApproval = append(a.AwaitingApproval, s)
		case StateWaitingDependency:
			a.WaitingDependency = append(a.WaitingDependency, s)
		case StateEnqueued:
			a.Queued = append(a.Queued, s)
		case StateDelayed:
			a.Delayed = append(a.Delayed, s)
		}
	}
	// Queued and scheduled: whoever starts first comes first.
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

// lastStep returns the name of the workflow's last recorded step, or "" if there are none.
func (e *Engine) lastStep(id string) string {
	steps, err := dbos.GetWorkflowSteps(e.client, id, dbos.WithStepsLoadOutput(false))
	if err != nil || len(steps) == 0 {
		return ""
	}
	return steps[len(steps)-1].StepName
}

var _ service.PlatformService = (*Engine)(nil)
