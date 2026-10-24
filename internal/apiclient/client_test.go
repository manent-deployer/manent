package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manent-deployer/manent/internal/descriptor"
	"github.com/manent-deployer/manent/internal/service"
)

func TestClient(t *testing.T) {
	var polls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/deployments", func(w http.ResponseWriter, r *http.Request) {
		var d descriptor.Descriptor
		_ = json.NewDecoder(r.Body).Decode(&d)
		if d.Environment == "prod" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"permesso negato: token:ci non può eseguire deployment.submit su app/prod"}`))
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"d1","state":"ENQUEUED"}`))
	})
	mux.HandleFunc("GET /api/v1/deployments/d1", func(w http.ResponseWriter, r *http.Request) {
		state := "PENDING"
		if polls.Add(1) >= 3 {
			state = "SUCCESS"
		}
		_ = json.NewEncoder(w).Encode(service.Status{ID: "d1", State: state, Result: &service.Result{DeploymentID: "d1"}})
	})
	mux.HandleFunc("GET /api/v1/deployments/r1", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(service.Status{ID: "r1", State: service.StateRejected})
	})
	mux.HandleFunc("POST /api/v1/deployments/d1/reject", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["comment"] != "no" {
			w.WriteHeader(http.StatusBadRequest)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer segreto" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	defer srv.Close()

	c, err := New(srv.URL+"/", "segreto")
	if err != nil {
		t.Fatal(err)
	}
	c.PollInterval = 10 * time.Millisecond
	ctx := context.Background()
	d := &descriptor.Descriptor{Application: "app", Environment: "test"}
	sub, err := c.Submit(ctx, d)
	if err != nil || sub.ID != "d1" {
		t.Fatalf("submit: %+v %v", sub, err)
	}
	res, err := c.Wait(ctx, "d1")
	if err != nil || res.DeploymentID != "d1" || polls.Load() < 3 {
		t.Fatalf("wait: %+v %v (interrogazioni %d)", res, err, polls.Load())
	}
	if _, err := c.Wait(ctx, "r1"); err == nil {
		t.Error("un deploy respinto è un errore")
	}

	// Server errors become engine errors, with the original message.
	d.Environment = "prod"
	_, err = c.Submit(ctx, d)
	if !errors.Is(err, service.ErrForbidden) || err.Error() != "permesso negato: token:ci non può eseguire deployment.submit su app/prod" {
		t.Errorf("errore inatteso: %v", err)
	}
	if _, err := c.Status(ctx, "altro"); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("atteso ErrNotFound, ricevuto %v", err)
	}
	if err := c.Decide(ctx, "d1", service.DecisionRequest{Comment: "no"}); err != nil {
		t.Errorf("reject: %v", err)
	}

	c.Token = "sbagliato"
	if _, err := c.Status(ctx, "d1"); !errors.Is(err, errAuth) {
		t.Errorf("atteso errore di autenticazione, ricevuto %v", err)
	}
	if _, err := New("ftp://x", ""); err == nil {
		t.Error("URL non http: atteso errore")
	}
}
