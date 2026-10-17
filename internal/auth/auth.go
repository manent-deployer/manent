// Package auth identifies who uses manent: OIDC users and static tokens on the API, the
// operating system user on the CLI. The identity travels in the context down to the service,
// which applies the permissions (internal/authz) and records it in the deploy and the audit.
package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os/user"
	"slices"
	"strings"
	"time"
)

// Methods an identity was authenticated with.
const (
	MethodOIDC  = "oidc"
	MethodToken = "token"
	MethodCLI   = "cli"
	// MethodNone means authentication is disabled (auth.mode: none).
	MethodNone = "none"
)

// Principal is an authenticated identity.
type Principal struct {
	// Subject is the name of the user (OIDC claim), of the token or of the operating system user.
	Subject string   `json:"subject"`
	Groups  []string `json:"groups,omitempty"`
	Method  string   `json:"method"`
}

// String returns the identifier "<method>:<subject>" (e.g. "oidc:alice", "token:ci"),
// used in RBAC rules, in the audit and as the DBOS workflow user.
func (p *Principal) String() string {
	if p == nil {
		return ""
	}
	return p.Method + ":" + p.Subject
}

// Anonymous is the identity used when authentication is disabled.
var Anonymous = &Principal{Subject: "anonymous", Method: MethodNone}

type ctxKey struct{}

// WithPrincipal returns a context with the given identity.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext returns the identity of the context, or nil if absent.
func FromContext(ctx context.Context) *Principal {
	p, _ := ctx.Value(ctxKey{}).(*Principal)
	return p
}

// CLIPrincipal returns the identity of the operating system user running the CLI.
func CLIPrincipal() *Principal {
	name := "unknown"
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	return &Principal{Subject: name, Method: MethodCLI}
}

// ErrUnauthenticated means missing or invalid credentials.
var ErrUnauthenticated = errors.New("authentication required")

// Authenticator verifies the bearer token of a request.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (*Principal, error)
}

// BearerToken extracts the token from the Authorization header ("Bearer <token>").
func BearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	scheme, token, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
		return "", false
	}
	return strings.TrimSpace(token), true
}

// StaticToken is a token for automated systems (e.g. CI pipelines). The configuration holds
// only the token's sha256, never its value.
type StaticToken struct {
	Name   string
	SHA256 string
	Groups []string
	// ExpiresAt, if not zero, is the token expiry.
	ExpiresAt time.Time
}

// Tokens authenticates static tokens by comparing their sha256.
type Tokens []StaticToken

// Authenticate returns the matching token, or ErrUnauthenticated.
// The comparison is constant-time and always scans all tokens.
// An expired token is not valid.
func (ts Tokens) Authenticate(_ context.Context, token string) (*Principal, error) {
	sum := sha256.Sum256([]byte(token))
	got := []byte(hex.EncodeToString(sum[:]))
	var found *StaticToken
	for i := range ts {
		if subtle.ConstantTimeCompare(got, []byte(strings.ToLower(ts[i].SHA256))) == 1 {
			found = &ts[i]
		}
	}
	if found == nil || (!found.ExpiresAt.IsZero() && !time.Now().Before(found.ExpiresAt)) {
		return nil, ErrUnauthenticated
	}
	return &Principal{Subject: found.Name, Groups: slices.Clone(found.Groups), Method: MethodToken}, nil
}

// Chain tries the authenticators in order and returns the first valid identity.
type Chain []Authenticator

// Authenticate implements Authenticator.
func (c Chain) Authenticate(ctx context.Context, token string) (*Principal, error) {
	var errs []error
	for _, a := range c {
		p, err := a.Authenticate(ctx, token)
		if err == nil {
			return p, nil
		}
		if !errors.Is(err, ErrUnauthenticated) {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("%w: %w", ErrUnauthenticated, errors.Join(errs...))
	}
	return nil, ErrUnauthenticated
}
