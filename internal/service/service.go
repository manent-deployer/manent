// Package service is the contract between manent clients and server: service interfaces,
// requests, responses and errors. It depends neither on DBOS nor on the database, so the
// client (cmd/manent, internal/apiclient) stays light: engine.Engine implements it on the
// server and apiclient.Client on top of the HTTP API.
package service

import (
	"context"
	"errors"
	"time"

	"github.com/manent-deployer/manent/internal/descriptor"
	"github.com/manent-deployer/manent/internal/governance"
	"github.com/manent-deployer/manent/internal/pipeline"
)

// States of a Submission and of a deploy besides the workflow ones.
const (
	// StateScheduled is a recurring schedule created or updated.
	StateScheduled = "SCHEDULED"
	// StateAwaitingApproval is a deploy waiting for the required approvals.
	StateAwaitingApproval = "AWAITING_APPROVAL"
	// StateRejected is a deploy rejected by an approver.
	StateRejected = "REJECTED"
	// StateExpired is a deploy not approved within the timeout.
	StateExpired = "EXPIRED"
)

// Workflow states exposed by the service (the same as DBOS).
const (
	StatePending   = "PENDING"
	StateEnqueued  = "ENQUEUED"
	StateDelayed   = "DELAYED"
	StateSuccess   = "SUCCESS"
	StateError     = "ERROR"
	StateCancelled = "CANCELLED"
	// StateMaxRecoveryAttempts is a workflow no longer resumed after too many attempts.
	StateMaxRecoveryAttempts = "MAX_RECOVERY_ATTEMPTS_EXCEEDED"
)

// Terminal reports the final states of a deploy.
func Terminal(state string) bool {
	switch state {
	case StateSuccess, StateError, StateCancelled, StateMaxRecoveryAttempts, StateRejected, StateExpired, StateDependencyFailed:
		return true
	}
	return false
}

// Deploy kinds, recorded in the state and in the inventory.
const (
	KindDeploy    = "deploy"
	KindPromotion = "promotion"
	KindRollback  = "rollback"
)

var (
	// ErrNotFound means a deploy or schedule that does not exist.
	ErrNotFound = errors.New("not found")
	// ErrInvalidRequest means an unacceptable request (e.g. a time in the past).
	ErrInvalidRequest = errors.New("invalid request")
	// ErrForbidden means an action not allowed by the user's permissions.
	ErrForbidden = errors.New("permission denied")
	// ErrUnavailable means a temporary condition (e.g. no governance policy loaded): retry later.
	ErrUnavailable = errors.New("service unavailable")
)

// Service is the entry point used by CLI and API.
type Service interface {
	DeploymentService
	ScheduleService
	GovernanceService
	InsightService
	AccessService
	PlatformService
	ReleaseService
	PolicyService
	TenantService
}

// DeploymentService manages deploys, immediate or scheduled.
type DeploymentService interface {
	// Plan computes the steps that would run, without starting the deploy.
	Plan(ctx context.Context, d *descriptor.Descriptor) (pipeline.Plan, error)
	// Validate checks the descriptor as at submission and returns the effective deploy, without
	// starting it.
	Validate(ctx context.Context, d *descriptor.Descriptor, opts ValidateOptions) (*Validation, error)
	// ValidateEnvironment checks a candidate environment configuration, before it is applied, as
	// a deploy would use it at submission.
	ValidateEnvironment(ctx context.Context, c EnvironmentCheck) (*Validation, error)
	// Submit enqueues a deploy, schedules it at a time or creates a recurring schedule.
	Submit(ctx context.Context, d *descriptor.Descriptor) (*Submission, error)
	// Status returns the current state of a deploy.
	Status(ctx context.Context, id string) (*Status, error)
	// List lists the most recent deploys, optionally only those of a schedule.
	List(ctx context.Context, f ListFilter) ([]Status, error)
	// Wait waits for a deploy to end and returns its result.
	Wait(ctx context.Context, id string) (*Result, error)
	// Cancel cancels a waiting or running deploy.
	Cancel(ctx context.Context, id string) error
}

