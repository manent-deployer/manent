// Package rbac is the model of the permissions: actions, roles, bindings, the grants of an
// identity and the Authorizer that decides them (bindings of manent.yml or a Rego policy). It
// depends on no engine code: authz.Guard applies it to the service, policy implements the
// Rego Authorizer.
package rbac

import (
	"fmt"
	"maps"
	"slices"

	"github.com/joeg-ita/manent/internal/auth"
	"github.com/joeg-ita/manent/internal/glob"
	"github.com/joeg-ita/manent/internal/tenant"
)

// Action is an operation subject to a permission.
type Action string

// Available actions.
const (
	DeploymentRead   Action = "deployment.read"
	DeploymentSubmit Action = "deployment.submit"
	DeploymentDryRun Action = "deployment.dry-run"
	DeploymentCancel Action = "deployment.cancel"
	// DeploymentRollback allows a manual rollback to a previous version.
	DeploymentRollback Action = "deployment.rollback"
	// DeploymentApprove allows approving or rejecting a deploy.
	DeploymentApprove Action = "deployment.approve"
	// FreezeOverride allows a deploy during a freeze window.
	FreezeOverride Action = "deployment.freeze-override"
	ScheduleRead   Action = "schedule.read"
	ScheduleWrite  Action = "schedule.write"
	// PlatformRead allows seeing the workers (hosts, versions, regions) and their load.
	// It does not depend on application and environment: any binding granting it is enough.
	PlatformRead Action = "platform.read"
	// PolicyRead allows reading the governance policy revisions; PolicyWrite pushing, evaluating
	// and measuring the impact of new ones; PolicyApprove activating them. Like platform.read
	// they do not depend on application and environment.
	PolicyRead    Action = "policy.read"
	PolicyWrite   Action = "policy.write"
	PolicyApprove Action = "policy.approve"
	// FreezeWrite allows creating and deleting freeze windows at runtime, on the applications
	// and environments of the window (a "*" pattern requires the permission on "*").
	FreezeWrite Action = "freeze.write"
	// All stands for every action.
	All Action = "*"
)

// Actions lists the valid actions in a role definition.
var Actions = []Action{
	DeploymentRead, DeploymentSubmit, DeploymentDryRun, DeploymentCancel, DeploymentRollback,
	DeploymentApprove, FreezeOverride, ScheduleRead, ScheduleWrite, PlatformRead,
	PolicyRead, PolicyWrite, PolicyApprove, FreezeWrite, All,
}

// BuiltinRoles are the predefined roles; the configuration can add others.
// deployer and operator do not grant approvals and freeze overrides: whoever releases must
// not be able to approve themselves with another role.
var BuiltinRoles = map[string][]Action{
	"viewer":   {DeploymentRead, ScheduleRead},
	"tester":   {DeploymentRead, ScheduleRead, DeploymentDryRun},
	"approver": {DeploymentRead, ScheduleRead, DeploymentApprove},
	"deployer": {DeploymentRead, ScheduleRead, DeploymentDryRun, DeploymentSubmit, DeploymentCancel, DeploymentRollback},
	"operator": {DeploymentRead, ScheduleRead, DeploymentDryRun, DeploymentSubmit, DeploymentCancel, DeploymentRollback, ScheduleWrite, PlatformRead},
	// release-manager opens and closes freeze windows; policy changes stay with admin.
	"release-manager": {DeploymentRead, ScheduleRead, FreezeWrite},
	"admin":           {All},
}

// Binding assigns a role to users or groups on a set of applications and environments.
// All fields accept "*" patterns.
type Binding struct {
	// Subjects are identities in the form "<method>:<name>" (e.g. "oidc:alice", "token:ci", "cli:*").
	Subjects []string `yaml:"subjects"`
	// Groups are OIDC or static token groups.
	Groups       []string `yaml:"groups"`
	Role         string   `yaml:"role"`
	Applications []string `yaml:"applications"`
	Environments []string `yaml:"environments"`
	// Tenants, if set, is a selector on the tenant (tags and "name"): the binding applies only
	// to the deploys of the matching tenants. Without it, to every deploy.
	Tenants string `yaml:"tenants"`
}

// Policy decides whether an identity can perform an action on an application/environment.
type Policy struct {
	// Disabled allows everything (authentication disabled).
	Disabled bool
	Roles    map[string][]Action
	Bindings []Binding
}

// NewPolicy merges the predefined roles with the configured ones and validates the bindings.
func NewPolicy(roles map[string][]Action, bindings []Binding) (Policy, error) {
	p := Policy{Roles: maps.Clone(BuiltinRoles), Bindings: bindings}
	for name, acts := range roles {
		if _, ok := BuiltinRoles[name]; ok {
			return p, fmt.Errorf("rbac.roles: role %q is predefined and cannot be redefined", name)
		}
		for _, a := range acts {
			if !slices.Contains(Actions, a) {
				return p, fmt.Errorf("rbac.roles.%s: unknown action %q", name, a)
			}
		}
		p.Roles[name] = acts
	}
	for i, b := range bindings {
		if _, ok := p.Roles[b.Role]; !ok {
			return p, fmt.Errorf("rbac.bindings[%d]: unknown role %q", i, b.Role)
		}
		if len(b.Subjects) == 0 && len(b.Groups) == 0 {
			return p, fmt.Errorf("rbac.bindings[%d]: provide subjects or groups", i)
		}
		if len(b.Applications) == 0 || len(b.Environments) == 0 {
			return p, fmt.Errorf("rbac.bindings[%d]: applications and environments are required (\"*\" for all)", i)
		}
		if err := tenant.CheckSelector(b.Tenants); err != nil {
			return p, fmt.Errorf("rbac.bindings[%d].tenants: %w", i, err)
		}
	}
	return p, nil
}

// Allowed reports whether p can perform a on the given application and environment.
// Without an identity the answer is always no, unless the policy is disabled.
func (pol Policy) Allowed(p *auth.Principal, a Action, application, environment string) bool {
	return pol.AllowedFor(p, a, application, environment, nil)
}

// AllowedFor is Allowed for a deploy of a tenant (nil: untenanted deploy). A binding with
// tenants applies only to the matching tenants.
func (pol Policy) AllowedFor(p *auth.Principal, a Action, application, environment string, t *tenant.Ref) bool {
	if pol.Disabled {
		return true
	}
	if p == nil {
		return false
	}
	for _, b := range pol.Bindings {
		if !pol.grants(b.Role, a) || !b.matches(p) {
			continue
		}
		if glob.Any(b.Applications, application) && glob.Any(b.Environments, environment) && tenant.Matches(b.Tenants, t) {
			return true
		}
	}
	return false
}

// AllowedAnywhere reports whether p has action a in at least one binding, whatever the
// applications and environments: it is for platform actions, which concern no application.
func (pol Policy) AllowedAnywhere(p *auth.Principal, a Action) bool {
	if pol.Disabled {
		return true
	}
	if p == nil {
		return false
	}
	for _, b := range pol.Bindings {
		if pol.grants(b.Role, a) && b.matches(p) {
			return true
		}
	}
	return false
}

func (pol Policy) grants(role string, a Action) bool {
	acts := pol.Roles[role]
	return slices.Contains(acts, All) || slices.Contains(acts, a)
}

// matches reports whether the binding applies to the identity.
func (b Binding) matches(p *auth.Principal) bool {
	if glob.Any(b.Subjects, p.String()) {
		return true
	}
	for _, g := range p.Groups {
		if glob.Any(b.Groups, g) {
			return true
		}
	}
	return false
}
