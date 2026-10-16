package service

import (
	"context"
	"time"
)

// PlatformService exposes the platform state: the registered workers with the work they are
// doing and the active deploys (running, queued, scheduled, awaiting approval).
type PlatformService interface {
	// Workers lists the registered workers and those with pending workflows.
	Workers(ctx context.Context) ([]WorkerInfo, error)
	// Activity returns the deploys not yet ended, grouped by state.
	Activity(ctx context.Context, f ActivityFilter) (*Activity, error)
}

// Worker states.
const (
	// WorkerAlive is a worker that sends heartbeats.
	WorkerAlive = "alive"
	// WorkerStale is a worker that sends no heartbeats: it crashed or is unreachable.
	WorkerStale = "stale"
	// WorkerStopped is a worker stopped in an orderly way.
	WorkerStopped = "stopped"
	// WorkerUnregistered is an executor with pending workflows but missing from the registry.
	WorkerUnregistered = "unregistered"
)

// WorkerInfo is a worker (DBOS executor) with the assigned work.
type WorkerInfo struct {
	ID string `json:"id"`
	// State is alive, stale, stopped or unregistered.
	State string `json:"state"`
	// Version is the workflow version (app.version): a worker resumes only its own.
	Version  string `json:"version,omitempty"`
	Build    string `json:"build,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	// Region is the worker's region (dbos.region), if configured.
	Region      string     `json:"region,omitempty"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	HeartbeatAt *time.Time `json:"heartbeat_at,omitempty"`
	StoppedAt   *time.Time `json:"stopped_at,omitempty"`
	// RecoveredBy is who resumed the worker's workflows after it was lost.
	RecoveredBy string     `json:"recovered_by,omitempty"`
	RecoveredAt *time.Time `json:"recovered_at,omitempty"`
	// Pending counts the worker's pending workflows per workflow version.
	Pending map[string]int64 `json:"pending,omitempty"`
	// Tasks are the workflows running on the worker that the identity can read.
	Tasks []WorkerTask `json:"tasks"`
	// HiddenTasks counts the running workflows of unreadable applications/environments.
	HiddenTasks int `json:"hidden_tasks,omitempty"`
}

// Kinds of worker work.
const (
	TaskDeploy   = "deploy"
	TaskPass     = "pass"
	TaskGate     = "gate"
	TaskApproval = "approval"
	TaskSchedule = "schedule"
	// TaskAfter is a deploy's dependency wait; TaskRelease a composite release.
	TaskAfter   = "after"
	TaskRelease = "release"
)

// WorkerTask is a workflow running on a worker.
type WorkerTask struct {
	// ID is the workflow ID; DeploymentID the deploy it belongs to.
	ID           string `json:"id"`
	DeploymentID string `json:"deployment_id"`
	// Kind is deploy, pass (rollout pass in a region), gate, approval, schedule, after
	// (dependency wait) or release.
	Kind        string     `json:"kind"`
	Application string     `json:"application,omitempty"`
	Environment string     `json:"environment,omitempty"`
	Region      string     `json:"region,omitempty"`
	Version     string     `json:"version,omitempty"`
	Queue       string     `json:"queue,omitempty"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	// Attempts is the number of runs of the workflow: more than 1 after a recovery.
	Attempts int `json:"attempts,omitempty"`
}

// ActivityFilter filters the active deploys.
type ActivityFilter struct {
	Application string `json:"application,omitempty"`
	Environment string `json:"environment,omitempty"`
	// Match, if set, is an extra filter (e.g. permissions), applied before reading the details
	// of running deploys.
	Match func(Status) bool `json:"-"`
}

// Activity are the unfinished deploys, from the most recent.
type Activity struct {
	// Running are the running deploys, with worker and last recorded step.
	Running []Status `json:"running"`
	// AwaitingApproval are the deploys waiting for a decision, before the deploy or between two waves.
	AwaitingApproval []Status `json:"awaiting_approval"`
	// WaitingDependency are the deploys waiting for the deploys they depend on to succeed.
	WaitingDependency []Status `json:"waiting_dependency"`
	// Queued are the queued deploys, waiting for a free slot.
	Queued []Status `json:"queued"`
	// Delayed are the deploys scheduled with schedule.at.
	Delayed []Status `json:"delayed"`
	// Truncated means there were more active deploys than examined.
	Truncated bool `json:"truncated,omitempty"`
}
