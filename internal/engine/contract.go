package engine

import (
	"github.com/joeg-ita/manent/internal/service"
)

// The service contract lives in internal/service, without DBOS dependencies, so the client
// does not inherit them. The aliases let server code keep using the engine names.

// States, deploy kinds and approval outcomes.
const (
	StateScheduled         = service.StateScheduled
	StateAwaitingApproval  = service.StateAwaitingApproval
	StateRejected          = service.StateRejected
	StateExpired           = service.StateExpired
	StateWaitingDependency = service.StateWaitingDependency
	StateDependencyFailed  = service.StateDependencyFailed

	KindDeploy    = service.KindDeploy
	KindPromotion = service.KindPromotion
	KindRollback  = service.KindRollback

	ApprovalPending  = service.ApprovalPending
	ApprovalApproved = service.ApprovalApproved
	ApprovalRejected = service.ApprovalRejected
	ApprovalExpired  = service.ApprovalExpired
)

// Workflow states (the same as DBOS).
const (
	StatePending   = service.StatePending
	StateEnqueued  = service.StateEnqueued
	StateDelayed   = service.StateDelayed
	StateSuccess   = service.StateSuccess
	StateError     = service.StateError
	StateCancelled = service.StateCancelled
)

// Service errors.
var (
	ErrNotFound       = service.ErrNotFound
	ErrInvalidRequest = service.ErrInvalidRequest
	ErrForbidden      = service.ErrForbidden
	ErrUnavailable    = service.ErrUnavailable
)

// Terminal reports the final states of a deploy.
var Terminal = service.Terminal

type (
	Service              = service.Service
	DeploymentService    = service.DeploymentService
	ScheduleService      = service.ScheduleService
	GovernanceService    = service.GovernanceService
	InsightService       = service.InsightService
	AccessService        = service.AccessService
	PlatformService      = service.PlatformService
	WorkerInfo           = service.WorkerInfo
	WorkerResources      = service.WorkerResources
	QueueInfo            = service.QueueInfo
	WorkerTask           = service.WorkerTask
	ActivityFilter       = service.ActivityFilter
	Activity             = service.Activity
	ReleaseService       = service.ReleaseService
	ReleaseStatus        = service.ReleaseStatus
	ReleaseDeployState   = service.ReleaseDeployState
	AfterState           = service.AfterState
	DependencyState      = service.DependencyState
	Identity             = service.Identity
	Grant                = service.Grant
	Validation           = service.Validation
	EnvironmentCheck     = service.EnvironmentCheck
	GovernanceValidation = service.GovernanceValidation
	StepResult           = service.StepResult
	Result               = service.Result
	Submission           = service.Submission
	Status               = service.Status
	ListFilter           = service.ListFilter
	ScheduleInfo         = service.ScheduleInfo
	Decision             = service.Decision
	DecisionRequest      = service.DecisionRequest
	ApprovalState        = service.ApprovalState
	ChangeTicket         = service.ChangeTicket
	InventoryFilter      = service.InventoryFilter
	InventoryEntry       = service.InventoryEntry
	TargetsState         = service.TargetsState
	TargetState          = service.TargetState
	Redeploy             = service.Redeploy
	PromoteRequest       = service.PromoteRequest
	RollbackRequest      = service.RollbackRequest
	StepRecord           = service.StepRecord
	DORAFilter           = service.DORAFilter
	DORAMetrics          = service.DORAMetrics
	DORAReport           = service.DORAReport
	TenantService        = service.TenantService
	TenantFilter         = service.TenantFilter
	TenantInfo           = service.TenantInfo
	TenantApplication    = service.TenantApplication
)
