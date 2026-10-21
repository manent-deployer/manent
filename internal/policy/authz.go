package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"time"

	"github.com/open-policy-agent/opa/v1/ast"
	"github.com/open-policy-agent/opa/v1/rego"

	"github.com/joeg-ita/manent/internal/auth"
	"github.com/joeg-ita/manent/internal/rbac"
	"github.com/joeg-ita/manent/internal/tenant"
)

// Queries of the permission policy.
const (
	GrantsQuery = "data.manent.authz.grants"
	DenyQuery   = "data.manent.authz.deny"
)

// AuthzInputSchema is the version of the input of the permission policy.
const AuthzInputSchema = 1

// Library modules and default policy of the permissions.
const (
	authzLibModule     = "rego/authz.rego"
	authzDefaultModule = "rego/authz_default.rego"
)

// AuthzOptions enables the permission policy (rbac.engine: opa) in the compiled revisions.
type AuthzOptions struct {
	// Roles are rbac.roles of manent.yml; Bindings rbac.bindings. They become data.manent.rbac
	// when the revision does not define it.
	Roles    map[string][]rbac.Action
	Bindings []rbac.Binding
}

// RBACData converts the RBAC configuration into data.manent.rbac.
func RBACData(roles map[string][]rbac.Action, bindings []rbac.Binding) map[string]any {
	rs := map[string]any{}
	for name, acts := range roles {
		rs[name] = actionsAny(acts)
	}
	bs := make([]any, 0, len(bindings))
	for _, b := range bindings {
		m := map[string]any{"role": b.Role, "subjects": anySlice(b.Subjects), "groups": anySlice(b.Groups),
			"applications": anySlice(b.Applications), "environments": anySlice(b.Environments)}
		if b.Tenants != "" {
			m["tenants"] = b.Tenants
		}
		if len(b.Artifacts) > 0 {
			m["artifacts"] = anySlice(b.Artifacts)
		}
		if b.Selector != "" {
			m["selector"] = b.Selector
		}
		bs = append(bs, m)
	}
	return map[string]any{"roles": rs, "bindings": bs}
}

func actionsAny(acts []rbac.Action) []any {
	out := make([]any, len(acts))
	for i, a := range acts {
		out[i] = string(a)
	}
	return out
}

