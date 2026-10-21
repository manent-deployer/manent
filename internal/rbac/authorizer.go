package rbac

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/joeg-ita/manent/internal/auth"
	"github.com/joeg-ita/manent/internal/glob"
	"github.com/joeg-ita/manent/internal/tenant"
)

// Engines of the permissions.
const (
	EngineBuiltin = "builtin"
	EngineOPA     = "opa"
)

// Authorizer decides the permissions in two parts: the grants of an identity, evaluated once
// per identity and enough for every read, and the denials of a single change, evaluated only
// on the actions that modify (WriteActions).
type Authorizer interface {
	// Grants returns the permissions of the identity. An error means no permission (fail-closed).
	Grants(ctx context.Context, p *auth.Principal) ([]Grant, error)
	// Deny returns the reasons the change is refused; none means allowed. An error refuses it.
	Deny(ctx context.Context, r DenyRequest) ([]string, error)
	// Info returns the engine and the revision in force ("" for the builtin engine).
	Info() (engine, revision string)
}

// Grant is a permission of an identity: actions on applications, environments and, with
// Tenants, the deploys of the matching tenants.
type Grant struct {
	// Name labels the grant in Me and in the audit (for a binding, its position).
	Name string
	// Role is the role the actions come from, if the grant names one.
	Role         string
	Actions      []Action
	Applications []string
	Environments []string
	// Tenants is a selector on the tenant (tags and "name"); "" applies to every deploy.
	Tenants string
	// Artifacts are patterns on artifact.name; empty applies to every artifact.
	Artifacts []string
	// Selector is an application selector; "" applies to every application.
	Selector string
	// Until, if set, is the instant the grant stops applying.
	Until *time.Time
}

// grants reports whether the grant allows a on application/artifact/environment/tenant at now.
func (g Grant) grants(a Action, application, artifact, environment string, t *tenant.Ref, now time.Time) bool {
	if g.Until != nil && !now.Before(*g.Until) {
		return false
	}
	if !slices.Contains(g.Actions, All) && !slices.Contains(g.Actions, a) {
		return false
	}
	return glob.Any(g.Applications, application) && glob.Any(g.Environments, environment) && tenant.Matches(g.Tenants, t) &&
		ArtifactMatches(g.Artifacts, artifact) && tenant.MatchesApp(g.Selector, t)
}

// Anywhere reports whether the grant allows a on some application, at now.
func (g Grant) Anywhere(a Action, now time.Time) bool {
	if g.Until != nil && !now.Before(*g.Until) {
		return false
	}
	return slices.Contains(g.Actions, All) || slices.Contains(g.Actions, a)
}

// StaticActions are decided only by the static configuration (rbac.bindings), whatever the
// engine: a policy revision cannot grant itself the power to change the policy.
var StaticActions = []Action{PolicyRead, PolicyWrite, PolicyApprove}

// IsStatic reports whether a is decided only by the static configuration.
func IsStatic(a Action) bool { return slices.Contains(StaticActions, a) }

// WriteActions are the actions whose requests are also checked by Deny.
var WriteActions = []Action{
	DeploymentSubmit, DeploymentDryRun, DeploymentCancel, DeploymentRollback, DeploymentApprove,
	FreezeOverride, ScheduleWrite, FreezeWrite,
}

// IsWrite reports whether a is a change, subject to Deny.
func IsWrite(a Action) bool { return slices.Contains(WriteActions, a) }

// DenyRequest is a change checked by Deny.
type DenyRequest struct {
	Principal   *auth.Principal
	Action      Action
	Application string
	Environment string
	// Tenant is the tenant of the deploy, with its tags (nil without tenant).
	Tenant *tenant.Ref
	// Deployment is the deploy the action concerns: the request for a submit, the existing
	// deploy for cancel, rollback and approval. Nil for actions without a deploy.
	Deployment *DeploymentInfo
}

// DeploymentInfo describes the deploy of a change, as the Deny input sees it.
type DeploymentInfo struct {
	ID          string `json:"id,omitempty"`
	Kind        string `json:"kind,omitempty"`
	DryRun      bool   `json:"dry_run"`
	Artifact    string `json:"artifact_name,omitempty"`
	Type        string `json:"artifact_type,omitempty"`
	Version     string `json:"version,omitempty"`
	RequestedBy string `json:"requested_by,omitempty"`
}

// Grants implements Authorizer: the bindings that concern the identity, with the actions of
// their role.
func (pol Policy) Grants(_ context.Context, p *auth.Principal) ([]Grant, error) {
	if p == nil {
		return nil, nil
	}
	var out []Grant
	for i, b := range pol.Bindings {
		if !b.matches(p) {
			continue
		}
		out = append(out, Grant{
			Name: fmt.Sprintf("binding %d", i), Role: b.Role, Actions: slices.Clone(pol.Roles[b.Role]),
			Applications: b.Applications, Environments: b.Environments, Tenants: b.Tenants, Artifacts: b.Artifacts, Selector: b.Selector,
		})
	}
	return out, nil
}

// Deny implements Authorizer: the bindings have no denials.
func (Policy) Deny(context.Context, DenyRequest) ([]string, error) { return nil, nil }

// Info implements Authorizer.
func (Policy) Info() (string, string) { return EngineBuiltin, "" }

// GrantsAllow reports whether any grant allows the action on a deploy of the artifact (""
// for the actions on the whole environment).
func GrantsAllow(gs []Grant, a Action, application, artifact, environment string, t *tenant.Ref, now time.Time) bool {
	_, ok := GrantFor(gs, a, application, artifact, environment, t, now)
	return ok
}

// GrantFor returns the first grant that allows the action.
func GrantFor(gs []Grant, a Action, application, artifact, environment string, t *tenant.Ref, now time.Time) (Grant, bool) {
	for _, g := range gs {
		if g.grants(a, application, artifact, environment, t, now) {
			return g, true
		}
	}
	return Grant{}, false
}

// CheckGrant validates a grant produced by a policy: known actions (or a known role), patterns
// present, valid selector. Static actions are not an error: Filter drops them.
func CheckGrant(g Grant, roles map[string][]Action) error {
	if g.Role != "" {
		if _, ok := roles[g.Role]; !ok {
			return fmt.Errorf("unknown role %q", g.Role)
		}
	}
	if len(g.Actions) == 0 {
		return fmt.Errorf("grant %q: role or actions are required", g.Name)
	}
	for _, a := range g.Actions {
		if !slices.Contains(Actions, a) {
			return fmt.Errorf("grant %q: unknown action %q", g.Name, a)
		}
	}
	if len(g.Applications) == 0 || len(g.Environments) == 0 {
		return fmt.Errorf("grant %q: applications and environments are required (\"*\" for all)", g.Name)
	}
	if err := tenant.CheckSelector(g.Tenants); err != nil {
		return fmt.Errorf("grant %q: tenants: %w", g.Name, err)
	}
	if err := tenant.CheckSelector(g.Selector); err != nil {
		return fmt.Errorf("grant %q: selector: %w", g.Name, err)
	}
	return nil
}

// WithoutStatic removes the static actions from the grant ("*" stays, IsStatic actions are
// checked before the grants anyway); it reports whether some were removed.
func WithoutStatic(g Grant) (Grant, bool) {
	kept := g.Actions[:0:0]
	removed := false
	for _, a := range g.Actions {
		if IsStatic(a) {
			removed = true
			continue
		}
		kept = append(kept, a)
	}
	g.Actions = kept
	return g, removed
}
