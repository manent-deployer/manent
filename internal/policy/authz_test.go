package policy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/joeg-ita/manent/internal/auth"
	"github.com/joeg-ita/manent/internal/governance"
	"github.com/joeg-ita/manent/internal/rbac"
	"github.com/joeg-ita/manent/internal/service"
	"github.com/joeg-ita/manent/internal/tenant"
)

// testRBAC is an RBAC configuration with every kind of binding: subjects, groups, patterns,
// custom roles and tenant selectors.
func testRBAC(t *testing.T) (rbac.Policy, *AuthzOptions) {
	t.Helper()
	roles := map[string][]rbac.Action{"canceller": {rbac.DeploymentRead, rbac.DeploymentCancel}}
	bindings := []rbac.Binding{
		{Groups: []string{"team-a"}, Role: "deployer", Applications: []string{"team-a-*"}, Environments: []string{"dev", "test"}},
		{Groups: []string{"team-a"}, Role: "tester", Applications: []string{"team-a-*"}, Environments: []string{"prod"}},
		{Groups: []string{"release-*"}, Role: "operator", Applications: []string{"*"}, Environments: []string{"prod"}},
		{Subjects: []string{"token:ci"}, Role: "deployer", Applications: []string{"*"}, Environments: []string{"test"}},
		{Subjects: []string{"oidc:ops"}, Role: "canceller", Applications: []string{"*"}, Environments: []string{"*"}},
		{Groups: []string{"support-acme"}, Role: "deployer", Applications: []string{"shop"}, Environments: []string{"prod"}, Tenants: "name=acme"},
		{Groups: []string{"gold"}, Role: "approver", Applications: []string{"*"}, Environments: []string{"prod"}, Tenants: "tier=gold"},
		{Groups: []string{"platform"}, Role: "admin", Applications: []string{"*"}, Environments: []string{"*"}},
	}
	pol, err := rbac.NewPolicy(roles, bindings)
	if err != nil {
		t.Fatal(err)
	}
	return pol, &AuthzOptions{Roles: roles, Bindings: bindings}
}

// The default permission policy decides exactly as the bindings of manent.yml.
func TestPermessiPredefinitiEquivalentiAiBinding(t *testing.T) {
	pol, o := testRBAC(t)
	c, _, err := Compile(context.Background(), Files{}, "", CompileOptions{Authz: o})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	principals := []*auth.Principal{
		{Subject: "alice", Method: auth.MethodOIDC, Groups: []string{"team-a"}},
		{Subject: "bob", Method: auth.MethodOIDC, Groups: []string{"release-managers"}},
		{Subject: "ci", Method: auth.MethodToken, Groups: []string{"ci"}},
		{Subject: "ops", Method: auth.MethodOIDC},
		{Subject: "carol", Method: auth.MethodOIDC, Groups: []string{"support-acme", "gold"}},
		{Subject: "root", Method: auth.MethodOIDC, Groups: []string{"platform"}},
		{Subject: "nobody", Method: auth.MethodOIDC, Groups: []string{"elsewhere"}},
	}
	tenants := []*tenant.Ref{nil, {Name: "acme", Tags: map[string]string{"tier": "gold"}}, {Name: "globex", Tags: map[string]string{"tier": "silver"}}}
	checked := 0
	for _, p := range principals {
		res, err := c.Grants(context.Background(), GrantsInput{Schema: AuthzInputSchema, Now: now, Identity: IdentityOf(p)})
		if err != nil || len(res.Warnings) > 0 {
			t.Fatalf("%s: %+v %v", p, res, err)
		}
		for _, a := range rbac.Actions {
			if rbac.IsStatic(a) || a == rbac.All {
				continue
			}
			for _, app := range []string{"team-a-api", "team-b-api", "shop"} {
				for _, env := range []string{"dev", "test", "prod"} {
					for _, tn := range tenants {
						want := pol.AllowedFor(p, a, app, env, tn)
						got := rbac.GrantsAllow(res.Grants, a, app, env, tn, now)
						if got != want {
							t.Errorf("%s %s %s/%s %v: rego %v, binding %v", p, a, app, env, tn, got, want)
						}
						checked++
					}
				}
			}
		}
	}
	if checked < 1000 {
		t.Fatalf("poche verifiche: %d", checked)
	}
}

