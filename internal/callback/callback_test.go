package callback

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/joeg-ita/manent/internal/descriptor"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// mapProvider resolves references from a map.
type mapProvider map[string]string

func (m mapProvider) Resolve(_ context.Context, ref string) (string, error) {
	v, ok := m[ref]
	if !ok {
		return "", errors.New("non trovato")
	}
	return v, nil
}

// received is a request recorded by the test server.
type received struct {
	header http.Header
	body   []byte
}

func newServer(t *testing.T, status int) (*httptest.Server, *[]received) {
	t.Helper()
	var got []received
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, received{header: r.Header.Clone(), body: b})
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func newNotifier() HTTPNotifier {
	return HTTPNotifier{
		Client:  NewHTTPClient(5 * time.Second),
		Secrets: mapProvider{"env:TOKEN": "s3cr3t\n"},
		Policy:  Policy{AllowHTTP: true},
		Logger:  discard,
	}
}

func testEvent() Event {
	return Event{ID: "d1:1", Type: descriptor.CallbackEventStep, DeploymentID: "d1", Step: "qa", Status: StatusSucceeded, Timestamp: time.Unix(1700000000, 0)}
}

func TestNotifyAuth(t *testing.T) {
	d := &descriptor.Descriptor{Secrets: []descriptor.SecretRef{{Name: "tok", Ref: "env:TOKEN"}}}
	tests := []struct {
		auth  *descriptor.CallbackAuth
		check func(t *testing.T, r received)
	}{
		{nil, func(t *testing.T, r received) {
			if r.header.Get("Authorization") != "" {
				t.Error("autenticazione non richiesta")
			}
		}},
		{&descriptor.CallbackAuth{Type: "bearer", Secret: "tok"}, func(t *testing.T, r received) {
			if got := r.header.Get("Authorization"); got != "Bearer s3cr3t" {
				t.Errorf("Authorization = %q", got)
			}
		}},
		{&descriptor.CallbackAuth{Type: "basic", Secret: "tok", Username: "manent"}, func(t *testing.T, r received) {
			req := &http.Request{Header: r.header}
			if u, p, ok := req.BasicAuth(); !ok || u != "manent" || p != "s3cr3t" {
				t.Errorf("basic auth inattesa: %q %q", u, p)
			}
		}},
		{&descriptor.CallbackAuth{Type: "header", Secret: "tok", Header: "X-Api-Key"}, func(t *testing.T, r received) {
			if got := r.header.Get("X-Api-Key"); got != "s3cr3t" {
				t.Errorf("X-Api-Key = %q", got)
			}
		}},
		{&descriptor.CallbackAuth{Type: "hmac", Secret: "tok"}, func(t *testing.T, r received) {
			ts := r.header.Get(HeaderTimestamp)
			if ts != "1700000000" || r.header.Get(HeaderSignature) != "sha256="+Sign("s3cr3t", ts, r.body) {
				t.Errorf("firma non valida: %v", r.header)
			}
		}},
	}
	for _, tt := range tests {
		name := "none"
		if tt.auth != nil {
			name = tt.auth.Type
		}
		t.Run(name, func(t *testing.T) {
			srv, got := newServer(t, http.StatusNoContent)
			cb := descriptor.Callback{URL: srv.URL + "/hook", Auth: tt.auth}
			if err := newNotifier().Notify(context.Background(), d, cb, testEvent()); err != nil {
				t.Fatal(err)
			}
			r := (*got)[0]
			var ev Event
			if err := json.Unmarshal(r.body, &ev); err != nil || ev.Step != "qa" || ev.ID != "d1:1" {
				t.Errorf("corpo inatteso: %s", r.body)
			}
			if r.header.Get(HeaderDelivery) != "d1:1" || r.header.Get(HeaderEvent) != "step" {
				t.Errorf("header inattesi: %v", r.header)
			}
			tt.check(t, r)
		})
	}
}

func TestNotifyErrori(t *testing.T) {
	d := &descriptor.Descriptor{}
	srv, _ := newServer(t, http.StatusInternalServerError)
	if err := newNotifier().Notify(context.Background(), d, descriptor.Callback{URL: srv.URL}, testEvent()); err == nil {
		t.Error("atteso errore per status 500")
	}
	// The redirect is not followed: 3xx is an error.
	redir := httptest.NewServer(http.RedirectHandler(srv.URL, http.StatusFound))
	defer redir.Close()
	if err := newNotifier().Notify(context.Background(), d, descriptor.Callback{URL: redir.URL}, testEvent()); err == nil {
		t.Error("atteso errore per redirect")
	}
	// Undeclared auth secret.
	cb := descriptor.Callback{URL: srv.URL, Auth: &descriptor.CallbackAuth{Type: "bearer", Secret: "manca"}}
	if err := newNotifier().Notify(context.Background(), d, cb, testEvent()); err == nil {
		t.Error("atteso errore per segreto mancante")
	}
}

func TestPolicy(t *testing.T) {
	tests := []struct {
		policy Policy
		url    string
		ok     bool
	}{
		{Policy{}, "https://ci.example.com/hook", true},
		{Policy{}, "http://ci.example.com/hook", false},
		{Policy{AllowHTTP: true}, "http://ci.example.com/hook", true},
		{Policy{AllowedHosts: []string{"ci.example.com"}}, "https://CI.example.com/hook", true},
		{Policy{AllowedHosts: []string{"ci.example.com"}}, "https://169.254.169.254/", false},
		{Policy{AllowedHosts: []string{"*.example.com"}}, "https://a.b.example.com:8443/x", true},
		{Policy{AllowedHosts: []string{"*.example.com"}}, "https://example.com.evil.io/", false},
		{Policy{AllowedHosts: []string{"*.example.com"}}, "https://evilexample.com/", false},
	}
	for _, tt := range tests {
		if err := tt.policy.Check(tt.url); (err == nil) != tt.ok {
			t.Errorf("%+v %s: errore %v, atteso ok=%v", tt.policy, tt.url, err, tt.ok)
		}
	}
}
