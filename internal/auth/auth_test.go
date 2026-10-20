package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

func TestTokens(t *testing.T) {
	sum := sha256.Sum256([]byte("s3cr3t"))
	ts := Tokens{{Name: "ci", SHA256: hex.EncodeToString(sum[:]), Groups: []string{"ci"}}}
	p, err := ts.Authenticate(context.Background(), "s3cr3t")
	if err != nil {
		t.Fatal(err)
	}
	if p.String() != "token:ci" || !slices.Equal(p.Groups, []string{"ci"}) {
		t.Errorf("identità inattesa: %+v", p)
	}
	if _, err := ts.Authenticate(context.Background(), "altro"); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("atteso ErrUnauthenticated, ricevuto %v", err)
	}
}

func TestBearerToken(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if _, ok := BearerToken(r); ok {
		t.Error("nessun token atteso")
	}
	r.Header.Set("Authorization", "bearer abc")
	if tok, ok := BearerToken(r); !ok || tok != "abc" {
		t.Errorf("token inatteso: %q %v", tok, ok)
	}
	r.Header.Set("Authorization", "Basic abc")
	if _, ok := BearerToken(r); ok {
		t.Error("lo schema Basic non è un bearer")
	}
}

// fakeIssuer is a minimal OIDC identity provider: discovery, JWKS and token signing.
type fakeIssuer struct {
	srv *httptest.Server
	key *rsa.PrivateKey
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIssuer{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                f.srv.URL,
			"jwks_uri":                              f.srv.URL + "/jwks",
			"authorization_endpoint":                f.srv.URL + "/auth",
			"token_endpoint":                        f.srv.URL + "/token",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIssuer) token(t *testing.T, claims map[string]any) string {
	t.Helper()
	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: f.key}, (&jose.SignerOptions{}).WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jwt.Signed(sig).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestOIDC(t *testing.T) {
	iss := newFakeIssuer(t)
	o, err := NewOIDC(context.Background(), OIDCConfig{Issuer: iss.srv.URL, Audience: "manent", PolicyClaims: []string{"email", "employee_type", "admin"}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	claims := func(aud string, exp time.Time) map[string]any {
		return map[string]any{
			"iss": iss.srv.URL, "aud": aud, "sub": "u-123", "exp": exp.Unix(), "iat": now.Unix(),
			"preferred_username": "alice", "groups": []string{"team-a", "release-managers"},
			"email": "alice@example.com", "employee_type": "contractor", "admin": true, "salary": "secret",
		}
	}
	ctx := context.Background()
	p, err := o.Authenticate(ctx, iss.token(t, claims("manent", now.Add(time.Hour))))
	if err != nil {
		t.Fatal(err)
	}
	if p.String() != "oidc:alice" || !slices.Equal(p.Groups, []string{"team-a", "release-managers"}) {
		t.Errorf("identità inattesa: %+v", p)
	}
	// Only the listed claims reach the permission policy.
	if len(p.Claims) != 3 || p.Claims["email"] != "alice@example.com" || p.Claims["employee_type"] != "contractor" || p.Claims["admin"] != "true" {
		t.Errorf("claim esposti: %v", p.Claims)
	}
	if _, err := o.Authenticate(ctx, iss.token(t, claims("altra-app", now.Add(time.Hour)))); err == nil {
		t.Error("audience errata accettata")
	}
	if _, err := o.Authenticate(ctx, iss.token(t, claims("manent", now.Add(-time.Minute)))); err == nil {
		t.Error("token scaduto accettato")
	}
	// A value that is not a JWT goes to the next authenticator of the chain.
	if _, err := o.Authenticate(ctx, "token-statico"); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("atteso ErrUnauthenticated, ricevuto %v", err)
	}

	// Chain: static token and OIDC.
	sum := sha256.Sum256([]byte("ci-token"))
	chain := Chain{Tokens{{Name: "ci", SHA256: hex.EncodeToString(sum[:])}}, o}
	if p, err := chain.Authenticate(ctx, "ci-token"); err != nil || p.Method != MethodToken {
		t.Errorf("token statico non riconosciuto: %+v %v", p, err)
	}
	if p, err := chain.Authenticate(ctx, iss.token(t, claims("manent", now.Add(time.Hour)))); err != nil || p.Method != MethodOIDC {
		t.Errorf("token OIDC non riconosciuto: %+v %v", p, err)
	}
	if _, err := chain.Authenticate(ctx, "sconosciuto"); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("atteso ErrUnauthenticated, ricevuto %v", err)
	}
}
