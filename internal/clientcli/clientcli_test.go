package clientcli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/manent-deployer/manent/internal/service"
)

// server returns a fake API that records the received token.
func server(t *testing.T, got *string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(service.Status{ID: "d1", State: service.StateSuccess})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestConfigurazioneDelClient(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	var auth string
	url := server(t, &auth)
	fileToken := write("token-file", "dal-file\n")
	flagToken := write("token-flag", "da-opzione\n")
	cfg := write("client.yml", "server: "+url+"\ntoken_file: "+fileToken+"\ntimeout: 5s\n")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("MANENT_TOKEN", "")

	status := func(g GlobalOptions) error {
		return withService(&g, func(ctx context.Context, svc service.Service) error {
			_, err := svc.Status(ctx, "d1")
			return err
		})
	}
	// Configuration file: server and token.
	if err := status(GlobalOptions{Config: cfg}); err != nil || auth != "Bearer dal-file" {
		t.Fatalf("dal file: %q %v", auth, err)
	}
	// MANENT_TOKEN wins over the token_file of the configuration file...
	t.Setenv("MANENT_TOKEN", "da-variabile")
	if err := status(GlobalOptions{Config: cfg}); err != nil || auth != "Bearer da-variabile" {
		t.Fatalf("da variabile: %q %v", auth, err)
	}
	// ...but not over --token-file (or MANENT_TOKEN_FILE).
	if err := status(GlobalOptions{Config: cfg, TokenFile: flagToken}); err != nil || auth != "Bearer da-opzione" {
		t.Fatalf("da opzione: %q %v", auth, err)
	}
	// --server wins over the file.
	if err := status(GlobalOptions{Config: cfg, Server: "http://127.0.0.1:1"}); err == nil {
		t.Fatal("--server ignorato")
	}
	// An explicitly given file must exist; the default one need not.
	if err := status(GlobalOptions{Config: filepath.Join(dir, "manca.yml")}); err == nil {
		t.Fatal("file di configurazione mancante accettato")
	}
	if c, err := newClient(&GlobalOptions{}); err != nil || c.BaseURL != DefaultServer {
		t.Fatalf("default: %+v %v", c, err)
	}
	// "~/" in the file paths means the home.
	t.Setenv("HOME", dir)
	tilde := write("tilde.yml", "server: "+url+"\ntoken_file: ~/token-file\n")
	t.Setenv("MANENT_TOKEN", "")
	if err := status(GlobalOptions{Config: tilde}); err != nil || auth != "Bearer dal-file" {
		t.Fatalf("token_file con ~: %q %v", auth, err)
	}
	// Unknown keys rejected.
	bad := write("bad.yml", "srever: x\n")
	if err := status(GlobalOptions{Config: bad}); err == nil {
		t.Fatal("chiave sconosciuta accettata")
	}
}
