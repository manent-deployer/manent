package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/joeg-ita/manent/internal/engine"
	"github.com/joeg-ita/manent/internal/governance"
)

func (m *mockService) CreateFreeze(_ context.Context, r engine.FreezeRequest) (*governance.WindowStatus, error) {
	m.op = "freeze:" + r.Name
	return &governance.WindowStatus{Name: r.Name, Active: true, Dynamic: true}, nil
}
func (m *mockService) DeleteFreeze(_ context.Context, name string) error {
	if name != "f1" {
		return engine.ErrNotFound
	}
	m.op = "delete:" + name
	return nil
}
func (m *mockService) Policies(context.Context) (*engine.PolicyList, error) {
	return &engine.PolicyList{Engine: "opa", Source: "store", Current: "1:abc"}, nil
}
func (m *mockService) PolicyRevision(_ context.Context, rev int64) (*engine.PolicyRevision, error) {
	return &engine.PolicyRevision{Revision: rev, Files: map[string]string{"p.rego": "package x"}}, nil
}
func (m *mockService) PushPolicy(_ context.Context, r engine.PolicyPush) (*engine.PolicyRevision, error) {
	if len(r.Files) == 0 {
		return nil, engine.ErrInvalidRequest
	}
	return &engine.PolicyRevision{Revision: 2, Status: "draft"}, nil
}
func (m *mockService) EvaluatePolicy(_ context.Context, rev int64, r engine.PolicyEvaluationRequest) (*engine.PolicyEvaluation, error) {
	if rev == 9 {
		return nil, engine.ErrUnavailable
	}
	return &engine.PolicyEvaluation{Decision: governance.Decision{Revision: "1:abc"}}, nil
}
func (m *mockService) PolicyImpact(_ context.Context, rev int64, since time.Duration) (*engine.PolicyImpact, error) {
	m.op = "impact:" + since.String()
	return &engine.PolicyImpact{Evaluated: 3}, nil
}
func (m *mockService) PolicyAccess(_ context.Context, rev int64, r engine.PolicyAccessRequest) (*engine.PolicyAccess, error) {
	m.op = "access:" + r.Subject
	return &engine.PolicyAccess{Revision: strconv.FormatInt(rev, 10), Subject: r.Subject, Grants: []engine.Grant{{Name: "team", Actions: []string{"deployment.read"}}}}, nil
}
func (m *mockService) ApprovePolicy(_ context.Context, rev int64) error {
	m.op = "approve"
	return nil
}
func (m *mockService) ActivatePolicy(_ context.Context, rev int64) error {
	m.op = "activate"
	return nil
}

func TestRottePolicyEFreeze(t *testing.T) {
	svc := &mockService{}
	r := newTestRouter(svc)
	if w := do(r, http.MethodPost, "/api/v1/freezes", `{"name":"f1","applications":["*"],"environments":["prod"],"to":"2030-01-01T00:00:00Z"}`); w.Code != http.StatusCreated || svc.op != "freeze:f1" {
		t.Errorf("creazione del freeze: %d %s", w.Code, w.Body)
	}
	if w := do(r, http.MethodPost, "/api/v1/freezes", `{"name":"f1","campo":1}`); w.Code != http.StatusBadRequest {
		t.Errorf("campo sconosciuto: %d", w.Code)
	}
	if w := do(r, http.MethodDelete, "/api/v1/freezes/f1", ""); w.Code != http.StatusNoContent {
		t.Errorf("eliminazione: %d", w.Code)
	}
	if w := do(r, http.MethodDelete, "/api/v1/freezes/altro", ""); w.Code != http.StatusNotFound {
		t.Errorf("freeze inesistente: %d", w.Code)
	}
	w := do(r, http.MethodGet, "/api/v1/policies", "")
	var l engine.PolicyList
	if _ = json.Unmarshal(w.Body.Bytes(), &l); w.Code != http.StatusOK || l.Current != "1:abc" {
		t.Errorf("elenco: %d %s", w.Code, w.Body)
	}
	if w := do(r, http.MethodGet, "/api/v1/policies/x", ""); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("revisione non valida: %d", w.Code)
	}
	if w := do(r, http.MethodPost, "/api/v1/policies", `{"files":{"p.rego":"package x"}}`); w.Code != http.StatusCreated {
		t.Errorf("push: %d %s", w.Code, w.Body)
	}
	if w := do(r, http.MethodPost, "/api/v1/policies/current/evaluate", `{"descriptor":`+validJSON+`}`); w.Code != http.StatusOK {
		t.Errorf("valutazione: %d %s", w.Code, w.Body)
	}
	if w := do(r, http.MethodPost, "/api/v1/policies/9/evaluate", `{"descriptor":`+validJSON+`}`); w.Code != http.StatusServiceUnavailable {
		t.Errorf("policy non disponibile: atteso 503, ricevuto %d", w.Code)
	}
	if w := do(r, http.MethodPost, "/api/v1/policies/3/access", `{"subject":"oidc:alice","groups":["team-a"]}`); w.Code != http.StatusOK || svc.op != "access:oidc:alice" {
		t.Errorf("simulazione degli accessi: %d %s %s", w.Code, w.Body, svc.op)
	}
	if w := do(r, http.MethodPost, "/api/v1/policies/3/access", `{"subject":"x","check":{"action":"deployment.submit","descriptor":{}}}`); w.Code != http.StatusBadRequest {
		t.Errorf("descrittore non valido nella simulazione: %d", w.Code)
	}
	if w := do(r, http.MethodGet, "/api/v1/policies/2/impact?since=30d", ""); w.Code != http.StatusOK || svc.op != "impact:720h0m0s" {
		t.Errorf("impatto: %d %s %s", w.Code, w.Body, svc.op)
	}
	if w := do(r, http.MethodPost, "/api/v1/policies/2/approve", ""); w.Code != http.StatusNoContent || svc.op != "approve" {
		t.Errorf("approvazione: %d", w.Code)
	}
	if w := do(r, http.MethodPost, "/api/v1/policies/1/activate", ""); w.Code != http.StatusNoContent || svc.op != "activate" {
		t.Errorf("riattivazione: %d", w.Code)
	}
}
