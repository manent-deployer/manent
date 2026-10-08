package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/joeg-ita/go-dart/internal/descriptor"
	"github.com/joeg-ita/go-dart/internal/engine"
	"github.com/joeg-ita/go-dart/internal/pipeline"
)

// mockService implementa engine.Service per i test degli handler.
type mockService struct {
	submitted *descriptor.Descriptor
}

func (m *mockService) Plan(*descriptor.Descriptor) (pipeline.Plan, error) {
	return pipeline.Plan{{Name: "deploy"}}, nil
}

func (m *mockService) Submit(_ context.Context, d *descriptor.Descriptor) (string, error) {
	m.submitted = d
	return "dep-1", nil
}

func (m *mockService) Status(_ context.Context, id string) (*engine.Status, error) {
	if id != "dep-1" {
		return nil, engine.ErrNotFound
	}
	return &engine.Status{ID: id, State: "SUCCESS"}, nil
}

func (m *mockService) Wait(context.Context, string) (*engine.Result, error) { return nil, nil }

const validJSON = `{
  "apiVersion": "go-dart/v1",
  "application": "app",
  "environment": "test",
  "artifact": {"type": "application", "name": "app", "version": "1.0.0", "source": "/tmp/app.tar.gz"},
  "deploy": {"targets": [{"host": "h1", "user": "deploy"}]}
}`

func newTestRouter(svc engine.Service) *gin.Engine {
	gin.SetMode(gin.TestMode)
	return NewRouter(svc, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func do(r http.Handler, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	return w
}

func TestSubmit(t *testing.T) {
	svc := &mockService{}
	w := do(newTestRouter(svc), http.MethodPost, "/api/v1/deployments", validJSON)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["id"] != "dep-1" || svc.submitted.Application != "app" {
		t.Errorf("risposta inattesa: %v", resp)
	}
}

func TestSubmitDescrittoreNonValido(t *testing.T) {
	w := do(newTestRouter(&mockService{}), http.MethodPost, "/api/v1/deployments", `{"apiVersion":"go-dart/v1","campo":1}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("atteso 400, ricevuto %d", w.Code)
	}
}

func TestStatus(t *testing.T) {
	r := newTestRouter(&mockService{})
	if w := do(r, http.MethodGet, "/api/v1/deployments/dep-1", ""); w.Code != http.StatusOK {
		t.Errorf("atteso 200, ricevuto %d", w.Code)
	}
	if w := do(r, http.MethodGet, "/api/v1/deployments/altro", ""); w.Code != http.StatusNotFound {
		t.Errorf("atteso 404, ricevuto %d", w.Code)
	}
}
