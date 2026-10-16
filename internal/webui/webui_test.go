package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func serve(h http.Handler, method, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w
}

func TestHandler(t *testing.T) {
	files := fstest.MapFS{
		"index.html":           {Data: []byte("<!doctype html><title>manent</title>")},
		"assets/app-1a2b3c.js": {Data: []byte("console.log(1)")},
		"favicon.svg":          {Data: []byte("<svg/>")},
	}
	h := newHandler(files, Options{ConnectOrigins: []string{"https://idp.example.com/realms/corp", "non un url"}})

	w := serve(h, http.MethodGet, "/deployments/dep-1")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<title>manent</title>") {
		t.Fatalf("fallback SPA: %d %q", w.Code, w.Body.String())
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("index.html non deve stare in cache: %q", cc)
	}
	csp := w.Header().Get("Content-Security-Policy")
	for _, want := range []string{"script-src 'self'", "frame-ancestors 'none'", "connect-src 'self' https://idp.example.com", "object-src 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP senza %q: %s", want, csp)
		}
	}
	if strings.Contains(csp, "script-src 'self' 'unsafe") || strings.Contains(csp, "unsafe-eval") || strings.Contains(csp, "non un url") {
		t.Errorf("CSP troppo permissiva: %s", csp)
	}
	for k, v := range map[string]string{"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer"} {
		if w.Header().Get(k) != v {
			t.Errorf("%s = %q, atteso %q", k, w.Header().Get(k), v)
		}
	}

	w = serve(h, http.MethodGet, "/assets/app-1a2b3c.js")
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("asset con hash: %d %q", w.Code, w.Header().Get("Cache-Control"))
	}
	if w = serve(h, http.MethodGet, "/assets/manca.js"); w.Code != http.StatusNotFound {
		t.Errorf("un file inesistente è un 404, non la pagina: %d", w.Code)
	}
	if w = serve(h, http.MethodGet, "/../../etc/passwd"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "manent") {
		t.Errorf("percorso fuori dalla build: %d", w.Code)
	}
	if w = serve(h, http.MethodPost, "/"); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", w.Code)
	}
	if w = serve(h, http.MethodHead, "/inventory"); w.Code != http.StatusOK {
		t.Errorf("HEAD: %d", w.Code)
	}

	// Without a build: a page that explains how to build it.
	empty := newHandler(fstest.MapFS{".gitkeep": {}}, Options{})
	if empty.Built() {
		t.Error("senza index.html la UI non è costruita")
	}
	if w = serve(empty, http.MethodGet, "/"); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "make web") {
		t.Errorf("pagina senza build: %d %q", w.Code, w.Body.String())
	}
}
