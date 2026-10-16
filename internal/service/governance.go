package service

import (
	"time"

	"github.com/joeg-ita/manent/internal/descriptor"
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
	Status    string     `json:"status"`
	Required  int        `json:"required"`
	AllowSelf bool       `json:"allow_self,omitempty"`
	ExpiresAt time.Time  `json:"expires_at"`
	Decisions []Decision `json:"decisions,omitempty"`
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
}

// InventoryEntry is the state of an application in an environment.
type InventoryEntry struct {
	Application string `json:"application"`
	Environment string `json:"environment"`
	// Current is the last successfully ended deploy.
	Current *Status `json:"current,omitempty"`
	// LastFailure is the last failed or cancelled deploy, if after Current: without a completed
	// rollback the environment may not be in Current's state.
	LastFailure *Status `json:"last_failure,omitempty"`
	// Drift is a rollout stopped after updating part of the regions or AZs: the environment has
	// different versions (details in LastFailure.RolloutProgress).
	Drift bool `json:"drift,omitempty"`
}

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
	// Version, if given, is the version to go back to; default the one before the version in service.
	Version string `json:"version,omitempty"`
	Redeploy
}
