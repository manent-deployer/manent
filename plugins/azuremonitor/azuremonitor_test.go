package azuremonitor

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/manent-deployer/manent/internal/azurefake"
	"github.com/manent-deployer/manent/internal/rollout"
)

func TestSerieEValore(t *testing.T) {
	f := azurefake.New("sp-secret")
	f.Series = []azurefake.Series{{Match: "requests", Values: []float64{0.01, 0.02, 0.5}}}
	srv := httptest.NewServer(f.Handler())
	defer srv.Close()
	id := "11111111-2222-3333-4444-555555555555"
	s := &Source{WorkspaceID: id, Endpoint: srv.URL, Authority: srv.URL, TenantID: id, ClientID: id, ClientSecretRef: "vault:sp",
		Secret: func(_ context.Context, ref string) (string, error) { return "sp-secret", nil }}
	end := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	q := "AppRequests | where AppRoleName == 'shop' | summarize avg(toint(Success == false)) by bin(TimeGenerated, {step})"
	v, err := s.Series(context.Background(), strings.Replace(q, "AppRequests", "requests", 1), end.Add(-10*time.Minute), end, time.Minute)
	if err != nil || len(v) != 3 || v[2] != 0.5 {
		t.Fatalf("serie: %v %v", v, err)
	}
	if got := f.Queries[0]; !strings.Contains(got, "bin(TimeGenerated, 60s)") || !strings.HasSuffix(got, "| 2026-10-17T11:50:00Z/2026-10-17T12:00:00Z") {
		t.Errorf("query inviata: %s", got)
	}
	if x, err := s.Value(context.Background(), "requests", end); err != nil || x != 0.5 {
		t.Errorf("valore: %v %v", x, err)
	}
	if _, err := s.Value(context.Background(), "nothing", end); !errors.Is(err, rollout.ErrNoData) {
		t.Errorf("nessun dato: %v", err)
	}
	s.Secret = func(context.Context, string) (string, error) { return "wrong", nil }
	s2 := &Source{WorkspaceID: id, Endpoint: srv.URL, Authority: srv.URL, TenantID: id, ClientID: id, ClientSecretRef: "x", Secret: s.Secret}
	if _, err := s2.Value(context.Background(), "requests", end); err == nil || !strings.Contains(err.Error(), "AADSTS") {
		t.Errorf("credenziali errate: %v", err)
	}
}
