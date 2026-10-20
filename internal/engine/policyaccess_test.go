package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/joeg-ita/manent/internal/auth"
	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/rbac"
)

// accessAdmin is a PolicyAdmin for the simulation: revision 2 adds the on-call grant and
// denies production to contractors.
type accessAdmin struct {
	PolicyAdmin
	deny []rbac.DenyRequest
}

func (a *accessAdmin) AccessRevision(_ context.Context, revision int64, p *auth.Principal) (string, []rbac.Grant, []string, error) {
	gs := []rbac.Grant{{Name: "team", Actions: rbac.BuiltinRoles["deployer"], Applications: []string{"shop"}, Environments: []string{"*"}}}
	switch revision {
	case 0, 1:
		return "1:abc", gs, nil, nil
	case 2:
		return "2:def", append(gs, rbac.Grant{Name: "oncall", Actions: []rbac.Action{rbac.DeploymentCancel}, Applications: []string{"*"}, Environments: []string{"prod"}}), []string{"grant x: unknown action"}, nil
	}
	return "", nil, nil, ErrNotFound
}

func (a *accessAdmin) DenyRevision(_ context.Context, _ int64, r rbac.DenyRequest) ([]string, error) {
	a.deny = append(a.deny, r)
	if r.Principal.Claims["employee_type"] == "contractor" && r.Environment == "prod" {
		return []string{"contractors cannot change production"}, nil
	}
	return nil, nil
}

func TestSimulazioneDegliAccessi(t *testing.T) {
	ctx := context.Background()
	pa := &accessAdmin{}
	e := &Engine{policies: pa, now: func() time.Time { return time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC) }}

	acc, err := e.PolicyAccess(ctx, 2, PolicyAccessRequest{Subject: "alice", Groups: []string{"team-shop"}, Compare: true})
	if err != nil || acc.Subject != "oidc:alice" || acc.Revision != "2:def" || len(acc.Grants) != 2 || len(acc.Warnings) != 1 {
		t.Fatalf("simulazione: %+v %v", acc, err)
	}
	if acc.Compared != "1:abc" || len(acc.Gained) != 1 || acc.Gained[0].Name != "oncall" || len(acc.Lost) != 0 {
		t.Errorf("confronto con la revisione attiva: %+v", acc)
	}

	// A change reaches the denials of the revision, with the artifact of the descriptor.
	d := &descriptor.Descriptor{Application: "shop", Environment: "prod", Artifact: descriptor.Artifact{Name: "shop", Type: "jar", Version: "1.0"}}
	acc, err = e.PolicyAccess(ctx, 2, PolicyAccessRequest{Subject: "oidc:c", Claims: map[string]string{"employee_type": "contractor"},
		Check: &AccessCheck{Action: "deployment.submit", Descriptor: d}})
	if err != nil || acc.Check == nil || acc.Check.Allowed || acc.Check.GrantedBy != "team" || len(acc.Check.Denied) != 1 {
		t.Fatalf("divieto simulato: %+v %v", acc.Check, err)
	}
	if r := pa.deny[len(pa.deny)-1]; r.Deployment == nil || r.Deployment.Type != "jar" || r.Application != "shop" {
		t.Errorf("richiesta ai divieti: %+v", r)
	}
	// Reads do not evaluate the denials; no grant is a refusal with a reason.
	n := len(pa.deny)
	if acc, _ := e.PolicyAccess(ctx, 2, PolicyAccessRequest{Subject: "c", Check: &AccessCheck{Action: "deployment.read", Application: "shop", Environment: "prod"}}); !acc.Check.Allowed || len(pa.deny) != n {
		t.Errorf("lettura: %+v", acc.Check)
	}
	if acc, _ := e.PolicyAccess(ctx, 1, PolicyAccessRequest{Subject: "c", Check: &AccessCheck{Action: "deployment.read", Application: "billing", Environment: "prod"}}); acc.Check.Allowed || acc.Check.Reason == "" {
		t.Errorf("senza grant: %+v", acc.Check)
	}
	// policy.* is static, whatever the revision says.
	if acc, _ := e.PolicyAccess(ctx, 2, PolicyAccessRequest{Subject: "c", Check: &AccessCheck{Action: "policy.write"}}); acc.Check.Allowed {
		t.Errorf("azione statica: %+v", acc.Check)
	}

	for _, r := range []PolicyAccessRequest{{}, {Subject: "oidc:"}, {Subject: "c", Check: &AccessCheck{Action: "deployment.nuke"}}, {Subject: "c", Check: &AccessCheck{Action: "deployment.read"}}} {
		if _, err := e.PolicyAccess(ctx, 1, r); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("richiesta non valida %+v: %v", r, err)
		}
	}
	if _, err := e.PolicyAccess(ctx, 9, PolicyAccessRequest{Subject: "c"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("revisione inesistente: %v", err)
	}
}
