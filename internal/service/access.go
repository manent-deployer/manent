package service

import (
	"context"
	"time"
)

// AccessService describes the caller's identity and what it can do: interfaces (UI,
// "manent whoami") use it to show only the allowed actions. The real check stays in the
// service, which rejects a forbidden action anyway.
type AccessService interface {
	// Me returns the identity of the request and the permissions concerning it.
	Me(ctx context.Context) (*Identity, error)
}

// Identity is the identity of a request with the permissions that apply to it.
type Identity struct {
	// Subject is the "<method>:<name>" identifier used by RBAC and audit (e.g. "oidc:alice").
	Subject string   `json:"subject"`
	Name    string   `json:"name"`
	Method  string   `json:"method"`
	Groups  []string `json:"groups,omitempty"`
	// AuthEnabled is false with auth.mode none: everything is allowed and the identity is anonymous.
	AuthEnabled bool `json:"auth_enabled"`
	// Grants are the roles assigned to the identity, with the applications and environments they
	// apply to ("*" patterns).
	Grants []Grant `json:"grants"`
	// AuthzEngine and AuthzRevision are the permission engine (opa) and the policy revision in
	// force; empty with the builtin engine.
	AuthzEngine   string `json:"authz_engine,omitempty"`
	AuthzRevision string `json:"authz_revision,omitempty"`
}

// Grant is a role assigned on a set of applications and environments.
type Grant struct {
	// Name labels the grant (a binding's position, or the name given by the policy).
	Name         string   `json:"name,omitempty"`
	Role         string   `json:"role,omitempty"`
	Actions      []string `json:"actions"`
	Applications []string `json:"applications"`
	Environments []string `json:"environments"`
	// Tenants is the selector of the tenants the grant is limited to ("" for all).
	Tenants string `json:"tenants,omitempty"`
	// Artifacts are the artifact patterns the grant is limited to (none for all).
	Artifacts []string `json:"artifacts,omitempty"`
	// Selector is the application selector the grant is limited to ("" for all).
	Selector string `json:"selector,omitempty"`
	// Until, if set, is when the grant stops applying.
	Until *time.Time `json:"until,omitempty"`
}
