package auth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// OIDCConfig configures the verification of JWTs issued by an OIDC identity provider
// (Keycloak, Entra ID, Okta, ...).
type OIDCConfig struct {
	// Issuer is the issuer URL: discovery and signing keys (JWKS) are read from here.
	Issuer string
	// Audience is the value expected in the aud claim (typically manent's client ID).
	Audience string
	// UsernameClaim is the claim with the user name (default preferred_username, then sub).
	UsernameClaim string
	// GroupsClaim is the claim with the groups (default groups).
	GroupsClaim string
	// CAFile is a bundle of extra CAs to reach the issuer.
	CAFile string
}

// OIDC verifies signature, issuer, audience and expiry of bearer tokens.
type OIDC struct {
	verifier      *oidc.IDTokenVerifier
	usernameClaim string
	groupsClaim   string
}

// NewOIDC runs the issuer discovery. Signing keys are reloaded automatically when they
// change (rotation).
func NewOIDC(ctx context.Context, cfg OIDCConfig) (*OIDC, error) {
	if cfg.Issuer == "" || cfg.Audience == "" {
		return nil, errors.New("oidc: issuer e audience obbligatori")
	}
	client := &http.Client{Timeout: 10 * time.Second}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("oidc: lettura ca_file: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("oidc: ca_file non contiene certificati PEM")
		}
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	}
	// The client stays in the context after discovery too: go-oidc uses it to reload the keys.
	ctx = oidc.ClientContext(context.WithoutCancel(ctx), client)
	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc: discovery di %s: %w", cfg.Issuer, err)
	}
	o := &OIDC{
		verifier:      provider.Verifier(&oidc.Config{ClientID: cfg.Audience}),
		usernameClaim: cfg.UsernameClaim,
		groupsClaim:   cfg.GroupsClaim,
	}
	if o.usernameClaim == "" {
		o.usernameClaim = "preferred_username"
	}
	if o.groupsClaim == "" {
		o.groupsClaim = "groups"
	}
	return o, nil
}

// Authenticate verifies the token and extracts user and groups. A value that is not a JWT
// (e.g. an unknown static token) yields ErrUnauthenticated.
func (o *OIDC) Authenticate(ctx context.Context, token string) (*Principal, error) {
	if strings.Count(token, ".") != 2 {
		return nil, ErrUnauthenticated
	}
	tok, err := o.verifier.Verify(ctx, token)
	if err != nil {
		return nil, fmt.Errorf("token OIDC non valido: %w", err)
	}
	var claims map[string]any
	if err := tok.Claims(&claims); err != nil {
		return nil, fmt.Errorf("claim OIDC non leggibili: %w", err)
	}
	p := &Principal{Subject: tok.Subject, Method: MethodOIDC}
	if v, ok := claims[o.usernameClaim].(string); ok && v != "" {
		p.Subject = v
	}
	switch g := claims[o.groupsClaim].(type) {
	case []any:
		for _, v := range g {
			if s, ok := v.(string); ok {
				p.Groups = append(p.Groups, s)
			}
		}
	case string:
		p.Groups = []string{g}
	}
	return p, nil
}
