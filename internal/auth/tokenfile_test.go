package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestTokenScaduto(t *testing.T) {
	ts := Tokens{
		{Name: "vecchio", SHA256: hashOf("a"), ExpiresAt: time.Now().Add(-time.Minute)},
		{Name: "nuovo", SHA256: hashOf("b"), ExpiresAt: time.Now().Add(time.Hour)},
	}
	if _, err := ts.Authenticate(context.Background(), "a"); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("un token scaduto non è valido: %v", err)
	}
	if p, err := ts.Authenticate(context.Background(), "b"); err != nil || p.Subject != "nuovo" {
		t.Errorf("token valido rifiutato: %v", err)
	}
}

func TestTokenFileRiletto(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.yml")
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		// The check uses modification time and size: force a different time.
		mt := time.Now().Add(time.Duration(len(content)) * time.Second)
		_ = os.Chtimes(path, mt, mt)
	}
	write("tokens:\n  - {name: ci, sha256: " + hashOf("t1") + ", groups: [ci]}\n")
	f, err := NewTokenFile(path, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	f.Interval = time.Nanosecond
	ctx := context.Background()
	if p, err := f.Authenticate(ctx, "t1"); err != nil || p.String() != "token:ci" || p.Groups[0] != "ci" {
		t.Fatalf("token del file rifiutato: %v", err)
	}

	// Revocation: the token is removed from the file.
	write("tokens:\n  - {name: altro, sha256: " + hashOf("t2") + "}\n")
	if _, err := f.Authenticate(ctx, "t1"); !errors.Is(err, ErrUnauthenticated) {
		t.Error("il token revocato è ancora valido")
	}
	if _, err := f.Authenticate(ctx, "t2"); err != nil {
		t.Errorf("il nuovo token non è valido: %v", err)
	}

	// An invalid file does not change the tokens in use.
	write("tokens: [non valido")
	if _, err := f.Authenticate(ctx, "t2"); err != nil {
		t.Errorf("con un file non valido restano i token precedenti: %v", err)
	}

	// A removed file revokes everything.
	_ = os.Remove(path)
	if _, err := f.Authenticate(ctx, "t2"); !errors.Is(err, ErrUnauthenticated) {
		t.Error("con il file rimosso nessun token è valido")
	}
}

func TestTokenFileNonValido(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.yml")
	_ = os.WriteFile(path, []byte("tokens:\n  - {name: ci, sha256: in-chiaro}\n"), 0o600)
	if _, err := NewTokenFile(path, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Error("atteso errore per uno sha256 non valido")
	}
}
