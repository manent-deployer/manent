package service

import (
	"context"
	"time"

	"github.com/joeg-ita/manent/internal/descriptor"
)

// States related to dependencies between deploys.
const (
	// StateWaitingDependency is a deploy waiting for the deploys in after to end.
	StateWaitingDependency = "WAITING_DEPENDENCY"
	// StateDependencyFailed is a deploy that did not run: a dependency failed or the wait expired.
	StateDependencyFailed = "DEPENDENCY_FAILED"
)

// Outcomes of the dependency wait.
const (
	AfterWaiting   = "waiting"
	AfterSatisfied = "satisfied"
	AfterFailed    = "failed"
	AfterExpired   = "expired"
)

// AfterState is the state of a deploy's dependency wait.
type AfterState struct {
	Status       string            `json:"status"`
	ExpiresAt    time.Time         `json:"expires_at"`
	Dependencies []DependencyState `json:"dependencies"`
	// Reason explains a failed wait (dependency failed or expired).
	Reason string `json:"reason,omitempty"`
}

// DependencyState is the state of a deploy another one depends on.
type DependencyState struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

// ReleaseService manages composite releases: several interdependent deploys.
type ReleaseService interface {
	// SubmitRelease enqueues all the release deploys, each waiting for its predecessors, and the
	// workflow that follows their outcome and applies onFailure.
	SubmitRelease(ctx context.Context, r *descriptor.Release) (*ReleaseStatus, error)
	// ReleaseStatus returns the state of the release and its deploys.
	ReleaseStatus(ctx context.Context, id string) (*ReleaseStatus, error)
	// ListReleases lists the most recent releases.
	ListReleases(ctx context.Context, limit int) ([]ReleaseStatus, error)
	// CancelRelease cancels the unfinished deploys and the release.
	CancelRelease(ctx context.Context, id string) error
}

// ReleaseStatus is the state of a release.
type ReleaseStatus struct {
	ID string `json:"id"`
	// State is PENDING while the release runs, then SUCCESS, ERROR or CANCELLED.
	State       string               `json:"state"`
	OnFailure   string               `json:"on_failure"`
	RequestedBy string               `json:"requested_by,omitempty"`
	CreatedAt   time.Time            `json:"created_at"`
	UpdatedAt   time.Time            `json:"updated_at"`
	Deploys     []ReleaseDeployState `json:"deploys"`
	Error       string               `json:"error,omitempty"`
}

// ReleaseDeployState is the state of a release deploy.
type ReleaseDeployState struct {
	Name         string   `json:"name"`
	DeploymentID string   `json:"deployment_id"`
	Application  string   `json:"application"`
	Artifact     string   `json:"artifact,omitempty"`
	Environment  string   `json:"environment"`
	Tenant       string   `json:"tenant,omitempty"`
	Version      string   `json:"version,omitempty"`
	After        []string `json:"after,omitempty"`
	State        string   `json:"state"`
	Error        string   `json:"error,omitempty"`
	// RollbackID and RollbackState describe the rollback run with onFailure: rollback.
	RollbackID    string `json:"rollback_id,omitempty"`
	RollbackState string `json:"rollback_state,omitempty"`
}
