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
	// Queues lists the DBOS queues of manent with their limits, the workflows they hold and the
	// workers that listen to them.
	Queues(ctx context.Context) ([]QueueInfo, error)
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
	// Queues are the DBOS queues the worker dequeues from.
	Queues []string `json:"queues,omitempty"`
	// Resources is the last resource sample of the worker, sent at every heartbeat.
	Resources *WorkerResources `json:"resources,omitempty"`
	// Pending counts the worker's pending workflows per workflow version.
	Pending map[string]int64 `json:"pending,omitempty"`
	// Tasks are the workflows running on the worker that the identity can read.
	Tasks []WorkerTask `json:"tasks"`
	// HiddenTasks counts the running workflows of unreadable applications/environments.
	HiddenTasks int `json:"hidden_tasks,omitempty"`
}

// WorkerResources describes the capacity and the use of a worker's host and process. Values
// that cannot be read on the worker's operating system are zero (and omitted).
type WorkerResources struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
	// CPUs is the number of logical CPUs of the host; CPULimit the CPUs the process may use
	// (GOMAXPROCS, which follows the container's CPU quota).
	CPUs     int `json:"cpus"`
	CPULimit int `json:"cpu_limit"`
	// MemoryTotal is the host memory; MemoryAvailable the memory still available on the host;
	// MemoryLimit the container's memory limit (cgroup), 0 without a limit. Bytes.
	MemoryTotal     uint64 `json:"memory_total_bytes,omitempty"`
	MemoryAvailable uint64 `json:"memory_available_bytes,omitempty"`
	MemoryLimit     uint64 `json:"memory_limit_bytes,omitempty"`
	// ProcessMemory is the resident memory of the worker process, in bytes.
	ProcessMemory uint64 `json:"process_memory_bytes,omitempty"`
	// ProcessCPU is the CPU used by the process since the previous sample, in cores (1.5 = one
	// and a half CPUs busy).
	ProcessCPU float64 `json:"process_cpu_cores"`
	// Load1, Load5 and Load15 are the host load averages (Linux).
	Load1  float64 `json:"load1,omitempty"`
	Load5  float64 `json:"load5,omitempty"`
	Load15 float64 `json:"load15,omitempty"`
	// Goroutines is the number of goroutines of the process.
	Goroutines int `json:"goroutines"`
	// WorkdirFree is the free space of the file system of the working directory, in bytes.
	WorkdirFree uint64 `json:"workdir_free_bytes,omitempty"`
	// Concurrency is the number of deploys the worker runs at the same time (dbos.concurrency).
	Concurrency int       `json:"concurrency,omitempty"`
	SampledAt   time.Time `json:"sampled_at"`
}

// Kinds of queues.
const (
	// QueueDeployments is the deploy queue, partitioned by application/environment.
	QueueDeployments = "deployments"
	// QueueApprovals holds approvals, dependency waits, releases and multi-tenant releases:
	// waits that take no deploy slot.
	QueueApprovals = "approvals"
	// QueueRegion holds the rollout passes of a region, run only by that region's workers.
	QueueRegion = "region"
	// QueueOther is a queue not created by manent (for example DBOS's internal queue).
	QueueOther = "other"
)

// QueueInfo is a DBOS queue with its limits and content.
type QueueInfo struct {
	Name string `json:"name"`
	// Kind is deployments, approvals, region or other.
	Kind   string `json:"kind"`
	Region string `json:"region,omitempty"`
	// WorkerConcurrency is the workflows of the queue a single worker runs at the same time,
	// GlobalConcurrency those on all workers, PartitionConcurrency those of a single
	// partition (application/environment): 0 means no limit.
	WorkerConcurrency    int `json:"worker_concurrency,omitempty"`
	GlobalConcurrency    int `json:"global_concurrency,omitempty"`
	PartitionConcurrency int `json:"partition_concurrency,omitempty"`
	// Enqueued, Running and Delayed count the workflows waiting for a slot, running and
	// scheduled for later.
	Enqueued int64 `json:"enqueued"`
	Running  int64 `json:"running"`
	Delayed  int64 `json:"delayed"`
	// OldestEnqueued is the creation time of the oldest workflow waiting for a slot.
	OldestEnqueued *time.Time `json:"oldest_enqueued,omitempty"`
	// Workers are the live workers that listen to the queue.
	Workers []string `json:"workers"`
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
	Tenant      string `json:"tenant,omitempty"`
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