// revisionRoles returns the roles a grant may name: the predefined ones plus those of
// data.manent.rbac.roles.
func revisionRoles(data map[string]any) (map[string][]rbac.Action, error) {
	roles := maps.Clone(rbac.BuiltinRoles)
	v, ok := lookup(data, "manent", "rbac", "roles")
	if !ok {
		return roles, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var custom map[string][]rbac.Action
	if err := json.Unmarshal(b, &custom); err != nil {
		return nil, fmt.Errorf("data.manent.rbac.roles: %w", err)
	}
	for name, acts := range custom {
		if _, builtin := rbac.BuiltinRoles[name]; builtin {
			continue
		}
		for _, a := range acts {
			if !slices.Contains(rbac.Actions, a) {
				return nil, fmt.Errorf("data.manent.rbac.roles.%s: unknown action %q", name, a)
			}
		}
		roles[name] = acts
	}
	return roles, nil
}

// authzQueries are the prepared queries of the permissions of a revision.
type authzQueries struct {
	grants rego.PreparedEvalQuery
	deny   rego.PreparedEvalQuery
	roles  map[string][]rbac.Action
}

// prepareAuthz prepares the grants and deny queries on the modules and data of the revision.
func prepareAuthz(ctx context.Context, base []func(*rego.Rego), roles map[string][]rbac.Action) (*authzQueries, error) {
	g, err := rego.New(append(slices.Clone(base), rego.Query(GrantsQuery))...).PrepareForEval(ctx)
	if err != nil {
		return nil, err
	}
	d, err := rego.New(append(slices.Clone(base), rego.Query(DenyQuery))...).PrepareForEval(ctx)
	if err != nil {
		return nil, err
	}
	return &authzQueries{grants: g, deny: d, roles: roles}, nil
}

// AuthzIdentity is the identity in the input of the permission policy.
type AuthzIdentity struct {
	Subject string            `json:"subject"`
	Method  string            `json:"method"`
	Name    string            `json:"name"`
	Groups  []string          `json:"groups"`
	Claims  map[string]string `json:"claims"`
}

// IdentityOf builds the input identity of a principal: "<method>:<name>" as subject, the claims
// already limited to auth.oidc.policy_claims.
func IdentityOf(p *auth.Principal) AuthzIdentity {
	id := AuthzIdentity{Subject: p.String(), Method: p.Method, Name: p.Subject, Groups: slices.Clone(p.Groups), Claims: maps.Clone(p.Claims)}
	if id.Groups == nil {
		id.Groups = []string{}
	}
	if id.Claims == nil {
		id.Claims = map[string]string{}
	}
	return id
}

// GrantsInput is the input of data.manent.authz.grants.
type GrantsInput struct {
	Schema   int           `json:"schema"`
	Now      time.Time     `json:"now"`
	Identity AuthzIdentity `json:"identity"`
}

// DenyInput is the input of data.manent.authz.deny.
type DenyInput struct {
	Schema   int           `json:"schema"`
	Now      time.Time     `json:"now"`
	Identity AuthzIdentity `json:"identity"`
	Action   string        `json:"action"`
	Resource DenyResource  `json:"resource"`
}

// DenyResource is the object of a change.
type DenyResource struct {
	Application string          `json:"application"`
	Environment string          `json:"environment"`
	Tenant      *tenant.Ref     `json:"tenant,omitempty"`
	Deployment  *DenyDeployment `json:"deployment,omitempty"`
}

// DenyDeployment is the deploy of a change.
type DenyDeployment struct {
	ID          string       `json:"id,omitempty"`
	Kind        string       `json:"kind,omitempty"`
	DryRun      bool         `json:"dry_run"`
	Artifact    DenyArtifact `json:"artifact"`
	RequestedBy string       `json:"requested_by,omitempty"`
}

// DenyArtifact is the artifact of the deploy of a change.
type DenyArtifact struct {
	Type    string `json:"type,omitempty"`
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
}

// DenyInputOf builds the input of Deny from a request of the Guard.
func DenyInputOf(r rbac.DenyRequest, now time.Time) DenyInput {
	in := DenyInput{Schema: AuthzInputSchema, Now: now.UTC(), Identity: IdentityOf(r.Principal), Action: string(r.Action),
		Resource: DenyResource{Application: r.Application, Environment: r.Environment, Tenant: r.Tenant}}
	if d := r.Deployment; d != nil {
		in.Resource.Deployment = &DenyDeployment{ID: d.ID, Kind: d.Kind, DryRun: d.DryRun, RequestedBy: d.RequestedBy,
			Artifact: DenyArtifact{Type: d.Type, Name: d.Artifact, Version: d.Version}}
	}
	return in
}

// rawGrant is a grant as the policy writes it.
type rawGrant struct {
	Name         string   `json:"name"`
	Role         string   `json:"role"`
	Actions      []string `json:"actions"`
	Applications []string `json:"applications"`
	Environments []string `json:"environments"`
	Tenants      string   `json:"tenants"`
	Artifacts    []string `json:"artifacts"`
	Selector     string   `json:"selector"`
	Until        string   `json:"until"`
}

// GrantsResult is the outcome of the grants of an identity: the valid grants, and the problems
// of the ones dropped (invalid, or with static actions removed).
type GrantsResult struct {
	Grants   []rbac.Grant
	Warnings []string
}

// Grants evaluates the grants of an identity. An undefined result, a wrong shape or an
// evaluation error is an error; single invalid grants are dropped with a warning.
func (c *Compiled) Grants(ctx context.Context, in GrantsInput) (GrantsResult, error) {
	if c.authz == nil {
		return GrantsResult{}, errors.New("the revision was compiled without the permission policy (rbac.engine: opa)")
	}
	v, err := c.evalAuthz(ctx, c.authz.grants, in)
	if err != nil {
		return GrantsResult{}, err
	}
	if v == nil {
		return GrantsResult{}, errors.New("the permission grants are undefined: manent.authz must define grants (add a default)")
	}
	return decodeGrants(v, c.authz.roles)
}

// Deny evaluates the denials of a change: an undefined deny means none.
func (c *Compiled) Deny(ctx context.Context, in DenyInput) ([]string, error) {
	if c.authz == nil {
		return nil, errors.New("the revision was compiled without the permission policy (rbac.engine: opa)")
	}
	v, err := c.evalAuthz(ctx, c.authz.deny, in)
	if err != nil || v == nil {
		return nil, err
	}
	return decodeReasons(v)
}

// evalAuthz evaluates a prepared query; nil without a result.
func (c *Compiled) evalAuthz(ctx context.Context, q rego.PreparedEvalQuery, in any) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	b, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var generic any
	if err := json.Unmarshal(b, &generic); err != nil {
		return nil, err
	}
	val, err := ast.InterfaceToValue(generic)
	if err != nil {
		return nil, err
	}
	rs, err := q.Eval(ctx, rego.EvalParsedInput(val))
	if err != nil {
		return nil, err
	}
	if len(rs) == 0 || len(rs[0].Expressions) == 0 {
		return nil, nil
	}
	return rs[0].Expressions[0].Value, nil
}

