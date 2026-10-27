package artifact

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProbeHTTP(t *testing.T) {
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if r.Header.Get("Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("manent:pw")) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/releases/app-1.0.tgz":
			w.Header().Set("Content-Length", "9")
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte("contenuto"))
			}
		case "/no-head/app.tgz":
			if r.Method == http.MethodHead {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			_, _ = w.Write([]byte("x"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	srcs, err := NewSources([]Source{
		{Prefix: srv.URL + "/releases/", Auth: &SourceAuth{Type: AuthBasic, Username: "manent", SecretRef: "env:PW"}},
		{Prefix: srv.URL + "/no-head/", Auth: &SourceAuth{Type: AuthBasic, Username: "manent", SecretRef: "env:PW"}},
		{Prefix: srv.URL + "/anon/"},
	}, false, mapProvider{"env:PW": "pw"}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	f := DefaultFetcher{Sources: srcs}
	ctx := context.Background()

	// HEAD only: nothing is downloaded.
	p, err := f.Probe(ctx, srv.URL+"/releases/app-1.0.tgz")
	if err != nil || p.Size != 9 || p.SHA256 != "" {
		t.Fatalf("probe: %+v %v", p, err)
	}
	if len(methods) != 1 || methods[0] != http.MethodHead {
		t.Errorf("richieste inattese: %v", methods)
	}
	// A server without HEAD: GET closed after the headers.
	if _, err := f.Probe(ctx, srv.URL+"/no-head/app.tgz"); err != nil {
		t.Errorf("server senza HEAD: %v", err)
	}
	for src, want := range map[string]string{
		srv.URL + "/releases/app-9.9.tgz": "not found",
		srv.URL + "/anon/app.tgz":         "access to the artifact denied",
		srv.URL + "/altro/app.tgz":        "not allowed",
	} {
		if _, err := f.Probe(ctx, src); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: atteso %q, ricevuto %v", src, want, err)
		}
	}
}

func TestProbeFileEImmagine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.tgz")
	if err := os.WriteFile(path, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := DefaultFetcher{}
	ctx := context.Background()
	if p, err := f.Probe(ctx, "file://"+path); err != nil || p.Size != 3 {
		t.Errorf("file: %+v %v", p, err)
	}
	for src, want := range map[string]string{filepath.Join(dir, "manca.tgz"): "not found", dir: "not a regular file"} {
		if _, err := f.Probe(ctx, src); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: atteso %q, ricevuto %v", src, want, err)
		}
	}

	// Image: the manifest digest, with the registry token flow.
	srv, digest := fakeRegistry(t, true)
	srcs, err := NewSources([]Source{{Prefix: "oci+http://" + strings.TrimPrefix(srv.URL, "http://") + "/",
		Auth: &SourceAuth{Type: AuthBasic, Username: "ci", SecretRef: "env:REG"}}}, false, mapProvider{"env:REG": "segreto"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f = DefaultFetcher{Sources: srcs}
	host := strings.TrimPrefix(srv.URL, "http://")
	if p, err := f.Probe(ctx, "oci+http://"+host+"/team/app:1.0"); err != nil || p.SHA256 != digest {
		t.Errorf("immagine: %+v %v (digest atteso %s)", p, err, digest)
	}
	if _, err := f.Probe(ctx, "oci+http://"+host+"/team/app:2.0"); err == nil || !strings.Contains(err.Error(), "status 404") {
		t.Errorf("tag inesistente: %v", err)
	}
}
