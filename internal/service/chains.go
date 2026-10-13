package service

import (
	"context"
	"time"

	"github.com/joeg-ita/manent/internal/descriptor"
)

// Stati legati alle dipendenze tra deploy.
const (
	// StateWaitingDependency indica un deploy che attende la conclusione dei deploy in after.
	StateWaitingDependency = "WAITING_DEPENDENCY"
	// StateDependencyFailed indica un deploy non eseguito: una dipendenza non è riuscita o
	// l'attesa è scaduta.
	StateDependencyFailed = "DEPENDENCY_FAILED"
)

// Esiti dell'attesa delle dipendenze.
const (
	AfterWaiting   = "waiting"
	AfterSatisfied = "satisfied"
	AfterFailed    = "failed"
	AfterExpired   = "expired"
)

// AfterState è lo stato dell'attesa delle dipendenze di un deploy.
type AfterState struct {
	Status       string            `json:"status"`
	ExpiresAt    time.Time         `json:"expires_at"`
	Dependencies []DependencyState `json:"dependencies"`
	// Reason spiega un'attesa fallita (dipendenza non riuscita o scaduta).
	Reason string `json:"reason,omitempty"`
}

// DependencyState è lo stato di un deploy da cui un altro dipende.
type DependencyState struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

// ReleaseService gestisce le release composte: più deploy con dipendenze tra loro.
type ReleaseService interface {
	// SubmitRelease accoda tutti i deploy della release, ciascuno in attesa dei propri
	// predecessori, e il workflow che ne segue l'esito e applica onFailure.
	SubmitRelease(ctx context.Context, r *descriptor.Release) (*ReleaseStatus, error)
	// ReleaseStatus restituisce lo stato della release e dei suoi deploy.
	ReleaseStatus(ctx context.Context, id string) (*ReleaseStatus, error)
	// ListReleases elenca le release più recenti.
	ListReleases(ctx context.Context, limit int) ([]ReleaseStatus, error)
	// CancelRelease annulla i deploy non conclusi e la release.
	CancelRelease(ctx context.Context, id string) error
}

// ReleaseStatus è lo stato di una release.
type ReleaseStatus struct {
	ID string `json:"id"`
	// State è PENDING finché la release è in corso, poi SUCCESS, ERROR o CANCELLED.
	State       string               `json:"state"`
	OnFailure   string               `json:"on_failure"`
	RequestedBy string               `json:"requested_by,omitempty"`
	CreatedAt   time.Time            `json:"created_at"`
	UpdatedAt   time.Time            `json:"updated_at"`
	Deploys     []ReleaseDeployState `json:"deploys"`
	Error       string               `json:"error,omitempty"`
}

// ReleaseDeployState è lo stato di un deploy della release.
type ReleaseDeployState struct {
	Name         string   `json:"name"`
	DeploymentID string   `json:"deployment_id"`
	Application  string   `json:"application"`
	Environment  string   `json:"environment"`
	Version      string   `json:"version,omitempty"`
	After        []string `json:"after,omitempty"`
	State        string   `json:"state"`
	Error        string   `json:"error,omitempty"`
	// RollbackID e RollbackState descrivono il rollback eseguito con onFailure: rollback.
	RollbackID    string `json:"rollback_id,omitempty"`
	RollbackState string `json:"rollback_state,omitempty"`
}
