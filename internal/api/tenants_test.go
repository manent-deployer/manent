package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/joeg-ita/manent/internal/engine"
)

func (m *mockService) Tenants(_ context.Context, f engine.TenantFilter) ([]engine.TenantInfo, error) {
	m.op = "tenants:" + f.Selector
	return []engine.TenantInfo{{Name: "acme"}}, nil
}
func (m *mockService) Tenant(_ context.Context, name string) (*engine.TenantInfo, error) {
	if name != "acme" {
		return nil, engine.ErrNotFound
	}
	return &engine.TenantInfo{Name: name}, nil
}

func TestRotteDeiTenant(t *testing.T) {
	svc := &mockService{}
	r := newTestRouter(svc)
	if w := do(r, http.MethodGet, "/api/v1/tenants?selector=tier%3Dgold", ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "acme") || svc.op != "tenants:tier=gold" {
		t.Errorf("elenco: %d %s %s", w.Code, w.Body, svc.op)
	}
	if w := do(r, http.MethodGet, "/api/v1/tenants/acme", ""); w.Code != http.StatusOK {
		t.Errorf("tenant: %d", w.Code)
	}
	if w := do(r, http.MethodGet, "/api/v1/tenants/nessuno", ""); w.Code != http.StatusNotFound {
		t.Errorf("tenant inesistente: %d", w.Code)
	}
	if w := do(r, http.MethodGet, "/api/v1/deployments?tenant=acme", ""); w.Code != http.StatusOK || svc.filter.Tenant != "acme" {
		t.Errorf("filtro per tenant: %d %+v", w.Code, svc.filter)
	}
}
