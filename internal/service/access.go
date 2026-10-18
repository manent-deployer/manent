package service

import "context"

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
}

// Grant is a role assigned on a set of applications and environments.
type Grant struct {
	Role         string   `json:"role"`
	Actions      []string `json:"actions"`
	Applications []string `json:"applications"`
	Environments []string `json:"environments"`
	// Tenants is the selector of the tenants the grant is limited to ("" for all).
	Tenants string `json:"tenants,omitempty"`
}
