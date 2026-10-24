// Package authz applies permissions (RBAC) per application and environment. Guard wraps the
// engine service, so the same rules apply to the API and the CLI. The permission model lives in
// package rbac and is re-exported here.
package authz

import "github.com/manent-deployer/manent/internal/rbac"

// Types of the permission model (package rbac).
type (
	Action         = rbac.Action
	Binding        = rbac.Binding
	Policy         = rbac.Policy
	Grant          = rbac.Grant
	Authorizer     = rbac.Authorizer
	DenyRequest    = rbac.DenyRequest
	DeploymentInfo = rbac.DeploymentInfo
)

// Actions subject to a permission.
const (
	DeploymentRead     = rbac.DeploymentRead
	DeploymentSubmit   = rbac.DeploymentSubmit
	DeploymentDryRun   = rbac.DeploymentDryRun
	DeploymentCancel   = rbac.DeploymentCancel
	DeploymentRollback = rbac.DeploymentRollback
	DeploymentApprove  = rbac.DeploymentApprove
	FreezeOverride     = rbac.FreezeOverride
	ScheduleRead       = rbac.ScheduleRead
	ScheduleWrite      = rbac.ScheduleWrite
	PlatformRead       = rbac.PlatformRead
	PolicyRead         = rbac.PolicyRead
	PolicyWrite        = rbac.PolicyWrite
	PolicyApprove      = rbac.PolicyApprove
	FreezeWrite        = rbac.FreezeWrite
	All                = rbac.All
)

// Engines of the permissions.
const (
	EngineBuiltin = rbac.EngineBuiltin
	EngineOPA     = rbac.EngineOPA
)

// Variables and functions of the permission model.
var (
	Actions       = rbac.Actions
	BuiltinRoles  = rbac.BuiltinRoles
	StaticActions = rbac.StaticActions
	WriteActions  = rbac.WriteActions
	NewPolicy     = rbac.NewPolicy
	IsStatic      = rbac.IsStatic
	IsWrite       = rbac.IsWrite
	CheckGrant    = rbac.CheckGrant
	WithoutStatic = rbac.WithoutStatic
	GrantsAllow   = rbac.GrantsAllow
	GrantFor      = rbac.GrantFor
)
