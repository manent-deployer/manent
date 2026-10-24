// Command oidc is a minimal OIDC identity provider for the use case lab: login by choosing a
// test user (no password), Authorization Code with PKCE, refresh tokens and RS256 JWTs with
// the groups claim. Never use it outside the lab.
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// users are the test users with the groups used by the lab's RBAC rules.
var users = map[string][]string{
	"ci":     {"ci"},
	"cab":    {"cab"},
	"admin":  {"admins"},
	"viewer": {"viewers"},
	// UC25: two members of team-uc25, an employee and a contractor (claim employee_type).
	"dave":  {"team-uc25"},
	"carol": {"team-uc25"},
}

// extraClaims are the claims besides name and groups, exposed by manent to the permission
// policy through auth.oidc.policy_claims.
var extraClaims = map[string]map[string]any{
	"dave":  {"employee_type": "employee", "email": "dave@uc.example"},
	"carol": {"employee_type": "contractor", "email": "carol@uc.example"},
}

var order = []string{"viewer", "ci", "cab", "admin"}

type grant struct {
	user, clientID, redirect, challenge, nonce string
	expires                                    time.Time
}

type server struct {
	issuer, audience, clientID string
	key                        *rsa.PrivateKey
	signer                     jose.Signer
	mu                         sync.Mutex
	codes                      map[string]grant
	refresh                    map[string]grant
}

func main() {
	listen := flag.String("listen", ":9000", "listen address")
	issuer := flag.String("issuer", "http://oidc:9000", "issuer published in the tokens")
	audience := flag.String("audience", "manent", "token audience")
	clientID := flag.String("client-id", "manent-ui", "allowed public OIDC client")
	flag.Parse()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: "uc", Algorithm: "RS256"}},
		(&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		log.Fatal(err)
	}
	s := &server{issuer: *issuer, audience: *audience, clientID: *clientID, key: key, signer: signer,
		codes: map[string]grant{}, refresh: map[string]grant{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", s.discovery)
	mux.HandleFunc("/keys", s.keys)
	mux.HandleFunc("/authorize", s.authorize)
	mux.HandleFunc("/token", s.token)
	mux.HandleFunc("/logout", s.logout)
	mux.HandleFunc("/lab/token", s.labToken)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	log.Printf("test identity provider on %s, issuer %s", *listen, *issuer)
	srv := &http.Server{Addr: *listen, Handler: cors(mux), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

// cors lets the UI (another origin) read discovery, keys and tokens.
func cors(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *server) discovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                s.issuer,
		"authorization_endpoint":                s.issuer + "/authorize",
		"token_endpoint":                        s.issuer + "/token",
		"jwks_uri":                              s.issuer + "/keys",
		"end_session_endpoint":                  s.issuer + "/logout",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"code_challenge_methods_supported":      []string{"S256"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"scopes_supported":                      []string{"openid", "profile", "offline_access"},
	})
}

func (s *server) keys(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &s.key.PublicKey, KeyID: "uc", Algorithm: "RS256", Use: "sig"}}})
}

