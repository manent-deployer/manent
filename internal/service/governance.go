package service

import (
	"time"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/governance"
)

// Outcomes of an approval.
const (
	ApprovalPending  = "pending"
	ApprovalApproved = "approved"
	ApprovalRejected = "rejected"
	ApprovalExpired  = "expired"
)

// Decision is the decision of an approver.
type Decision struct {
	Approver string    `json:"approver"`
	Approve  bool      `json:"approve"`
	Comment  string    `json:"comment,omitempty"`
	Time     time.Time `json:"time"`
}

// DecisionRequest is the decision requested through CLI or API: the approver is the identity
// in the context.
type DecisionRequest struct {
	Approve bool   `json:"approve"`
	Comment string `json:"comment,omitempty"`
}

// ApprovalState is the state of an approval.
type ApprovalState struct {
	Status    string `json:"status"`
	Required  int    `json:"required"`
	AllowSelf bool   `json:"allow_self,omitempty"`
	// Approvers, if set, limits who can approve (groups or subjects).
	Approvers *governance.Approvers `json:"approvers,omitempty"`
	ExpiresAt time.Time             `json:"expires_at"`
	Decisions []Decision            `json:"decisions,omitempty"`
	// Change is the change ticket to approve in the ITSM, if required.
	Change *ChangeTicket `json:"change,omitempty"`
}

// ChangeTicket is the change management ticket of a deploy.
type ChangeTicket struct {
	// ID is the technical identifier in the ITSM (sys_id, issue id).
	ID     string `json:"id,omitempty"`
	Number string `json:"number"`
	URL    string `json:"url,omitempty"`
	// Approval is the approval state in the ITSM: pending, approved, rejected.
	Approval string `json:"approval,omitempty"`
}

// InventoryFilter filters the inventory.
type InventoryFilter struct {
	Application string `json:"application,omitempty"`
	Environment string `json:"environment,omitempty"`
	// Tenant filters by tenant; without it the inventory has an entry per tenant.
	Tenant string `json:"tenant,omitempty"`
	// Artifact filters by artifact name.
	Artifact string `json:"artifact,omitempty"`
	// Selector filters the applications by an application selector (tags, application,
	// tenant, tenant.<tag>).
	Selector string `json:"selector,omitempty"`
}

// InventoryEntry is the state of an artifact of an application in an environment.
type InventoryEntry struct {
	Application string `json:"application"`
	// Artifact is the artifact name of the entry.
	Artifact    string `json:"artifact"`
	Environment string `json:"environment"`
	// Tenant is the tenant of the entry ("" for the untenanted deploys).
	Tenant string `json:"tenant,omitempty"`
	// Current is the last successfully ended deploy.
	Current *Status `json:"current,omitempty"`
	// LastFailure is the last failed or cancelled deploy, if after Current: without a completed
	// rollback the environment may not be in Current's state.
	LastFailure *Status `json:"last_failure,omitempty"`
	// Drift is a rollout stopped after updating part of the regions or AZs: the environment has
	// different versions (details in LastFailure.RolloutProgress).
	Drift bool `json:"drift,omitempty"`
	// Targets is the state read from the targets by the last reconciliation (reconcile.interval),
	// if it refers to Current.
	Targets *TargetsState `json:"targets,omitempty"`
	// OutOfSync means that at least one target is not on Current's version (state drifted or
	// missing): something changed it outside manent.
	OutOfSync bool `json:"out_of_sync,omitempty"`
}

// TargetsState is the state of an environment's targets read by the reconciliation.
type TargetsState struct {
	CheckedAt time.Time `json:"checked_at"`
	// Deployment and Version are the deploy in service and its version, the targets were
	// compared with.
	Deployment string        `json:"deployment"`
	Version    string        `json:"version"`
	Targets    []TargetState `json:"targets,omitempty"`
	// Error is a failure reading the whole environment (configuration, plugin).
	Error string `json:"error,omitempty"`
}

// TargetState is the release read from a target.
type TargetState struct {
	Host   string `json:"host"`
	Region string `json:"region,omitempty"`
	AZ     string `json:"az,omitempty"`
	Slot   string `json:"slot,omitempty"`
	// Version is the version in service on the target; empty if none or unreadable.
	Version string `json:"version,omitempty"`
	State   string `json:"state"`
	Detail  string `json:"detail,omitempty"`
}

// Target states.
const (
	// TargetInSync: the target runs the version in service.
	TargetInSync = "in-sync"
	// TargetDrifted: the target runs another version.
	TargetDrifted = "drifted"
	// TargetMissing: the target has no release.
	TargetMissing = "missing"
	// TargetStandby: in a slot rollout, the slot without traffic (the replaced version).
	TargetStandby = "standby"
	// TargetUnknown: the target could not be read.
	TargetUnknown = "unknown"
)

// Changed reports a target state that means a change outside manent.
func (t TargetState) Changed() bool { return t.State == TargetDrifted || t.State == TargetMissing }

// Redeploy are the options shared by promotions and manual rollbacks.
type Redeploy struct {
	// ID of the new deploy, optional (idempotency key).
	ID     string `json:"id,omitempty"`
	DryRun bool   `json:"dry_run,omitempty"`
	// FreezeOverride, if set, is the reason for overriding an active freeze.
	FreezeOverride string `json:"freeze_override,omitempty"`
}

// PromoteRequest asks to release to To the artifact already successfully released to From.
type PromoteRequest struct {
	Application string `json:"application"`
	From        string `json:"from"`
	To          string `json:"to"`
	// Artifact chooses the artifact to promote, required when From has more than one.
	Artifact string `json:"artifact,omitempty"`
	// Version, if given, chooses the version to promote; default the one in service in From.
	Version string `json:"version,omitempty"`
	// Deploy replaces the deploy section of the source descriptor; without it, the source
	// targets are not copied and come from To's environment configuration.
	Deploy *descriptor.Deploy `json:"deploy,omitempty"`
	Redeploy
}

// RollbackRequest asks to go back to a previously released version.
type RollbackRequest struct {
	Application string `json:"application"`
	Environment string `json:"environment"`
	// Artifact chooses the artifact to roll back, required when the environment has more than one.
	Artifact string `json:"artifact,omitempty"`
	// Version, if given, is the version to go back to; default the one before the version in service.
	Version string `json:"version,omitempty"`
	Redeploy
}