// ScheduleService manages the schedules of recurring deploys.
type ScheduleService interface {
	ListSchedules(ctx context.Context) ([]ScheduleInfo, error)
	GetSchedule(ctx context.Context, name string) (*ScheduleInfo, error)
	PauseSchedule(ctx context.Context, name string) error
	ResumeSchedule(ctx context.Context, name string) error
	DeleteSchedule(ctx context.Context, name string) error
}

// GovernanceService manages inventory, promotions, manual rollbacks, approvals and freezes.
type GovernanceService interface {
	// Inventory returns the release in service in every application/environment.
	Inventory(ctx context.Context, f InventoryFilter) ([]InventoryEntry, error)
	// Promote releases to an environment the same artifact already released to another one.
	Promote(ctx context.Context, r PromoteRequest) (*Submission, error)
	// Rollback releases a previous version again.
	Rollback(ctx context.Context, r RollbackRequest) (*Submission, error)
	// Decide approves or rejects a deploy awaiting approval.
	Decide(ctx context.Context, id string, r DecisionRequest) error
	// Freezes lists the active and future freeze windows.
	Freezes(ctx context.Context, application, environment string) ([]governance.WindowStatus, error)
	// Approvals lists the deploys waiting for a decision: before the deploy or between two waves.
	// Through permissions, only those the identity can approve.
	Approvals(ctx context.Context) ([]Status, error)
}

// InsightService exposes timeline, logs and DORA metrics of deploys.
type InsightService interface {
	// Steps returns the recorded steps of a deploy (or of its approval, if the deploy has not
	// started yet), with times, outputs and errors.
	Steps(ctx context.Context, id string) ([]StepRecord, error)
	// Logs returns the deploy log lines from offset.
	Logs(ctx context.Context, id string, offset int) (*LogPage, error)
	// DORA computes the DORA metrics from the deploy history.
	DORA(ctx context.Context, f DORAFilter) (*DORAReport, error)
}

// Validation is the result of Validate: the plan and the effective deploy, after merging
// with the environment configuration and applying the policies.
type Validation struct {
	Valid    bool                 `json:"valid"`
	Plan     pipeline.Plan        `json:"plan"`
	Rollback *descriptor.Rollback `json:"rollback,omitempty"`
	Deploy   *descriptor.Deploy   `json:"deploy,omitempty"`
	// Secrets are the names of the declared secrets, never the references or values.
	Secrets []string `json:"secrets,omitempty"`
	// Config describes the environment configuration used (source and revision).
	Config     map[string]string     `json:"config,omitempty"`
	Governance *GovernanceValidation `json:"governance,omitempty"`
	// Rollout is the wave plan (stages and passes) that would run.
	Rollout *RolloutState `json:"rollout,omitempty"`
	// Artifact is the outcome of the artifact check (ValidateOptions.CheckArtifact).
	Artifact *ArtifactCheck `json:"artifact,omitempty"`
	// Warnings reports the checks not performed, for example with Consul unreachable.
	Warnings []string `json:"warnings,omitempty"`
}

// ValidateOptions are the optional checks of Validate.
type ValidateOptions struct {
	// CheckArtifact also checks, from the API server, that the artifact exists at its source
	// and that the source credentials give access to it, without downloading it.
	CheckArtifact bool `json:"check_artifact,omitempty"`
}

// ArtifactCheck describes the artifact found at its source.
type ArtifactCheck struct {
	Source string `json:"source"`
	// SHA256 is the digest read from the source: only for registry images (the manifest).
	SHA256 string `json:"sha256,omitempty"`
	// Size is the size declared by the source, omitted if unknown.
	Size int64 `json:"size,omitempty"`
	// SHA256Verified reports whether the expected artifact.sha256 was compared with the one
	// of the source; for files and URLs that requires the download, left to the worker.
	SHA256Verified bool `json:"sha256_verified"`
}

