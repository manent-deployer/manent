package engine

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/joeg-ita/manent/internal/auth"
	"github.com/joeg-ita/manent/internal/rbac"
)

// ServiceGrant converts a grant of the permission model to the service contract.
func ServiceGrant(gr rbac.Grant) Grant {
	acts := make([]string, len(gr.Actions))
	for i, a := range gr.Actions {
		acts[i] = string(a)
	}
	return Grant{Name: gr.Name, Role: gr.Role, Actions: acts, Applications: gr.Applications, Environments: gr.Environments, Tenants: gr.Tenants, Artifacts: gr.Artifacts, Selector: gr.Selector, Until: gr.Until}
}

// PolicyAccess implements PolicyService: the grants of the identity given by hand with the
// revision, the difference from the active one (Compare) and the outcome of an action (Check).
// Nothing about the identity is read from the identity provider or stored.
func (e *Engine) PolicyAccess(ctx context.Context, revision int64, r PolicyAccessRequest) (*PolicyAccess, error) {
	pa, err := e.policyAdmin()
	if err != nil {
		return nil, err
	}
	p, err := simulatedPrincipal(r)
	if err != nil {
		return nil, err
	}
	label, grants, warnings, err := pa.AccessRevision(ctx, revision, p)
	if err != nil {
		return nil, accessError(err)
	}
	out := &PolicyAccess{Revision: label, Subject: p.String(), Grants: serviceGrants(grants), Warnings: warnings}
	if r.Compare {
		active, before, _, err := pa.AccessRevision(ctx, 0, p)
		if err != nil {
			return nil, accessError(err)
		}
		out.Compared = active
		out.Gained, out.Lost = grantsDiff(serviceGrants(before), out.Grants)
	}
	if r.Check != nil {
		out.Check, err = e.accessCheck(ctx, pa, revision, p, grants, *r.Check)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// accessError keeps a missing revision (404) and an invalid request (422) as they are.
func accessError(err error) error {
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalidRequest) {
		return err
	}
	return policyError(err)
}

// simulatedPrincipal builds the identity of the simulation: "<method>:<name>", oidc by default.
func simulatedPrincipal(r PolicyAccessRequest) (*auth.Principal, error) {
	subject := strings.TrimSpace(r.Subject)
	if subject == "" {
		return nil, fmt.Errorf("%w: subject is required", ErrInvalidRequest)
	}
	method, name, ok := strings.Cut(subject, ":")
	if !ok {
		method, name = auth.MethodOIDC, subject
	}
	if method == "" || name == "" {
		return nil, fmt.Errorf("%w: invalid subject %q (expected <method>:<name>)", ErrInvalidRequest, subject)
	}
	return &auth.Principal{Subject: name, Method: method, Groups: r.Groups, Claims: r.Claims}, nil
}

// accessCheck simulates an action: static actions are decided only by rbac.bindings, the
// others need a grant and, for the changes, no denial of the revision.
func (e *Engine) accessCheck(ctx context.Context, pa PolicyAdmin, revision int64, p *auth.Principal, grants []rbac.Grant, c AccessCheck) (*AccessOutcome, error) {
	a := rbac.Action(c.Action)
	if !slices.Contains(rbac.Actions, a) {
		return nil, fmt.Errorf("%w: unknown action %q", ErrInvalidRequest, c.Action)
	}
	if rbac.IsStatic(a) {
		return &AccessOutcome{Reason: "policy.* actions are decided only by rbac.bindings in the configuration, not by the policy"}, nil
	}
	if d := c.Descriptor; d != nil {
		c.Application, c.Environment = cmp.Or(c.Application, d.Application), cmp.Or(c.Environment, d.Environment)
		c.Artifact, c.Tenant = cmp.Or(c.Artifact, d.Artifact.Name), cmp.Or(c.Tenant, d.Tenant)
	}
	if c.Application == "" || c.Environment == "" {
		return nil, fmt.Errorf("%w: application and environment are required for the check", ErrInvalidRequest)
	}
	ref := e.ownerRef(ctx, c.Application, c.Tenant)
	gr, ok := rbac.GrantFor(grants, a, c.Application, c.Artifact, c.Environment, ref, e.now())
	if !ok {
		return &AccessOutcome{Reason: "no grant of the revision allows the action"}, nil
	}
	out := &AccessOutcome{Allowed: true, GrantedBy: gr.Name}
	if !rbac.IsWrite(a) {
		return out, nil
	}
	req := rbac.DenyRequest{Principal: p, Action: a, Application: c.Application, Environment: c.Environment, Tenant: ref}
	if d := c.Descriptor; d != nil {
		req.Deployment = &rbac.DeploymentInfo{
			ID: d.ID, Kind: KindDeploy, DryRun: d.DryRun || a == rbac.DeploymentDryRun, Artifact: d.Artifact.Name,
			Type: d.Artifact.Type, Version: d.Artifact.Version, RequestedBy: p.String(),
		}
	}
	reasons, err := pa.DenyRevision(ctx, revision, req)
	if err != nil {
		return nil, accessError(err)
	}
	if len(reasons) > 0 {
		out.Allowed, out.Denied = false, reasons
	}
	return out, nil
}

func serviceGrants(gs []rbac.Grant) []Grant {
	out := make([]Grant, len(gs))
	for i, g := range gs {
		out[i] = ServiceGrant(g)
	}
	return out
}

// grantsDiff returns the grants only in after (gained) and only in before (lost), compared by
// content.
func grantsDiff(before, after []Grant) (gained, lost []Grant) {
	key := func(g Grant) string {
		b, _ := json.Marshal(g)
		return string(b)
	}
	in := func(gs []Grant, g Grant) bool {
		k := key(g)
		return slices.ContainsFunc(gs, func(x Grant) bool { return key(x) == k })
	}
	for _, g := range after {
		if !in(before, g) {
			gained = append(gained, g)
		}
	}
	for _, g := range before {
		if !in(after, g) {
			lost = append(lost, g)
		}
	}
	return gained, lost
}
