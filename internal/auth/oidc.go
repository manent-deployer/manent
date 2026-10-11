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

// OIDCConfig configura la verifica dei token JWT emessi da un identity provider OIDC
// (Keycloak, Entra ID, Okta, ...).
type OIDCConfig struct {
	// Issuer è l'URL dell'issuer: da qui si leggono discovery e chiavi di firma (JWKS).
	Issuer string
	// Audience è il valore atteso nel claim aud (tipicamente il client ID di manent).
	Audience string
	// UsernameClaim è il claim con il nome dell'utente (default preferred_username, poi sub).
	UsernameClaim string
	// GroupsClaim è il claim con i gruppi (default groups).
	GroupsClaim string
	// CAFile è un bundle di CA aggiuntive per raggiungere l'issuer.
	CAFile string
}

// OIDC verifica firma, issuer, audience e scadenza dei token bearer.
type OIDC struct {
	verifier      *oidc.IDTokenVerifier
	usernameClaim string
	groupsClaim   string
}

// NewOIDC esegue la discovery dell'issuer. Le chiavi di firma vengono rilette automaticamente
// quando cambiano (rotazione).
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
	// Il client va nel context anche dopo la discovery: go-oidc lo usa per rileggere le chiavi.
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

// Authenticate verifica il token e ne estrae utente e gruppi. Un valore che non è un JWT
// (es. un token statico non riconosciuto) vale ErrUnauthenticated.
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