// EnvironmentCheck asks to check a candidate environment configuration (e.g. a change in the
// repository or in Terraform) before it reaches the configuration system.
type EnvironmentCheck struct {
	// Tenant chooses among the tenants that list the application, required only when more
	// than one does.
	Tenant      string `json:"tenant,omitempty"`
	Application string `json:"application"`
	// Artifact is the artifact the configuration is for (templates with {artifact}); it chooses
	// the deploy in service used as example.
	Artifact    string `json:"artifact,omitempty"`
	Environment string `json:"environment"`
	// Config is the configuration in YAML or JSON, as it would be stored in the provider.
	Config string `json:"config"`
	// Descriptor is the example deploy merged with the configuration; default the request of
	// the deploy in service in the environment.
	Descriptor *descriptor.Descriptor `json:"descriptor,omitempty"`
}

// GovernanceValidation are the governance rules that would apply to the deploy.
type GovernanceValidation struct {
	// Revision is the governance policy that produced the decision.
	Revision  string                   `json:"revision,omitempty"`
	Rule      string                   `json:"rule,omitempty"`
	Requires  []string                 `json:"requires,omitempty"`
	Approvals int                      `json:"approvals"`
	Approvers *governance.Approvers    `json:"approvers,omitempty"`
	Change    *governance.Change       `json:"change,omitempty"`
	Freeze    *governance.WindowStatus `json:"freeze,omitempty"`
	// Deny are the reasons the policy would refuse the deploy.
	Deny  []governance.Denial `json:"deny,omitempty"`
	Notes []string            `json:"notes,omitempty"`
	// Error is a policy evaluation failure: the deploy would be refused.
	Error string `json:"error,omitempty"`
}

// StepResult is the outcome of a run step.
type StepResult struct {
	Name   string          `json:"name"`
	Output pipeline.Output `json:"output"`
}

// Result is the output of the deploy workflow.
type Result struct {
	DeploymentID string `json:"deployment_id"`
	// DryRun means a simulation: no changes to production environments.
	DryRun bool         `json:"dry_run,omitempty"`
	Steps  []StepResult `json:"steps"`
	// Rollout is the final state of the wave release, if the deploy has one.
	Rollout *RolloutState `json:"rollout,omitempty"`
	// Change is the deploy's change management ticket, if any.
	Change *ChangeTicket `json:"change,omitempty"`
	// Exports are the values the deploy offers to the deploys importing it (descriptor.Export).
	Exports map[string]string `json:"exports,omitempty"`
	// PublicExports are the exports other tenants can import.
	PublicExports []string `json:"public_exports,omitempty"`
}

// Submission describes the outcome of Submit.
type Submission struct {
	// ID of the deploy; empty for recurring schedules.
	ID string `json:"id,omitempty"`
	// State is ENQUEUED, DELAYED (scheduled single deploy) or SCHEDULED (recurring).
	State       string     `json:"state"`
	ScheduledAt *time.Time `json:"scheduled_at,omitempty"`
	Schedule    string     `json:"schedule,omitempty"`
	DryRun      bool       `json:"dry_run,omitempty"`
}

