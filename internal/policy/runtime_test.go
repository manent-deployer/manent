package policy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-policy-agent/opa/v1/ast"
	"github.com/open-policy-agent/opa/v1/bundle"

	"github.com/joeg-ita/manent/internal/cluster"
	"github.com/joeg-ita/manent/internal/governance"
	"github.com/joeg-ita/manent/internal/service"
	"github.com/joeg-ita/manent/internal/testdb"
)

func openStore(t *testing.T) *cluster.SQL {
	t.Helper()
	s, err := cluster.Open(context.Background(), testdb.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

const twoApprovals = "package manent.governance\nimport rego.v1\ndecision := {\"rule\": \"due\", \"approvals\": {\"required\": 2}}\n"

func TestRevisioniNelloStore(t *testing.T) {
	ctx := context.Background()
	static, err := governance.New([]governance.Rule{{Name: "yaml", Applications: []string{"*"}, Environments: []string{"prod"}, Approvals: governance.Approvals{Required: 1}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err := New(Options{Engine: EngineOPA, Static: static, Store: openStore(t), Review: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Ready(ctx); err == nil {
		t.Fatal("prima del caricamento la policy non è pronta")
	}
	if _, err := r.Evaluate(ctx, input("prod", "deploy", "1.0.0")); !errors.Is(err, governance.ErrUnavailable) {
		t.Fatalf("senza policy caricata: %v", err)
	}
	if err := r.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	// No active revision: the default policy applies the rules of manent.yml.
	d, err := r.Evaluate(ctx, input("prod", "deploy", "1.0.0"))
	if err != nil || d.Rule != "yaml" || d.Approvals.RequiredOrZero() != 1 || !strings.HasPrefix(d.Revision, "default:") {
		t.Fatalf("policy predefinita: %+v %v", d, err)
	}
	if _, err := r.Push(ctx, Files{"p.rego": "package manent.governance\nimport rego.v1\ndecision := {"}, "rotta", "oidc:alice"); !errors.Is(err, service.ErrInvalidRequest) {
		t.Fatalf("revisione che non compila accettata: %v", err)
	}
	rev, err := r.Push(ctx, Files{"p.rego": twoApprovals}, "due approvazioni", "oidc:alice")
	if err != nil || rev.Status != service.PolicyDraft {
		t.Fatalf("push: %+v %v", rev, err)
	}
	// The draft is not in force; its evaluation is available.
	if d, _ := r.Evaluate(ctx, input("prod", "deploy", "1.0.0")); d.Rule != "yaml" {
		t.Errorf("la bozza non deve essere in vigore: %+v", d)
	}
	if d, err := r.EvaluateRevision(ctx, rev.Revision, input("prod", "deploy", "1.0.0")); err != nil || d.Approvals.RequiredOrZero() != 2 {
		t.Errorf("valutazione della bozza: %+v %v", d, err)
	}
	if err := r.Activate(ctx, rev.Revision, "oidc:alice", true); !errors.Is(err, service.ErrForbidden) {
		t.Fatalf("quattro occhi: %v", err)
	}
	if err := r.Activate(ctx, rev.Revision, "oidc:bob", false); !errors.Is(err, service.ErrInvalidRequest) {
		t.Fatalf("una bozza si approva, non si riattiva: %v", err)
	}
	if err := r.Activate(ctx, rev.Revision, "oidc:bob", true); err != nil {
		t.Fatal(err)
	}
	d, err = r.Evaluate(ctx, input("prod", "deploy", "1.0.0"))
	if err != nil || d.Rule != "due" || !strings.HasPrefix(d.Revision, "1:") {
		t.Fatalf("revisione approvata in vigore: %+v %v", d, err)
	}
	list, err := r.List(ctx)
	if err != nil || len(list.Revisions) != 1 || list.Revisions[0].ReviewedBy != "oidc:bob" || list.Current != d.Revision {
		t.Fatalf("elenco: %+v %v", list, err)
	}
	rev2, err := r.Push(ctx, Files{"p.rego": strings.Replace(twoApprovals, "2", "3", 1)}, "", "oidc:alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Activate(ctx, rev2.Revision, "oidc:bob", true); err != nil {
		t.Fatal(err)
	}
	// Back to the first one.
	if err := r.Activate(ctx, rev.Revision, "oidc:carol", false); err != nil {
		t.Fatal(err)
	}
	if d, _ := r.Evaluate(ctx, input("prod", "deploy", "1.0.0")); d.Approvals.RequiredOrZero() != 2 {
		t.Errorf("ritorno alla revisione 1: %+v", d)
	}
}

func TestFreezeARuntime(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	now := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	r, err := New(Options{Store: store, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	w := governance.Window{Name: "incidente-881", Reason: "incidente", Applications: []string{"*"}, Environments: []string{"prod"},
		From: now.Add(-time.Minute), To: now.Add(time.Hour), Hard: true, CreatedBy: "oidc:carol", CreatedAt: now}
	if err := r.CreateFreeze(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateFreeze(ctx, w); !errors.Is(err, service.ErrInvalidRequest) {
		t.Fatalf("nome duplicato: %v", err)
	}
	in := input("prod", "deploy", "1.0.0")
	in.Now = now
	in.Deployment.FreezeOverride = "INC-881"
	d, err := r.Evaluate(ctx, in)
	if err != nil || d.Freeze == nil || !d.Freeze.Hard || !d.Freeze.Dynamic || d.Freeze.CreatedBy != "oidc:carol" {
		t.Fatalf("freeze hard a runtime: %+v %v", d.Freeze, err)
	}
	if ws := r.Windows("shop", "prod", now); len(ws) != 1 {
		t.Errorf("elenco dei freeze: %+v", ws)
	}
	// Another process sees it after its refresh.
	other, _ := New(Options{Store: store, Now: func() time.Time { return now }})
	_ = other.Refresh(ctx)
	if ws := other.Windows("", "", now); len(ws) != 1 {
		t.Errorf("l'altro processo non vede il freeze: %+v", ws)
	}
	if err := r.DeleteFreeze(ctx, w.Name); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteFreeze(ctx, w.Name); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("seconda eliminazione: %v", err)
	}
	if d, _ := r.Evaluate(ctx, in); d.Freeze != nil {
		t.Errorf("freeze eliminato ancora attivo: %+v", d.Freeze)
	}
}

func TestSorgenteFileERevisioneNonValida(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	write := func(src string) {
		t.Helper()
		p := filepath.Join(dir, "governance.rego")
		if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
		// A distinct modification time, even on file systems with a coarse resolution.
		at := time.Now().Add(time.Duration(len(src)) * time.Second)
		_ = os.Chtimes(p, at, at)
	}
	write(twoApprovals)
	now := time.Now()
	r, err := New(Options{Engine: EngineOPA, SourceName: SourceFile, Source: &FileSource{Dir: dir}, MaxStaleness: time.Minute, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	first := r.Revision()
	write("package manent.governance\nimport rego.v1\ndecision := {\"approvals\": {\"required\": -1}} # invalid sample\n")
	if err := r.Refresh(ctx); err == nil {
		t.Fatal("revisione non valida accettata")
	}
	if r.Revision() != first {
		t.Fatalf("la revisione non valida ha sostituito quella in uso: %s", r.Revision())
	}
	if d, err := r.Evaluate(ctx, input("prod", "deploy", "1.0.0")); err != nil || d.Approvals.RequiredOrZero() != 2 {
		t.Fatalf("revisione precedente in vigore: %+v %v", d, err)
	}
	// Beyond max_staleness without a successful refresh, real deploys are refused.
	now = now.Add(2 * time.Minute)
	if _, err := r.Evaluate(ctx, input("prod", "deploy", "1.0.0")); !errors.Is(err, governance.ErrUnavailable) {
		t.Fatalf("policy non aggiornata oltre max_staleness: %v", err)
	}
	write(strings.Replace(twoApprovals, "2", "3", 1))
	if err := r.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if d, err := r.Evaluate(ctx, input("prod", "deploy", "1.0.0")); err != nil || d.Approvals.RequiredOrZero() != 3 {
		t.Fatalf("nuova revisione: %+v %v", d, err)
	}
}

// signedBundle builds a bundle signed with the key (nil = unsigned).
func signedBundle(t *testing.T, key *rsa.PrivateKey, revision string) []byte {
	t.Helper()
	b := bundle.Bundle{
		Manifest: bundle.Manifest{Revision: revision},
		Data:     map[string]any{},
		Modules:  []bundle.ModuleFile{{Path: "/governance.rego", URL: "/governance.rego", Raw: []byte(twoApprovals), Parsed: ast.MustParseModule(twoApprovals)}},
	}
	if key != nil {
		pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
		if err := b.GenerateSignature(bundle.NewSigningConfig(string(pemKey), "RS256", ""), "default", false); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	if err := bundle.NewWriter(&buf).Write(b); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestSorgenteBundleFirmato(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	pubPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub}))
	body, requests, notModified := signedBundle(t, key, "git-abc123"), 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer segreto" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("If-None-Match") == `"v1"` {
			notModified++
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	src := &BundleSource{URL: srv.URL, Verification: NewVerification(pubPEM, "RS256", "", ""),
		Token: func(context.Context) (string, error) { return "segreto", nil }}
	files, origin, changed, err := src.Fetch(context.Background())
	if err != nil || !changed || origin != "git-abc123" || files["governance.rego"] == "" {
		t.Fatalf("primo download: %v %s %v %v", changed, origin, err, files)
	}
	if _, _, changed, err := src.Fetch(context.Background()); err != nil || changed || notModified != 1 {
		t.Fatalf("bundle invariato riscaricato: %v %v %d", changed, err, notModified)
	}
	// An unsigned bundle (or signed with another key) is refused.
	body = signedBundle(t, nil, "x")
	src2 := &BundleSource{URL: srv.URL, Verification: NewVerification(pubPEM, "RS256", "", ""),
		Token: func(context.Context) (string, error) { return "segreto", nil }}
	if _, _, _, err := src2.Fetch(context.Background()); err == nil {
		t.Fatal("bundle non firmato accettato")
	}
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	body = signedBundle(t, other, "y")
	if _, _, _, err := src2.Fetch(context.Background()); err == nil {
		t.Fatal("bundle firmato con un'altra chiave accettato")
	}
}

func TestSorgenteConsul(t *testing.T) {
	index := "7"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/kv/manent/policies/" || r.URL.Query().Get("recurse") != "true" || r.Header.Get("X-Consul-Token") != "tok" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("X-Consul-Index", index)
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"Key": "manent/policies/", "Value": ""},
			{"Key": "manent/policies/governance.rego", "Value": base64.StdEncoding.EncodeToString([]byte(twoApprovals)), "ModifyIndex": 7},
		})
	}))
	defer srv.Close()
	src := &ConsulSource{Address: srv.URL, Token: "tok", Prefix: "manent/policies"}
	files, origin, changed, err := src.Fetch(context.Background())
	if err != nil || !changed || origin != "consul:7" || len(files) != 1 {
		t.Fatalf("lettura da Consul: %v %s %v %v", changed, origin, err, files)
	}
	if _, _, changed, _ := src.Fetch(context.Background()); changed {
		t.Fatal("indice invariato: nessuna nuova revisione attesa")
	}
	index = "8"
	if _, _, changed, _ := src.Fetch(context.Background()); !changed {
		t.Fatal("indice cambiato: nuova revisione attesa")
	}
}
