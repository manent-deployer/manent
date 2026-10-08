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
	filter    engine.ListFilter
	op        string
}

func (m *mockService) Plan(*descriptor.Descriptor) (pipeline.Plan, error) {
	return pipeline.Plan{{Name: "deploy"}}, nil
}

func (m *mockService) Submit(_ context.Context, d *descriptor.Descriptor) (*engine.Submission, error) {
	m.submitted = d
	if d.Schedule != nil && d.Schedule.Cron != "" {
		return &engine.Submission{State: engine.StateScheduled, Schedule: d.ScheduleName()}, nil
	}
	return &engine.Submission{ID: "dep-1", State: "ENQUEUED"}, nil
}

func (m *mockService) List(_ context.Context, f engine.ListFilter) ([]engine.Status, error) {
	m.filter = f
	return []engine.Status{{ID: "dep-1"}}, nil
}

func (m *mockService) Cancel(_ context.Context, id string) error {
	if id != "dep-1" {
		return engine.ErrNotFound
	}
	return nil
}

func (m *mockService) ListSchedules(context.Context) ([]engine.ScheduleInfo, error) {
	return []engine.ScheduleInfo{{Name: "nightly"}}, nil
}

func (m *mockService) GetSchedule(_ context.Context, name string) (*engine.ScheduleInfo, error) {
	if name != "nightly" {
		return nil, engine.ErrNotFound
	}
	return &engine.ScheduleInfo{Name: name}, nil
}

func (m *mockService) PauseSchedule(_ context.Context, name string) error {
	m.op = "pause:" + name
	return nil
}

func (m *mockService) ResumeSchedule(_ context.Context, name string) error {
	m.op = "resume:" + name
	return nil
}

func (m *mockService) DeleteSchedule(_ context.Context, name string) error {
	m.op = "delete:" + name
	return nil
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

func TestSubmitScheduleRicorrente(t *testing.T) {
	body := strings.Replace(validJSON, `"deploy":`, `"schedule": {"cron": "0 2 * * *", "name": "nightly"}, "deploy":`, 1)
	w := do(newTestRouter(&mockService{}), http.MethodPost, "/api/v1/deployments", body)
	if w.Code != http.StatusCreated || w.Header().Get("Location") != "/api/v1/schedules/nightly" {
		t.Fatalf("status %d, location %q: %s", w.Code, w.Header().Get("Location"), w.Body)
	}
}

func TestListECancel(t *testing.T) {
	svc := &mockService{}
	r := newTestRouter(svc)
	if w := do(r, http.MethodGet, "/api/v1/deployments?schedule=nightly&limit=5", ""); w.Code != http.StatusOK {
		t.Fatalf("list: status %d", w.Code)
	}
	if svc.filter.Schedule != "nightly" || svc.filter.Limit != 5 {
		t.Errorf("filtro inatteso: %+v", svc.filter)
	}
	if w := do(r, http.MethodGet, "/api/v1/deployments?limit=x", ""); w.Code != http.StatusBadRequest {
		t.Errorf("limit non valido: atteso 400, ricevuto %d", w.Code)
	}
	if w := do(r, http.MethodPost, "/api/v1/deployments/dep-1/cancel", ""); w.Code != http.StatusNoContent {
		t.Errorf("cancel: atteso 204, ricevuto %d", w.Code)
	}
	if w := do(r, http.MethodPost, "/api/v1/deployments/altro/cancel", ""); w.Code != http.StatusNotFound {
		t.Errorf("cancel inesistente: atteso 404, ricevuto %d", w.Code)
	}
}

func TestSchedules(t *testing.T) {
	svc := &mockService{}
	r := newTestRouter(svc)
	if w := do(r, http.MethodGet, "/api/v1/schedules", ""); w.Code != http.StatusOK {
		t.Errorf("list: atteso 200, ricevuto %d", w.Code)
	}
	if w := do(r, http.MethodGet, "/api/v1/schedules/altra", ""); w.Code != http.StatusNotFound {
		t.Errorf("get inesistente: atteso 404, ricevuto %d", w.Code)
	}
	for _, tc := range []struct{ method, path, op string }{
		{http.MethodPost, "/api/v1/schedules/nightly/pause", "pause:nightly"},
		{http.MethodPost, "/api/v1/schedules/nightly/resume", "resume:nightly"},
		{http.MethodDelete, "/api/v1/schedules/nightly", "delete:nightly"},
	} {
		if w := do(r, tc.method, tc.path, ""); w.Code != http.StatusNoContent || svc.op != tc.op {
			t.Errorf("%s %s: status %d, op %q", tc.method, tc.path, w.Code, svc.op)
		}
	}
}
