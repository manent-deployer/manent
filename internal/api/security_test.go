package api

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// clientIP returns the client_ip logged for a request from remote with X-Forwarded-For.
func clientIP(t *testing.T, proxies []string, remote string) string {
	t.Helper()
	var buf bytes.Buffer
	r := NewRouterWith(&mockService{}, nil, slog.New(slog.NewJSONHandler(&buf, nil)), Options{TrustedProxies: proxies})
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.RemoteAddr = remote + ":40000"
	req.Header.Set("X-Forwarded-For", "203.0.113.66")
	r.ServeHTTP(httptest.NewRecorder(), req)
	_, rest, ok := strings.Cut(buf.String(), `"client_ip":"`)
	if !ok {
		t.Fatalf("client_ip not logged: %s", buf.String())
	}
	ip, _, _ := strings.Cut(rest, `"`)
	return ip
}

func TestXForwardedForSoloDaiProxyFidati(t *testing.T) {
	if ip := clientIP(t, nil, "192.0.2.10"); ip != "192.0.2.10" {
		t.Errorf("senza proxy fidati X-Forwarded-For deve essere ignorato: %s", ip)
	}
	if ip := clientIP(t, []string{"10.0.0.0/8"}, "192.0.2.10"); ip != "192.0.2.10" {
		t.Errorf("proxy non fidato creduto: %s", ip)
	}
	if ip := clientIP(t, []string{"10.0.0.0/8"}, "10.1.2.3"); ip != "203.0.113.66" {
		t.Errorf("proxy fidato ignorato: %s", ip)
	}
}

func TestReadyzNonEsponeIlMotivo(t *testing.T) {
	var buf bytes.Buffer
	err := errors.New("dial tcp postgres.internal:5432: connection refused")
	r := NewRouterWith(&mockService{}, nil, slog.New(slog.NewTextHandler(&buf, nil)), Options{
		Ready: func(context.Context) error { return err },
	})
	for range 3 {
		w := do(r, http.MethodGet, "/readyz", "")
		if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "postgres") {
			t.Fatalf("readyz: %d %s", w.Code, w.Body)
		}
	}
	// The reason is logged once, not at every probe.
	if n := strings.Count(buf.String(), "postgres.internal"); n != 1 {
		t.Errorf("motivo registrato %d volte:\n%s", n, buf.String())
	}
}
