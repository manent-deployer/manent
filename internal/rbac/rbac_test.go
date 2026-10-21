package rbac

import (
	"testing"

	"github.com/joeg-ita/manent/internal/auth"
)

// A binding with artifacts applies only to the deploys of those artifacts, never to the
// actions on the whole environment.
func TestBindingPerArtefatto(t *testing.T) {
	pol, err := NewPolicy(nil, []Binding{{Groups: []string{"web"}, Role: "deployer", Applications: []string{"shop"}, Environments: []string{"*"}, Artifacts: []string{"shop-web"}}})
	if err != nil {
		t.Fatal(err)
	}
	p := &auth.Principal{Subject: "dan", Method: auth.MethodOIDC, Groups: []string{"web"}}
	if !pol.AllowedFor(p, DeploymentSubmit, "shop", "shop-web", "prod", nil) {
		t.Error("il binding vale per il suo artefatto")
	}
	if pol.AllowedFor(p, DeploymentSubmit, "shop", "shop-api", "prod", nil) {
		t.Error("il binding non vale per un altro artefatto")
	}
	if pol.Allowed(p, DeploymentRead, "shop", "prod") {
		t.Error("un binding per artefatto non vale per le azioni sull'intero ambiente")
	}
}