var loginPage = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Lab sign-in</title>
<style>body{font-family:system-ui,sans-serif;background:#0a0e17;color:#e2e8f0;display:grid;place-items:center;min-height:100vh;margin:0}
main{background:#0f1626;border:1px solid #334155;border-radius:12px;padding:2rem;max-width:24rem}
button{display:block;width:100%;margin:.5rem 0;padding:.7rem;border-radius:8px;border:1px solid #334155;background:#151e32;color:#e2e8f0;font-size:1rem;cursor:pointer}
button:focus-visible{outline:2px solid #22d3ee;outline-offset:2px}</style></head>
<body><main><h1>Test identity provider</h1><p>Choose the user to sign in with.</p>
<form method="post" action="/authorize">
{{range $k, $v := .Params}}<input type="hidden" name="{{$k}}" value="{{$v}}">{{end}}
{{range .Users}}<button type="submit" name="user" value="{{.}}">{{.}}</button>{{end}}
</form></main></body></html>`))

func random() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// sessionCookie remembers the chosen user, like a real identity provider's SSO session:
// after a UI reload the login completes without asking again.
const sessionCookie = "uc_session"

// authorize shows the user choice (GET) and, on choice (POST), redirects to the client with
// the authorization code. With an active session the GET redirects at once.
func (s *server) authorize(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	q := r.Form
	if q.Get("client_id") != s.clientID || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		http.Error(w, "invalid authorization request (client_id, response_type=code and PKCE S256 required)", http.StatusBadRequest)
		return
	}
	redirect, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || (redirect.Scheme != "http" && redirect.Scheme != "https") {
		http.Error(w, "invalid redirect_uri", http.StatusBadRequest)
		return
	}
	user := q.Get("user")
	if c, err := r.Cookie(sessionCookie); err == nil && r.Method == http.MethodGet && q.Get("prompt") != "login" {
		if _, ok := users[c.Value]; ok {
			user = c.Value
		}
	}
	if r.Method == http.MethodGet && user == "" {
		params := map[string]string{}
		for _, k := range []string{"client_id", "redirect_uri", "response_type", "scope", "state", "nonce", "code_challenge", "code_challenge_method"} {
			params[k] = q.Get(k)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = loginPage.Execute(w, map[string]any{"Params": params, "Users": order})
		return
	}
	if _, ok := users[user]; !ok {
		http.Error(w, "unknown user", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: user, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
	code := random()
	s.mu.Lock()
	s.codes[code] = grant{user: user, clientID: s.clientID, redirect: redirect.String(), challenge: q.Get("code_challenge"), nonce: q.Get("nonce"), expires: time.Now().Add(time.Minute)}
	s.mu.Unlock()
	v := redirect.Query()
	v.Set("code", code)
	v.Set("state", q.Get("state"))
	redirect.RawQuery = v.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound)
}

// token exchanges the code (verifying PKCE) or a refresh token for new tokens.
func (s *server) token(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.ParseForm() != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var g grant
	switch r.Form.Get("grant_type") {
	case "authorization_code":
		c, ok := s.codes[r.Form.Get("code")]
		delete(s.codes, r.Form.Get("code"))
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if !ok || time.Now().After(c.expires) || c.redirect != r.Form.Get("redirect_uri") ||
			base64.RawURLEncoding.EncodeToString(sum[:]) != c.challenge {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
			return
		}
		g = c
	case "refresh_token":
		c, ok := s.refresh[r.Form.Get("refresh_token")]
		delete(s.refresh, r.Form.Get("refresh_token"))
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
			return
		}
		g = c
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported_grant_type"})
		return
	}
	now := time.Now()
	claims := map[string]any{
		"iss": s.issuer, "sub": g.user, "aud": []string{s.audience, s.clientID}, "azp": s.clientID,
		"iat": now.Unix(), "exp": now.Add(15 * time.Minute).Unix(),
		"preferred_username": g.user, "name": g.user, "groups": users[g.user],
	}
	if g.nonce != "" {
		claims["nonce"] = g.nonce
	}
	for k, v := range extraClaims[g.user] {
		claims[k] = v
	}
	tok, err := jwt.Signed(s.signer).Claims(claims).Serialize()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
		return
	}
	rt := random()
	s.refresh[rt] = g
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": tok, "id_token": tok, "refresh_token": rt, "token_type": "Bearer",
		"expires_in": 900, "scope": "openid profile",
	})
}

// labToken returns an access token of a test user without the browser (UC25 calls the API
// with OIDC identities from the shell): it exists only because this server is lab-only.
func (s *server) labToken(w http.ResponseWriter, r *http.Request) {
	user := r.URL.Query().Get("user")
	if _, ok := users[user]; !ok {
		http.Error(w, "unknown user", http.StatusBadRequest)
		return
	}
	now := time.Now()
	claims := map[string]any{
		"iss": s.issuer, "sub": user, "aud": []string{s.audience}, "iat": now.Unix(), "exp": now.Add(15 * time.Minute).Unix(),
		"preferred_username": user, "name": user, "groups": users[user],
	}
	for k, v := range extraClaims[user] {
		claims[k] = v
	}
	tok, err := jwt.Signed(s.signer).Claims(claims).Serialize()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, _ = w.Write([]byte(tok))
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	to := r.URL.Query().Get("post_logout_redirect_uri")
	if u, err := url.Parse(to); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		_, _ = w.Write([]byte("signed out"))
		return
	}
	http.Redirect(w, r, to, http.StatusFound)
}
