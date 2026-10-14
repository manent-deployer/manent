package engine

import (
	"github.com/joeg-ita/manent/internal/service"
)

// Il contratto del servizio sta in internal/service, senza dipendenze da DBOS, così il client
// non le eredita. Gli alias permettono al codice del server di continuare a usare i nomi di engine.

// Stati, tipi di deploy ed esiti delle approvazioni.
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

// Stati dei workflow (gli stessi di DBOS).
const (
	StatePending   = service.StatePending
	StateEnqueued  = service.StateEnqueued
	StateDelayed   = service.StateDelayed
	StateSuccess   = service.StateSuccess
	StateError     = service.StateError
	StateCancelled = service.StateCancelled
)

// Errori del servizio.
var (
	ErrNotFound       = service.ErrNotFound
	ErrInvalidRequest = service.ErrInvalidRequest
	ErrForbidden      = service.ErrForbidden
)

// Terminal indica gli stati finali di un deploy.
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
	Redeploy             = service.Redeploy
	PromoteRequest       = service.PromoteRequest
	RollbackRequest      = service.RollbackRequest
	StepRecord           = service.StepRecord
	DORAFilter           = service.DORAFilter
	DORAMetrics          = service.DORAMetrics
	DORAReport           = service.DORAReport
)