// decodeGrants checks the shape of the grants: an array (or set) of objects without unknown keys.
func decodeGrants(v any, roles map[string][]rbac.Action) (GrantsResult, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return GrantsResult{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var raws []rawGrant
	if err := dec.Decode(&raws); err != nil {
		return GrantsResult{}, fmt.Errorf("grants: %w", err)
	}
	var res GrantsResult
	for i, r := range raws {
		g := rbac.Grant{Name: r.Name, Role: r.Role, Applications: r.Applications, Environments: r.Environments, Tenants: r.Tenants, Artifacts: r.Artifacts, Selector: r.Selector}
		if g.Name == "" {
			g.Name = fmt.Sprintf("grant %d", i)
		}
		for _, a := range r.Actions {
			g.Actions = append(g.Actions, rbac.Action(a))
		}
		if r.Role != "" && len(r.Actions) == 0 {
			g.Actions = slices.Clone(roles[r.Role])
		}
		if r.Until != "" {
			t, err := time.Parse(time.RFC3339, r.Until)
			if err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("grant %q: until is not an RFC 3339 instant", g.Name))
				continue
			}
			g.Until = &t
		}
		if err := rbac.CheckGrant(g, roles); err != nil {
			res.Warnings = append(res.Warnings, err.Error())
			continue
		}
		if kept, removed := rbac.WithoutStatic(g); removed {
			res.Warnings = append(res.Warnings, fmt.Sprintf("grant %q: the policy.* actions are decided only by rbac.bindings and were ignored", g.Name))
			if len(kept.Actions) == 0 {
				continue
			}
			g = kept
		}
		res.Grants = append(res.Grants, g)
	}
	sort.SliceStable(res.Grants, func(i, j int) bool { return res.Grants[i].Name < res.Grants[j].Name })
	return res, nil
}

// decodeReasons reads the denials: strings or objects with message (and code).
func decodeReasons(v any) ([]string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(b, &raws); err != nil {
		return nil, fmt.Errorf("deny must be a set of reasons: %w", err)
	}
	out := make([]string, 0, len(raws))
	for _, raw := range raws {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			out = append(out, s)
			continue
		}
		var o struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&o); err != nil || o.Message == "" {
			return nil, fmt.Errorf("deny: a reason is a string or {code, message}: %s", raw)
		}
		if o.Code != "" {
			out = append(out, o.Code+": "+o.Message)
		} else {
			out = append(out, o.Message)
		}
	}
	slices.Sort(out)
	return out, nil
}

// checkAuthzSamples evaluates the permission policy on sample identities and changes: grants
// and denials must be defined and well formed, and every grant valid.
func (c *Compiled) checkAuthzSamples(ctx context.Context) error {
	now := time.Date(2026, 1, 5, 10, 0, 0, 0, time.UTC)
	ids := []*auth.Principal{
		{Subject: "sample", Method: auth.MethodOIDC},
		{Subject: "sample", Method: auth.MethodOIDC, Groups: []string{"sample-group"}, Claims: map[string]string{"email": "sample@example.com", "name": "Sample"}},
		{Subject: "ci", Method: auth.MethodToken, Groups: []string{"ci"}},
	}
	for _, p := range ids {
		res, err := c.Grants(ctx, GrantsInput{Schema: AuthzInputSchema, Now: now, Identity: IdentityOf(p)})
		if err != nil {
			return fmt.Errorf("sample grants (%s): %w", p, err)
		}
		if len(res.Warnings) > 0 {
			return fmt.Errorf("sample grants (%s): %s", p, res.Warnings[0])
		}
		for _, a := range rbac.WriteActions {
			r := rbac.DenyRequest{Principal: p, Action: a, Application: "sample", Environment: "prod",
				Tenant:     &tenant.Ref{Name: "sample", Tags: map[string]string{"tier": "gold"}},
				Deployment: &rbac.DeploymentInfo{ID: "sample-1", Kind: "deploy", Artifact: "sample", Type: "application", Version: "1.0.0", RequestedBy: p.String()}}
			if _, err := c.Deny(ctx, DenyInputOf(r, now)); err != nil {
				return fmt.Errorf("sample deny (%s, %s): %w", p, a, err)
			}
		}
	}
	return nil
}

// AuthzStarter is the starting permission module written by "export-rbac": the bindings
// exported as data, plus commented examples of the changes most often needed.
const AuthzStarter = `# Permission policy (rbac.engine: opa). The bindings of manent.yml are in
# manent/rbac/data.json (data.manent.rbac): change them there, or replace them with rules.
package manent.authz

import rego.v1

import data.manent.lib.authz

# Grants of the identity: the exported bindings, plus the rules below.
grants := authz.bindings | extra

extra contains g if {
	false # example: a team owns the tenants tagged with its name
	some team in input.identity.groups
	g := {"name": sprintf("owner %s", [team]), "role": "deployer", "applications": ["*"], "environments": ["*"], "tenants": sprintf("owner=%s", [team])}
}

# Denials of the changes, whatever the grants say.
deny contains msg if {
	false # example: contractors never change production
	input.identity.claims.employee_type == "contractor"
	input.resource.environment == "prod"
	msg := "contractors cannot change production"
}
`

// ExportRBAC returns the files of a revision with the RBAC configuration as data and the
// starting module.
func ExportRBAC(roles map[string][]rbac.Action, bindings []rbac.Binding) (Files, error) {
	data, err := json.MarshalIndent(RBACData(roles, bindings), "", "  ")
	if err != nil {
		return nil, err
	}
	return Files{"manent/rbac/data.json": string(data) + "\n", "authz.rego": AuthzStarter}, nil
}
