package api

import (
	"bytes"
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
