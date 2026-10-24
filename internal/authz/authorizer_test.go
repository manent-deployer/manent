package authz

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/manent-deployer/manent/internal/auth"
	"github.com/manent-deployer/manent/internal/descriptor"
	"github.com/manent-deployer/manent/internal/engine"
	"github.com/manent-deployer/manent/internal/governance"
)

// fakeAuthorizer gives fixed grants per subject and refuses the changes of contractors.
type fakeAuthorizer struct {
	grants map[string][]Grant
	err    error
	denied []DenyRequest
}

func (f *fakeAuthorizer) Grants(_ context.Context, p *auth.Principal) ([]Grant, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.grants[p.String()], nil
}

func (f *fakeAuthorizer) Deny(_ context.Context, r DenyRequest) ([]string, error) {
	f.denied = append(f.denied, r)
	if r.Principal.Subject == "contractor" && r.Environment == "prod" {
		return []string{"contractors cannot change production"}, nil
	}
	return nil, nil
}

func (f *fakeAuthorizer) Info() (string, string) { return EngineOPA, "7:abc" }

func TestGuardConAuthorizer(t *testing.T) {
	static, err := NewPolicy(nil, []Binding{
		{Groups: []string{"platform"}, Role: "admin", Applications: []string{"*"}, Environments: []string{"*"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	until := now.Add(time.Hour)
	az := &fakeAuthorizer{grants: map[string][]Grant{
		"oidc:alice": {
			{Name: "team-a", Role: "deployer", Actions: BuiltinRoles["deployer"], Applications: []string{"team-a-*"}, Environments: []string{"*"}},
			// policy.* in a grant never applies: only the static configuration decides them.
			{Name: "sneaky", Actions: []Action{PolicyApprove, PlatformRead}, Applications: []string{"*"}, Environments: []string{"*"}},
		},
		"oidc:contractor": {{Name: "team-a", Actions: BuiltinRoles["deployer"], Applications: []string{"*"}, Environments: []string{"*"}}},
		"oidc:oncall":     {{Name: "oncall", Actions: []Action{DeploymentRead, DeploymentCancel}, Applications: []string{"*"}, Environments: []string{"prod"}, Until: &until}},
	}}
	svc := &fakeService{statuses: map[string]engine.Status{
		"a1": {ID: "a1", Application: "team-a-api", Environment: "prod", RequestedBy: "oidc:alice"},
		"b1": {ID: "b1", Application: "team-b-api", Environment: "prod"},
	}}
	rec := &recordAudit{}
	g := &Guard{Next: svc, Policy: static, Authorizer: az, Audit: rec, Logger: discardLogger(), Now: func() time.Time { return now }}
	as := func(name string, groups ...string) context.Context {
		return auth.WithPrincipal(context.Background(), &auth.Principal{Subject: name, Method: auth.MethodOIDC, Groups: groups})
	}

	// Reads use the grants of the authorizer.
	if list, err := g.List(as("alice"), engine.ListFilter{}); err != nil || len(list) != 1 || list[0].ID != "a1" {
		t.Errorf("elenco dai grant: %+v %v", list, err)
	}
	if _, err := g.Submit(as("alice"), &descriptor.Descriptor{Application: "team-a-api", Environment: "prod"}); err != nil {
		t.Errorf("submit concesso dal grant: %v", err)
	}
	// The audit names the revision and the grant that allowed the change.
	if e := rec.entries[len(rec.entries)-1]; e.Outcome != "allowed" || e.AuthzRevision != "7:abc" || e.Grant != "team-a" {
		t.Errorf("audit della modifica: %+v", e)
	}
	if _, err := g.Submit(as("alice"), &descriptor.Descriptor{Application: "team-b-api", Environment: "prod"}); !errors.Is(err, engine.ErrForbidden) {
		t.Errorf("submit fuori dai grant: %v", err)
	}
	// The change reaches Deny with the request.
	if r := az.denied[len(az.denied)-1]; r.Action != DeploymentSubmit || r.Application != "team-a-api" || r.Principal.Subject != "alice" {
		t.Errorf("richiesta a Deny: %+v", r)
	}

	// policy.* only from the static configuration.
	if _, err := g.Policies(as("alice")); !errors.Is(err, engine.ErrForbidden) {
		t.Errorf("policy.read da un grant della policy: %v", err)
	}
	if _, err := g.Policies(as("root", "platform")); err != nil {
		t.Errorf("policy.read dalla configurazione statica: %v", err)
	}
	// platform.read comes from the grants (only policy.* is static).
	if _, err := g.Workers(as("alice")); err != nil {
		t.Errorf("platform.read dal grant: %v", err)
	}

	// A denial wins over the grants, with its reason and the revision.
	_, err = g.Submit(as("contractor"), &descriptor.Descriptor{Application: "team-a-api", Environment: "prod"})
	if !errors.Is(err, engine.ErrForbidden) || !strings.Contains(err.Error(), "contractors cannot change production (policy 7:abc)") {
		t.Errorf("divieto: %v", err)
	}
	if e := rec.entries[len(rec.entries)-1]; e.Outcome != "denied" || !strings.Contains(e.Reason, "contractors") {
		t.Errorf("audit del divieto: %+v", e)
	}
	// Reads are not subject to Deny.
	if _, err := g.Status(as("contractor"), "a1"); err != nil {
		t.Errorf("lettura di un contractor: %v", err)
	}

	// A grant with until stops applying after that instant.
	if err := g.Cancel(as("oncall"), "b1"); err != nil {
		t.Errorf("cancel nel turno: %v", err)
	}
	now = until
	if err := g.Cancel(as("oncall"), "b1"); !errors.Is(err, engine.ErrForbidden) {
		t.Errorf("cancel a turno finito: %v", err)
	}

	// Me: engine, revision, the static policy.* and the grants of the policy.
	id, err := g.Me(as("alice"))
	if err != nil || id.AuthzEngine != EngineOPA || id.AuthzRevision != "7:abc" || len(id.Grants) != 2 || id.Grants[0].Name != "team-a" {
		t.Errorf("me: %+v %v", id, err)
	}
	id, _ = g.Me(as("root", "platform"))
	if len(id.Grants) != 1 || !strings.HasSuffix(id.Grants[0].Name, "(static)") || len(id.Grants[0].Actions) != len(StaticActions) {
		t.Errorf("me con policy.* statiche: %+v", id.Grants)
	}

	// An authorizer error denies everything but policy.*.
	az.err = errors.New("policy unavailable")
	if _, err := g.Status(as("alice"), "a1"); !errors.Is(err, engine.ErrForbidden) {
		t.Errorf("errore dei grant: %v", err)
	}
	if _, err := g.Policies(as("root", "platform")); err != nil {
		t.Errorf("policy.* senza policy: %v", err)
	}
	// A policy not loaded is a temporary condition (503), not a denial.
	az.err = fmt.Errorf("%w: no governance policy loaded", governance.ErrUnavailable)
	if _, err := g.Submit(as("alice"), &descriptor.Descriptor{Application: "team-a-api", Environment: "prod"}); !errors.Is(err, engine.ErrUnavailable) {
		t.Errorf("policy non caricata: %v", err)
	}
}

func TestGrantDellaPolicyStatica(t *testing.T) {
	pol := testPolicy(t)
	gs, err := pol.Grants(context.Background(), &auth.Principal{Subject: "x", Method: auth.MethodOIDC, Groups: []string{"team-a"}})
	if err != nil || len(gs) != 2 || gs[0].Role != "deployer" || gs[0].Name != "binding 0" || gs[1].Role != "tester" {
		t.Fatalf("grant dei binding: %+v %v", gs, err)
	}
	if err := CheckGrant(Grant{Name: "x", Actions: []Action{"deployment.nuke"}, Applications: []string{"*"}, Environments: []string{"*"}}, pol.Roles); err == nil {
		t.Error("azione sconosciuta accettata")
	}
	if g, removed := WithoutStatic(Grant{Actions: []Action{PolicyWrite, DeploymentRead}}); !removed || len(g.Actions) != 1 {
		t.Errorf("azioni statiche non tolte: %+v", g)
	}
}
