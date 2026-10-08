package artifact

import (
	"context"
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
	// Due worker con directory di lavoro diverse, come due nodi distinti.
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

	// Se la sorgente cambia dopo lo step artifacts, il deploy deve fallire.
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
