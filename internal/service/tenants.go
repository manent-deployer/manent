package service

import (
	"context"
	"time"
)

// KindTenants is the kind of a multi-tenant release (Status.Kind).
const KindTenants = "tenants"

// States of a multi-tenant release (TenantsRunState.Status).
const (
	TenantsRunning        = "running"
	TenantsBaking         = "baking"
	TenantsPausedApproval = "paused-approval"
	TenantsSucceeded      = "succeeded"
	TenantsFailed         = "failed"
	TenantsStopped        = "stopped"
)

// States of a tenant in a multi-tenant release that are not deploy states.
const (
	TenantPending = "PENDING_START"
	TenantSkipped = "SKIPPED"
	TenantDenied  = "DENIED"
)

// TenantService exposes the tenant registry.
type TenantService interface {
	// Tenants lists the tenants, optionally connected to an application/environment and
	// matching a selector.
	Tenants(ctx context.Context, f TenantFilter) ([]TenantInfo, error)
	// Tenant returns a tenant.
	Tenant(ctx context.Context, name string) (*TenantInfo, error)
}

// TenantFilter filters the tenants.
type TenantFilter struct {
	Application string `json:"application,omitempty"`
	Environment string `json:"environment,omitempty"`
	Selector    string `json:"selector,omitempty"`
}

// TenantInfo is a tenant as exposed by the API: parameters and secret names, never the
// secret references.
type TenantInfo struct {
	Name         string                       `json:"name"`
	Description  string                       `json:"description,omitempty"`
	Tags         map[string]string            `json:"tags,omitempty"`
	Applications map[string][]string          `json:"applications,omitempty"`
	Params       map[string]map[string]string `json:"params,omitempty"`
	Secrets      map[string][]string          `json:"secrets,omitempty"`
	Disabled     bool                         `json:"disabled,omitempty"`
	Source       string                       `json:"source,omitempty"`
}

// TenantSkip is a tenant left out of a multi-tenant release, with the reason.
type TenantSkip struct {
	Tenant string `json:"tenant"`
	Reason string `json:"reason"`
}

// TenantsRunState is the state of a multi-tenant release.
type TenantsRunState struct {
	ID          string `json:"id"`
	Application string `json:"application"`
	Environment string `json:"environment"`
	Artifact    string `json:"artifact,omitempty"`
	Version     string `json:"version,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Status      string `json:"status"`
	// Group is the group in progress; Paused explains a pause.
	Group       string             `json:"group,omitempty"`
	Paused      string             `json:"paused,omitempty"`
	MaxParallel int                `json:"max_parallel"`
	OnFailure   string             `json:"on_failure"`
	Groups      []TenantGroupState `json:"groups"`
	Tenants     []TenantRunEntry   `json:"tenants"`
	// Approval is the approval asked between two groups, while the release waits for it.
	Approval  *ApprovalState `json:"approval,omitempty"`
	Error     string         `json:"error,omitempty"`
	UpdatedAt time.Time      `json:"updated_at,omitzero"`
}

// TenantGroupState is a group of a multi-tenant release.
type TenantGroupState struct {
	Name string `json:"name"`
	Bake string `json:"bake,omitempty"`
	// State is pending, running, baking, done or skipped.
	State string `json:"state"`
}

// TenantRunEntry is the deploy of a tenant in a multi-tenant release.
type TenantRunEntry struct {
	Tenant       string `json:"tenant"`
	Group        string `json:"group"`
	DeploymentID string `json:"deployment_id,omitempty"`
	// State is the state of the deploy, or PENDING_START, SKIPPED, DENIED.
	State  string `json:"state"`
	Error  string `json:"error,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Counts returns how many tenants succeeded, failed and are not finished.
func (r *TenantsRunState) Counts() (succeeded, failed, open int) {
	for _, t := range r.Tenants {
		switch {
		case t.State == StateSuccess:
			succeeded++
		case t.State == TenantSkipped || t.State == TenantDenied:
		case Terminal(t.State):
			failed++
		default:
			open++
		}
	}
	return
}
