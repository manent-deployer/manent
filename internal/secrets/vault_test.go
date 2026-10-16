package secrets

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeVault simulates a Vault with a KV v2 mount ("secret"), a KV v1 one ("kv") and AppRole login.
type fakeVault struct {
	srv    *httptest.Server
	logins atomic.Int32
	// revoked makes the current token be rejected once, as after a revocation.
	revoked atomic.Bool
}

func newFakeVault(t *testing.T) *fakeVault {
	t.Helper()
	f := &fakeVault{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		write := func(code int, v any) {
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(v)
		}
		if r.URL.Path == "/v1/auth/approle/login" {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["role_id"] != "role" || body["secret_id"] != "sid" {
				write(http.StatusBadRequest, map[string]any{"errors": []string{"invalid role or secret ID"}})
				return
			}
			n := f.logins.Add(1)
			write(http.StatusOK, map[string]any{"auth": map[string]any{"client_token": "tok-" + string(rune('0'+n)), "lease_duration": 3600}})
			return
		}
		tok := r.Header.Get("X-Vault-Token")
		if tok == "" || (f.revoked.Load() && tok == "tok-1") {
			write(http.StatusForbidden, map[string]any{"errors": []string{"permission denied"}})
			return
		}
		switch r.URL.Path {
		case "/v1/secret/data/myapp/prod":
			write(http.StatusOK, map[string]any{"data": map[string]any{
				"data":     map[string]any{"ssh_key": "PEM", "port": 22},
				"metadata": map[string]any{"version": 3},
			}})
		case "/v1/kv/myapp":
			write(http.StatusOK, map[string]any{"data": map[string]any{"password": "pw"}})
		default:
			write(http.StatusNotFound, map[string]any{"errors": []string{}})
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func TestVaultToken(t *testing.T) {
	f := newFakeVault(t)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("root\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := NewVault(VaultConfig{Address: f.srv.URL, TokenFile: tokenFile})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for ref, want := range map[string]string{"secret/data/myapp/prod#ssh_key": "PEM", "kv/myapp#password": "pw"} {
		got, err := v.Resolve(ctx, ref)
		if err != nil || got != want {
			t.Errorf("%s = %q, %v; atteso %q", ref, got, err, want)
		}
	}
	for _, bad := range []string{"secret/data/myapp/prod", "secret/data/myapp/prod#manca", "secret/data/myapp/prod#port", "secret/data/altro#x", "/v1/x#y", "secret/../sys#x"} {
		_, err := v.Resolve(ctx, bad)
		if err == nil {
			t.Errorf("%s: atteso errore", bad)
		} else if strings.Contains(err.Error(), "PEM") {
			t.Errorf("%s: l'errore contiene un valore segreto: %v", bad, err)
		}
	}
}

func TestVaultAppRole(t *testing.T) {
	f := newFakeVault(t)
	sid := filepath.Join(t.TempDir(), "secret-id")
	if err := os.WriteFile(sid, []byte("sid"), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := NewVault(VaultConfig{Address: f.srv.URL, Auth: VaultAuthAppRole, RoleID: "role", SecretIDFile: sid})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for range 3 {
		if got, err := v.Resolve(ctx, "kv/myapp#password"); err != nil || got != "pw" {
			t.Fatalf("lettura fallita: %q %v", got, err)
		}
	}
	if n := f.logins.Load(); n != 1 {
		t.Errorf("il token va riusato: %d login", n)
	}
	// Token revoked: a new login and the read succeeds.
	f.revoked.Store(true)
	if got, err := v.Resolve(ctx, "kv/myapp#password"); err != nil || got != "pw" {
		t.Fatalf("lettura dopo revoca fallita: %q %v", got, err)
	}
	if n := f.logins.Load(); n != 2 {
		t.Errorf("atteso un secondo login, login totali %d", n)
	}
}

func TestVaultConfigErrata(t *testing.T) {
	for name, cfg := range map[string]VaultConfig{
		"senza indirizzo":  {Address: ""},
		"approle parziale": {Address: "http://v", Auth: VaultAuthAppRole, RoleID: "r"},
		"kubernetes":       {Address: "http://v", Auth: VaultAuthKubernetes},
		"auth sconosciuto": {Address: "http://v", Auth: "ldap"},
	} {
		t.Setenv("VAULT_ADDR", "")
		if _, err := NewVault(cfg); err == nil {
			t.Errorf("%s: atteso errore", name)
		}
	}
}
