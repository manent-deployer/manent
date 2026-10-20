package authz

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/joeg-ita/manent/internal/auth"
	"github.com/joeg-ita/manent/internal/engine"
	"github.com/joeg-ita/manent/internal/governance"
)

func (f *fakeService) CreateFreeze(_ context.Context, r engine.FreezeRequest) (*governance.WindowStatus, error) {
	return &governance.WindowStatus{Name: r.Name, Applications: r.Applications, Environments: r.Environments, Dynamic: true}, nil
}
func (f *fakeService) DeleteFreeze(context.Context, string) error { return nil }
func (f *fakeService) Policies(context.Context) (*engine.PolicyList, error) {
	return &engine.PolicyList{Engine: "opa"}, nil
}
func (f *fakeService) PolicyRevision(context.Context, int64) (*engine.PolicyRevision, error) {
	return &engine.PolicyRevision{Revision: 1}, nil
}
func (f *fakeService) PushPolicy(context.Context, engine.PolicyPush) (*engine.PolicyRevision, error) {
	return &engine.PolicyRevision{Revision: 2, SHA256: "abc"}, nil
}
func (f *fakeService) EvaluatePolicy(context.Context, int64, engine.PolicyEvaluationRequest) (*engine.PolicyEvaluation, error) {
	return &engine.PolicyEvaluation{}, nil
}
func (f *fakeService) PolicyImpact(context.Context, int64, time.Duration) (*engine.PolicyImpact, error) {
	return &engine.PolicyImpact{Changes: []engine.PolicyChange{
		{Deployment: "a1", Application: "team-a-api", Environment: "test"},
		{Deployment: "b1", Application: "team-b-api", Environment: "test"},
	}}, nil
}
func (f *fakeService) PolicyAccess(context.Context, int64, engine.PolicyAccessRequest) (*engine.PolicyAccess, error) {
	return &engine.PolicyAccess{}, nil
}
func (f *fakeService) ApprovePolicy(context.Context, int64) error  { return nil }
func (f *fakeService) ActivatePolicy(context.Context, int64) error { return nil }

func TestPermessiDellaPolicy(t *testing.T) {
	pol, err := NewPolicy(nil, []Binding{
		{Groups: []string{"team-a"}, Role: "deployer", Applications: []string{"team-a-*"}, Environments: []string{"*"}},
		{Groups: []string{"rm"}, Role: "release-manager", Applications: []string{"*"}, Environments: []string{"prod"}},
		{Groups: []string{"rm-a"}, Role: "release-manager", Applications: []string{"team-a-*"}, Environments: []string{"*"}},
		{Groups: []string{"platform"}, Role: "admin", Applications: []string{"*"}, Environments: []string{"*"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := &recordAudit{}
	g := &Guard{Next: &fakeService{}, Policy: pol, Audit: rec, Logger: discardLogger()}
	as := func(groups ...string) context.Context {
		return auth.WithPrincipal(context.Background(), &auth.Principal{Subject: "u", Groups: groups, Method: auth.MethodOIDC})
	}
	freeze := engine.FreezeRequest{Name: "f", Applications: []string{"*"}, Environments: []string{"prod"}}
	if _, err := g.CreateFreeze(as("team-a"), freeze); !errors.Is(err, engine.ErrForbidden) {
		t.Errorf("deployer senza freeze.write: %v", err)
	}
	if _, err := g.CreateFreeze(as("rm"), freeze); err != nil {
		t.Errorf("release-manager su */prod: %v", err)
	}
	// A "*" pattern requires the permission on "*": rm-a has it only on team-a-*.
	if _, err := g.CreateFreeze(as("rm-a"), freeze); !errors.Is(err, engine.ErrForbidden) {
		t.Errorf("freeze su tutte le applicazioni con permesso parziale: %v", err)
	}
	if _, err := g.CreateFreeze(as("rm-a"), engine.FreezeRequest{Name: "g", Applications: []string{"team-a-api"}, Environments: []string{"prod", "test"}}); err != nil {
		t.Errorf("freeze sulle proprie applicazioni: %v", err)
	}
	if _, err := g.Policies(as("rm")); !errors.Is(err, engine.ErrForbidden) {
		t.Errorf("policy.read al release-manager: %v", err)
	}
	if _, err := g.PushPolicy(as("rm"), engine.PolicyPush{}); !errors.Is(err, engine.ErrForbidden) {
		t.Errorf("policy.write al release-manager: %v", err)
	}
	if err := g.ApprovePolicy(as("team-a"), 2); !errors.Is(err, engine.ErrForbidden) {
		t.Errorf("policy.approve al deployer: %v", err)
	}
	if _, err := g.PushPolicy(as("platform"), engine.PolicyPush{Comment: "x"}); err != nil {
		t.Errorf("push da admin: %v", err)
	}
	if err := g.ApprovePolicy(as("platform"), 2); err != nil {
		t.Errorf("approvazione da admin: %v", err)
	}
	found := map[Action]bool{}
	for _, e := range rec.entries {
		found[Action(e.Action)] = true
	}
	for _, a := range []Action{ActionFreezeCreate, ActionPolicyPush, ActionPolicyActivate, FreezeWrite} {
		if !found[a] {
			t.Errorf("azione %s non registrata nell'audit: %+v", a, rec.entries)
		}
	}
	// The impact only lists the deploys the identity can read.
	pol2, _ := NewPolicy(map[string][]Action{"policy-writer": {PolicyWrite, DeploymentRead}}, []Binding{
		{Groups: []string{"pw"}, Role: "policy-writer", Applications: []string{"team-a-*"}, Environments: []string{"*"}},
	})
	g2 := &Guard{Next: &fakeService{}, Policy: pol2, Logger: discardLogger()}
	imp, err := g2.PolicyImpact(as("pw"), 1, time.Hour)
	if err != nil || len(imp.Changes) != 1 || imp.Changes[0].Deployment != "a1" {
		t.Errorf("impatto filtrato: %+v %v", imp, err)
	}
}

func TestApprovatoriDellaPolicy(t *testing.T) {
	pol, err := NewPolicy(nil, []Binding{
		{Groups: []string{"team-a", "dba"}, Role: "approver", Applications: []string{"*"}, Environments: []string{"*"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := &fakeService{statuses: map[string]engine.Status{
		"m1": {ID: "m1", Application: "db", Environment: "prod", State: engine.StateAwaitingApproval, RequestedBy: "oidc:carol",
			Approval: &engine.ApprovalState{Status: engine.ApprovalPending, Required: 1, Approvers: &governance.Approvers{Groups: []string{"dba"}}}},
	}}
	g := &Guard{Next: svc, Policy: pol, Logger: discardLogger()}
	alice := auth.WithPrincipal(context.Background(), &auth.Principal{Subject: "alice", Groups: []string{"team-a"}, Method: auth.MethodOIDC})
	dana := auth.WithPrincipal(context.Background(), &auth.Principal{Subject: "dana", Groups: []string{"dba"}, Method: auth.MethodOIDC})
	if err := g.Decide(alice, "m1", engine.DecisionRequest{Approve: true}); !errors.Is(err, engine.ErrForbidden) {
		t.Errorf("approvazione fuori dagli approvatori: %v", err)
	}
	if err := g.Decide(dana, "m1", engine.DecisionRequest{Approve: true}); err != nil {
		t.Errorf("approvazione del gruppo dba: %v", err)
	}
}