const ownersPolicy = `package manent.authz

import rego.v1

grants contains g if {
	some group in input.identity.groups
	startswith(group, "team-")
	g := {"name": sprintf("owner %s", [group]), "role": "deployer", "applications": ["*"], "environments": ["*"], "tenants": sprintf("owner=%s", [group])}
}

grants contains {"name": "oncall", "role": "operator", "applications": ["*"], "environments": ["prod"], "until": "2026-10-20T18:00:00Z"} if {
	"oncall" in input.identity.groups
}

grants contains {"name": "sneaky", "actions": ["policy.approve", "platform.read"], "applications": ["*"], "environments": ["*"]} if {
	"sneaky" in input.identity.groups
}

deny contains "contractors cannot change production" if {
	input.identity.claims.employee_type == "contractor"
	startswith(input.resource.environment, "prod")
}

deny contains {"code": "dba", "message": "migrations in production are released by the DBA group"} if {
	input.action == "deployment.submit"
	input.resource.deployment.artifact.type == "migration"
	not "dba" in input.identity.groups
}
`

func TestPolicyDeiPermessiScritta(t *testing.T) {
	_, o := testRBAC(t)
	c, _, err := Compile(context.Background(), Files{"authz.rego": ownersPolicy}, "", CompileOptions{Authz: o})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	grants := func(p *auth.Principal) GrantsResult {
		t.Helper()
		res, err := c.Grants(context.Background(), GrantsInput{Schema: AuthzInputSchema, Now: now, Identity: IdentityOf(p)})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	acme := &tenant.Ref{Name: "acme", Tags: map[string]string{"owner": "team-shop"}}
	res := grants(&auth.Principal{Subject: "alice", Method: auth.MethodOIDC, Groups: []string{"team-shop"}})
	if !rbac.GrantsAllow(res.Grants, rbac.DeploymentSubmit, "shop", "prod", acme, now) {
		t.Errorf("il team proprietario non rilascia il proprio tenant: %+v", res.Grants)
	}
	if rbac.GrantsAllow(res.Grants, rbac.DeploymentSubmit, "shop", "prod", &tenant.Ref{Name: "globex", Tags: map[string]string{"owner": "team-other"}}, now) {
		t.Error("il team rilascia il tenant di un altro")
	}
	res = grants(&auth.Principal{Subject: "olga", Method: auth.MethodOIDC, Groups: []string{"oncall"}})
	if !rbac.GrantsAllow(res.Grants, rbac.DeploymentCancel, "x", "prod", nil, now) ||
		rbac.GrantsAllow(res.Grants, rbac.DeploymentCancel, "x", "prod", nil, now.Add(10*time.Hour)) {
		t.Error("il grant con until non scade")
	}
	// policy.* in a grant are removed with a warning; the rest of the grant stays.
	res = grants(&auth.Principal{Subject: "eve", Method: auth.MethodOIDC, Groups: []string{"sneaky"}})
	if len(res.Grants) != 1 || len(res.Grants[0].Actions) != 1 || res.Grants[0].Actions[0] != rbac.PlatformRead || len(res.Warnings) != 1 {
		t.Errorf("azioni statiche: %+v", res)
	}

	deny := func(p *auth.Principal, a rbac.Action, env, typ string) []string {
		t.Helper()
		r := rbac.DenyRequest{Principal: p, Action: a, Application: "shop", Environment: env,
			Deployment: &rbac.DeploymentInfo{Kind: "deploy", Type: typ, Version: "1.0.0"}}
		reasons, err := c.Deny(context.Background(), DenyInputOf(r, now))
		if err != nil {
			t.Fatal(err)
		}
		return reasons
	}
	contractor := &auth.Principal{Subject: "c", Method: auth.MethodOIDC, Claims: map[string]string{"employee_type": "contractor"}}
	if r := deny(contractor, rbac.DeploymentSubmit, "prod", "application"); len(r) != 1 || !strings.Contains(r[0], "contractors") {
		t.Errorf("contractor in produzione: %v", r)
	}
	if r := deny(contractor, rbac.DeploymentSubmit, "test", "application"); len(r) != 0 {
		t.Errorf("contractor in test: %v", r)
	}
	if r := deny(&auth.Principal{Subject: "d", Method: auth.MethodOIDC}, rbac.DeploymentSubmit, "test", "migration"); len(r) != 1 || r[0] != "dba: migrations in production are released by the DBA group" {
		t.Errorf("migrazione senza DBA: %v", r)
	}
}

func TestPolicyDeiPermessiNonValida(t *testing.T) {
	_, o := testRBAC(t)
	for name, src := range map[string]string{
		"senza grants":      "package manent.authz\n\nimport rego.v1\n\ndeny := set()\n",
		"ruolo sconosciuto": "package manent.authz\n\nimport rego.v1\n\ngrants := [{\"role\": \"superuser\", \"applications\": [\"*\"], \"environments\": [\"*\"]}]\n",
		"campo sconosciuto": "package manent.authz\n\nimport rego.v1\n\ngrants := [{\"role\": \"viewer\", \"applications\": [\"*\"], \"environments\": [\"*\"], \"scope\": \"x\"}]\n",
		"deny malformato":   "package manent.authz\n\nimport rego.v1\n\ngrants := []\n\ndeny contains 42 if true\n",
	} {
		if _, _, err := Compile(context.Background(), Files{"authz.rego": src}, "", CompileOptions{Authz: o}); err == nil {
			t.Errorf("%s: revisione accettata", name)
		}
	}
	// Without rbac.engine: opa the permission package is not compiled nor checked.
	if _, _, err := Compile(context.Background(), Files{"authz.rego": "package manent.authz\n\nimport rego.v1\n\ndeny := set()\n"}, "", CompileOptions{}); err != nil {
		t.Errorf("senza permessi in Rego: %v", err)
	}
}

// The Runtime is the authorizer of rbac.engine: opa: grants from the revision in force, cached
// per identity and revision, unavailable without a loaded policy.
func TestRuntimeComeAuthorizer(t *testing.T) {
	ctx := context.Background()
	_, o := testRBAC(t)
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	r, err := New(Options{Engine: EngineOPA, Store: openStore(t), Compile: CompileOptions{Authz: o}, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	alice := &auth.Principal{Subject: "alice", Method: auth.MethodOIDC, Groups: []string{"team-a", "team-shop"}}
	if _, err := r.Grants(ctx, alice); !errors.Is(err, governance.ErrUnavailable) {
		t.Fatalf("senza policy caricata: %v", err)
	}
	if err := r.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	// Default policy: the bindings of manent.yml.
	gs, err := r.Grants(ctx, alice)
	if err != nil || len(gs) != 2 || gs[0].Role != "deployer" {
		t.Fatalf("grant predefiniti: %+v %v", gs, err)
	}
	if eng, rev := r.Info(); eng != "opa" || !strings.HasPrefix(rev, "default:") {
		t.Errorf("info: %s %s", eng, rev)
	}
	// A new revision applies at once, without waiting for the cache.
	rev, err := r.Push(ctx, Files{"authz.rego": ownersPolicy}, "proprietari", "oidc:root")
	if err != nil {
		t.Fatal(err)
	}
	// The simulation evaluates a draft without activating it; 0 is the revision in force.
	label, sim, _, err := r.AccessRevision(ctx, rev.Revision, alice)
	if err != nil || !strings.HasPrefix(label, "1:") || len(sim) != 2 || sim[1].Name != "owner team-shop" {
		t.Fatalf("simulazione della bozza: %s %+v %v", label, sim, err)
	}
	if label, sim, _, err := r.AccessRevision(ctx, 0, alice); err != nil || !strings.HasPrefix(label, "default:") || sim[1].Name == "owner team-shop" {
		t.Errorf("simulazione della revisione in vigore: %s %+v %v", label, sim, err)
	}
	contractor := &auth.Principal{Subject: "c", Method: auth.MethodOIDC, Claims: map[string]string{"employee_type": "contractor"}}
	if reasons, err := r.DenyRevision(ctx, rev.Revision, rbac.DenyRequest{Principal: contractor, Action: rbac.DeploymentSubmit, Application: "shop", Environment: "prod"}); err != nil || len(reasons) != 1 {
		t.Errorf("divieti della bozza: %v %v", reasons, err)
	}
	if _, _, _, err := r.AccessRevision(ctx, 99, alice); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("revisione inesistente: %v", err)
	}
	if err := r.Activate(ctx, rev.Revision, "oidc:bob", true); err != nil {
		t.Fatal(err)
	}
	gs, err = r.Grants(ctx, alice)
	if err != nil || len(gs) != 2 || gs[1].Name != "owner team-shop" {
		t.Fatalf("grant della nuova revisione: %+v %v", gs, err)
	}
	reasons, err := r.Deny(ctx, rbac.DenyRequest{Principal: contractor,
		Action: rbac.DeploymentSubmit, Application: "shop", Environment: "prod"})
	if err != nil || len(reasons) != 1 {
		t.Errorf("divieto: %v %v", reasons, err)
	}
	// Same revision: the grants come from the cache (one entry per revision and identity).
	before := r.grants.len()
	if _, err := r.Grants(ctx, alice); err != nil || r.grants.len() != before {
		t.Errorf("seconda lettura non dalla cache: %d → %d, %v", before, r.grants.len(), err)
	}
}

// The exported revision decides as the bindings, even with a configuration without bindings.
func TestEsportazioneDeiBinding(t *testing.T) {
	ctx := context.Background()
	pol, o := testRBAC(t)
	files, err := ExportRBAC(o.Roles, o.Bindings)
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := Compile(ctx, files, "", CompileOptions{Authz: &AuthzOptions{}})
	if err != nil {
		t.Fatalf("revisione esportata: %v", err)
	}
	for _, p := range []*auth.Principal{
		{Subject: "alice", Method: auth.MethodOIDC, Groups: []string{"team-a", "gold"}},
		{Subject: "ci", Method: auth.MethodToken},
		{Subject: "ops", Method: auth.MethodOIDC},
	} {
		want, _ := pol.Grants(ctx, p)
		got, err := c.Grants(ctx, GrantsInput{Schema: AuthzInputSchema, Now: time.Now().UTC(), Identity: IdentityOf(p)})
		if err != nil || len(got.Grants) != len(want) {
			t.Fatalf("%s: %+v, attesi %+v (%v)", p, got.Grants, want, err)
		}
		for i := range want {
			if got.Grants[i].Name != want[i].Name || len(got.Grants[i].Actions) != len(want[i].Actions) || got.Grants[i].Tenants != want[i].Tenants {
				t.Errorf("%s: grant %d %+v, atteso %+v", p, i, got.Grants[i], want[i])
			}
		}
	}
	if reasons, err := c.Deny(ctx, DenyInputOf(rbac.DenyRequest{Principal: &auth.Principal{Subject: "x", Method: auth.MethodOIDC, Claims: map[string]string{"employee_type": "contractor"}},
		Action: rbac.DeploymentSubmit, Application: "shop", Environment: "prod"}, time.Now())); err != nil || len(reasons) != 0 {
		t.Errorf("gli esempi commentati non devono negare: %v %v", reasons, err)
	}
}
