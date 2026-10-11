package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func newUIRouter(svc *mockService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	ui := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ui:"+r.URL.Path) })
	return NewRouterWith(svc, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{
		UI:            ui,
		UIConfig:      &UIConfig{AuthMode: UIAuthOIDC, Issuer: "https://idp.example.com", ClientID: "manent-ui", Scopes: []string{"openid"}, RedirectPath: "/auth/callback", Version: "1.2.3"},
		EventInterval: 5 * time.Millisecond,
	})
}

func TestInterfacciaWeb(t *testing.T) {
	r := newUIRouter(&mockService{})
	w := do(r, http.MethodGet, "/ui/config.json", "")
	var cfg UIConfig
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &cfg) != nil || cfg.ClientID != "manent-ui" || cfg.AuthMode != UIAuthOIDC {
		t.Fatalf("config della UI: %d %s", w.Code, w.Body.String())
	}
	if w = do(r, http.MethodGet, "/inventory", ""); w.Body.String() != "ui:/inventory" {
		t.Errorf("i percorsi della UI vanno all'handler della UI: %q", w.Body.String())
	}
	if w = do(r, http.MethodGet, "/api/v1/sconosciuto", ""); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "error") {
		t.Errorf("una rotta API sconosciuta resta un 404 JSON: %d %q", w.Code, w.Body.String())
	}
	if w = do(r, http.MethodPost, "/inventory", ""); w.Code != http.StatusNotFound {
		t.Errorf("POST fuori dall'API: %d", w.Code)
	}
	if w = do(r, http.MethodGet, "/healthz", ""); w.Code != http.StatusOK {
		t.Errorf("le sonde hanno la precedenza sulla UI: %d", w.Code)
	}
}

func TestMeApprovazioniECursore(t *testing.T) {
	svc := &mockService{}
	r := newUIRouter(svc)
	w := do(r, http.MethodGet, "/api/v1/me", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"subject":"none:anonymous"`) {
		t.Errorf("me: %d %s", w.Code, w.Body.String())
	}
	w = do(r, http.MethodGet, "/api/v1/approvals", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id":"dep-2"`) {
		t.Errorf("approvals: %d %s", w.Code, w.Body.String())
	}
	w = do(r, http.MethodGet, "/api/v1/deployments?limit=1&cursor=dep-0", "")
	if svc.filter.Cursor != "dep-0" || !strings.Contains(w.Body.String(), `"next_cursor":"dep-1"`) {
		t.Errorf("cursore: %+v %s", svc.filter, w.Body.String())
	}
	if w = do(r, http.MethodGet, "/api/v1/deployments?limit=5", ""); strings.Contains(w.Body.String(), "next_cursor") {
		t.Errorf("una pagina non piena non ha seguito: %s", w.Body.String())
	}
}

func TestEventiSSE(t *testing.T) {
	r := newUIRouter(&mockService{})
	w := do(r, http.MethodGet, "/api/v1/deployments/dep-1/events", "")
	body := w.Body.String()
	if w.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content type: %q", w.Header().Get("Content-Type"))
	}
	iStatus, iLog, iEnd := strings.Index(body, "event: status\n"), strings.Index(body, "id: 2\nevent: log\n"), strings.Index(body, "event: end\n")
	if iStatus < 0 || iLog < iStatus || iEnd < iLog || !strings.Contains(body, `"message":"due"`) {
		t.Fatalf("eventi inattesi:\n%s", body)
	}

	// Ripresa dopo una disconnessione: nessuna riga già ricevuta.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/deployments/dep-1/events", nil)
	req.Header.Set("Last-Event-ID", "2")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if strings.Contains(w.Body.String(), "event: log") || !strings.Contains(w.Body.String(), "event: end") {
		t.Errorf("ripresa da Last-Event-ID:\n%s", w.Body.String())
	}

	if w = do(r, http.MethodGet, "/api/v1/deployments/manca/events", ""); w.Code != http.StatusNotFound {
		t.Errorf("deploy inesistente: %d", w.Code)
	}
	if w = do(r, http.MethodGet, "/api/v1/deployments/dep-1/events?offset=-1", ""); w.Code != http.StatusBadRequest {
		t.Errorf("offset non valido: %d", w.Code)
	}
}
