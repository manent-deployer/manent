package artifact

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestStoreEnsureECleanup(t *testing.T) {
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "app.tgz")
	if err := os.WriteFile(src, []byte("contenuto"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Two workers with different working directories, like two separate nodes.
	w1 := &Store{Fetcher: DefaultFetcher{}, WorkDir: t.TempDir()}
	w2 := &Store{Fetcher: DefaultFetcher{}, WorkDir: t.TempDir()}

	f, err := w1.Fetch(ctx, "d1", src)
	if err != nil {
		t.Fatal(err)
	}
	if p, refetched, err := w1.Ensure(ctx, "d1", src, f.LocalPath, f.SHA256); err != nil || refetched || p != f.LocalPath {
		t.Fatalf("sullo stesso worker il file va riusato: %s %v %v", p, refetched, err)
	}

	p, refetched, err := w2.Ensure(ctx, "d1", src, f.LocalPath+".assente", f.SHA256)
	if err != nil || !refetched {
		t.Fatalf("su un altro worker il file va riscaricato: %v %v", refetched, err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("artefatto riscaricato mancante: %v", err)
	}

	// If the source changes after the artifacts step, the deploy must fail.
	if err := os.WriteFile(src, []byte("modificato"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w2.Ensure(ctx, "d2", src, "", f.SHA256); err == nil {
		t.Fatal("atteso errore per sha256 diverso")
	}

	if err := w1.Cleanup(ctx, "d1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(f.LocalPath)); !os.IsNotExist(err) {
		t.Errorf("directory del deploy non rimossa: %v", err)
	}
	for _, id := range []string{"", "..", "../x", "a/b"} {
		if err := w1.Cleanup(ctx, id); err == nil {
			t.Errorf("Cleanup(%q) doveva fallire", id)
		}
	}
}

// mapProvider is an in-memory secrets.Provider.
type mapProvider map[string]string

func (m mapProvider) Resolve(_ context.Context, ref string) (string, error) {
	v, ok := m[ref]
	if !ok {
		return "", errors.New("riferimento sconosciuto")
	}
	return v, nil
}

func TestSorgentiConCredenziali(t *testing.T) {
	var gotAuth, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotKey = r.Header.Get("Authorization"), r.Header.Get("X-Api-Key")
		_, _ = w.Write([]byte("contenuto"))
	}))
	defer srv.Close()

	sp := mapProvider{"vault:nexus#password": "pw", "env:API_KEY": "k"}
	srcs, err := NewSources([]Source{
		{Prefix: srv.URL + "/releases/", Auth: &SourceAuth{Type: AuthBasic, Username: "manent", SecretRef: "vault:nexus#password"}},
		{Prefix: srv.URL + "/releases/special/", Auth: &SourceAuth{Type: AuthHeader, Header: "X-Api-Key", SecretRef: "env:API_KEY"}},
	}, false, sp, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	f := DefaultFetcher{Sources: srcs}
	dir := t.TempDir()
	if _, err := f.Fetch(context.Background(), srv.URL+"/releases/app-1.0.tgz", dir); err != nil {
		t.Fatal(err)
	}
	if want := "Basic " + base64.StdEncoding.EncodeToString([]byte("manent:pw")); gotAuth != want {
		t.Errorf("Authorization = %q, atteso %q", gotAuth, want)
	}
	// The longest prefix wins.
	if _, err := f.Fetch(context.Background(), srv.URL+"/releases/special/app.tgz", dir); err != nil {
		t.Fatal(err)
	}
	if gotKey != "k" || gotAuth != "" {
		t.Errorf("credenziali inattese: auth=%q key=%q", gotAuth, gotKey)
	}
	// Unlisted sources: rejected, both URLs and local paths.
	for _, src := range []string{srv.URL + "/altro/app.tgz", "/etc/passwd", "file:///etc/passwd"} {
		if err := srcs.Check(src); err == nil {
			t.Errorf("%s: sorgente non elencata ammessa", src)
		}
	}
	if _, err := f.Fetch(context.Background(), "/etc/passwd", dir); err == nil {
		t.Error("download da una sorgente non elencata eseguito")
	}
}

func TestSorgentiFile(t *testing.T) {
	dir := t.TempDir()
	srcs, err := NewSources([]Source{{Prefix: "file://" + filepath.ToSlash(dir) + "/"}}, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := srcs.Check(filepath.Join(dir, "app.tgz")); err != nil {
		t.Errorf("percorso nella directory ammessa rifiutato: %v", err)
	}
	if err := srcs.Check(filepath.Join(dir, "..", "fuori.tgz")); err == nil {
		t.Error("percorso fuori dalla directory ammessa accettato")
	}
	if _, err := NewSources([]Source{{Prefix: "ftp://x/"}}, false, nil, nil); err == nil {
		t.Error("atteso errore per un prefisso non supportato")
	}
	if _, err := NewSources([]Source{{Prefix: "https://x/", Auth: &SourceAuth{Type: AuthBasic, SecretRef: "env:X"}}}, false, nil, nil); err == nil {
		t.Error("atteso errore per basic senza username")
	}
}