// Status describes the state of a deploy.
type Status struct {
	ID          string `json:"id"`
	State       string `json:"state"`
	Application string `json:"application,omitempty"`
	Environment string `json:"environment,omitempty"`
	// Tenant is the tenant of the deploy, if any.
	Tenant   string `json:"tenant,omitempty"`
	Artifact string `json:"artifact,omitempty"`
	Version  string `json:"version,omitempty"`
	// SHA256 is the hash of the released artifact, recorded at the end of the deploy.
	SHA256 string `json:"sha256,omitempty"`
	// ConfigRevision is the commit SHA of the environment configuration (git provider),
	// recorded at the end of the deploy.
	ConfigRevision string `json:"config_revision,omitempty"`
	Kind           string `json:"kind,omitempty"`
	Source         string `json:"source,omitempty"`
	DryRun         bool   `json:"dry_run,omitempty"`
	// Rollback is the outcome of the automatic rollback, if run.
	Rollback    string     `json:"rollback,omitempty"`
	RequestedBy string     `json:"requested_by,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	ScheduledAt *time.Time `json:"scheduled_at,omitempty"`
	// StartedAt is the start of execution on the worker (absent while the deploy is queued).
	StartedAt *time.Time `json:"started_at,omitempty"`
	// Executor is the worker running or that ran the deploy; Queue the queue.
	Executor string `json:"executor,omitempty"`
	Queue    string `json:"queue,omitempty"`
	// LastStep is the last recorded step of a running deploy (only in activity).
	LastStep string         `json:"last_step,omitempty"`
	Approval *ApprovalState `json:"approval,omitempty"`
	// Change is the deploy's change management ticket, if any.
	Change *ChangeTicket `json:"change,omitempty"`
	// After is the state of the dependencies, if the deploy has any.
	After *AfterState `json:"after,omitempty"`
	// Release is the composite release the deploy belongs to.
	Release string `json:"release,omitempty"`
	// Policy is the governance decision recorded at submission (revision and rule).
	Policy *governance.Snapshot `json:"policy,omitempty"`
	// Rollout is the state of the wave release, if the deploy has one.
	Rollout *RolloutState `json:"rollout,omitempty"`
	// RolloutProgress summarizes the rollout in listings: "stages passed/total stages".
	RolloutProgress string `json:"rollout_progress,omitempty"`
	// Exports are the values a successful deploy offers to the deploys importing it;
	// PublicExports those other tenants can import.
	Exports       map[string]string `json:"exports,omitempty"`
	PublicExports []string          `json:"public_exports,omitempty"`
	Result        *Result           `json:"result,omitempty"`
	Error         string            `json:"error,omitempty"`
}

// ListFilter filters the deploy list.
type ListFilter struct {
	Schedule    string `json:"schedule,omitempty"`
	Application string `json:"application,omitempty"`
	Environment string `json:"environment,omitempty"`
	// Tenant filters by tenant.
	Tenant string `json:"tenant,omitempty"`
	// Artifact filters by artifact name.
	Artifact string `json:"artifact,omitempty"`
	// Selector filters the applications by an application selector (tags, application,
	// tenant, tenant.<tag>): applied by the server, which knows the tags.
	Selector string `json:"selector,omitempty"`
	// State filters by state (e.g. SUCCESS, ERROR, AWAITING_APPROVAL).
	State string `json:"state,omitempty"`
	Limit int    `json:"limit,omitempty"`
	// Cursor is the ID of the last deploy of the previous page: the listing restarts from the next one.
	Cursor string `json:"cursor,omitempty"`
	// Match, if set, is an extra filter (e.g. permissions): the listing goes on with older
	// deploys until it reaches Limit, so the filter does not reduce the number of results.
	Match func(Status) bool `json:"-"`
}

// Matches applies the filters to a state.
func (f ListFilter) Matches(s Status) bool {
	return (f.Application == "" || s.Application == f.Application) &&
		(f.Environment == "" || s.Environment == f.Environment) &&
		(f.Tenant == "" || s.Tenant == f.Tenant) &&
		(f.Artifact == "" || s.Artifact == f.Artifact) &&
		(f.State == "" || s.State == f.State) &&
		(f.Match == nil || f.Match(s))
}

// ScheduleInfo describes a recurring schedule.
type ScheduleInfo struct {
	Name        string `json:"name"`
	Cron        string `json:"cron"`
	Timezone    string `json:"timezone,omitempty"`
	Status      string `json:"status"`
	Tenant      string `json:"tenant,omitempty"`
	Application string `json:"application"`
	Environment string `json:"environment"`
	// RequestedBy is who last created or updated the schedule.
	RequestedBy string     `json:"requested_by,omitempty"`
	LastFiredAt *time.Time `json:"last_fired_at,omitempty"`
}
