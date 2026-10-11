// Package auth identifica chi usa manent: utenti OIDC e token statici sull'API, utente del
// sistema operativo sulla CLI. L'identità viaggia nel context fino al servizio, che applica
// i permessi (internal/authz) e la registra nel deploy e nell'audit.
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

// Metodi con cui un'identità è stata autenticata.
const (
	MethodOIDC  = "oidc"
	MethodToken = "token"
	MethodCLI   = "cli"
	// MethodNone indica l'autenticazione disattivata (auth.mode: none).
	MethodNone = "none"
)

// Principal è un'identità autenticata.
type Principal struct {
	// Subject è il nome dell'utente (claim OIDC), del token o dell'utente del sistema operativo.
	Subject string   `json:"subject"`
	Groups  []string `json:"groups,omitempty"`
	Method  string   `json:"method"`
}

// String restituisce l'identificativo "<metodo>:<subject>" (es. "oidc:alice", "token:ci"),
// usato nelle regole RBAC, nell'audit e come utente del workflow DBOS.
func (p *Principal) String() string {
	if p == nil {
		return ""
	}
	return p.Method + ":" + p.Subject
}

// Anonymous è l'identità usata quando l'autenticazione è disattivata.
var Anonymous = &Principal{Subject: "anonymous", Method: MethodNone}

type ctxKey struct{}

// WithPrincipal restituisce un context con l'identità indicata.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext restituisce l'identità del context, o nil se assente.
func FromContext(ctx context.Context) *Principal {
	p, _ := ctx.Value(ctxKey{}).(*Principal)
	return p
}

// CLIPrincipal restituisce l'identità dell'utente del sistema operativo che esegue la CLI.
func CLIPrincipal() *Principal {
	name := "unknown"
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	return &Principal{Subject: name, Method: MethodCLI}
}

// ErrUnauthenticated indica credenziali mancanti o non valide.
var ErrUnauthenticated = errors.New("autenticazione richiesta")

// Authenticator verifica il token bearer di una richiesta.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (*Principal, error)
}

// BearerToken estrae il token dall'header Authorization ("Bearer <token>").
func BearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	scheme, token, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
		return "", false
	}
	return strings.TrimSpace(token), true
}

// StaticToken è un token per sistemi automatici (es. pipeline CI). In configurazione c'è solo
// lo sha256 del token, mai il valore.
type StaticToken struct {
	Name   string
	SHA256 string
	Groups []string
	// ExpiresAt, se non zero, è la scadenza del token.
	ExpiresAt time.Time
}

// Tokens autentica i token statici confrontandone lo sha256.
type Tokens []StaticToken

// Authenticate restituisce il token corrispondente, o ErrUnauthenticated.
// Il confronto è a tempo costante e scorre sempre tutti i token.
// Un token scaduto non è valido.
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

// Chain prova gli authenticator in ordine e restituisce la prima identità valida.
type Chain []Authenticator

// Authenticate implementa Authenticator.
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
