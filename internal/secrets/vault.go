package secrets

import (
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Metodi di autenticazione verso Vault.
const (
	VaultAuthToken      = "token"
	VaultAuthAppRole    = "approle"
	VaultAuthKubernetes = "kubernetes"
)

// VaultConfig configura il provider dello schema "vault:".
type VaultConfig struct {
	// Address di Vault (default VAULT_ADDR).
	Address string
	// Namespace di Vault Enterprise / HCP, opzionale.
	Namespace string
	// Auth è token (default), approle o kubernetes.
	Auth string
	// TokenFile contiene il token (auth token); in alternativa si usa VAULT_TOKEN.
	TokenFile string
	// AppRole: role_id e file con il secret_id; Mount default "approle".
	RoleID       string
	SecretIDFile string
	// Kubernetes: ruolo Vault e token del service account; Mount default "kubernetes".
	Role    string
	JWTFile string
	// Mount è il percorso del metodo di autenticazione (approle o kubernetes).
	Mount string
	// CAFile è un bundle di CA aggiuntive per raggiungere Vault.
	CAFile  string
	Timeout time.Duration
}

// DefaultJWTFile è il token del service account montato nei pod Kubernetes.
const DefaultJWTFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// Vault risolve i riferimenti "vault:<percorso>#<campo>" leggendo i segreti da HashiCorp Vault
// (KV v1 e v2, es. "vault:secret/data/myapp/prod#ssh_key"). I valori non vengono mai
// memorizzati; il token di accesso è tenuto in memoria fino alla scadenza.
type Vault struct {
	cfg    VaultConfig
	client *http.Client

	mu     sync.Mutex
	token  string
	expiry time.Time // zero = non scade o letto da file a ogni uso
}

// NewVault verifica la configurazione e prepara il client HTTP.
func NewVault(cfg VaultConfig) (*Vault, error) {
	cfg.Address = strings.TrimRight(cmp.Or(cfg.Address, os.Getenv("VAULT_ADDR")), "/")
	if cfg.Address == "" {
		return nil, errors.New("vault: address obbligatorio (o VAULT_ADDR)")
	}
	cfg.Auth = cmp.Or(cfg.Auth, VaultAuthToken)
	switch cfg.Auth {
	case VaultAuthToken:
	case VaultAuthAppRole:
		if cfg.RoleID == "" || cfg.SecretIDFile == "" {
			return nil, errors.New("vault: approle richiede role_id e secret_id_file")
		}
		cfg.Mount = cmp.Or(cfg.Mount, "approle")
	case VaultAuthKubernetes:
		if cfg.Role == "" {
			return nil, errors.New("vault: kubernetes richiede role")
		}
		cfg.JWTFile = cmp.Or(cfg.JWTFile, DefaultJWTFile)
		cfg.Mount = cmp.Or(cfg.Mount, "kubernetes")
	default:
		return nil, fmt.Errorf("vault: auth %q non supportato (token|approle|kubernetes)", cfg.Auth)
	}
	client := &http.Client{Timeout: cmp.Or(cfg.Timeout, 10*time.Second)}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("vault: lettura ca_file: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("vault: ca_file non contiene certificati PEM")
		}
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	}
	return &Vault{cfg: cfg, client: client}, nil
}

// Resolve legge il campo indicato dal segreto. Se Vault rifiuta il token (scaduto o revocato),
// rifà il login una volta.
func (v *Vault) Resolve(ctx context.Context, ref string) (string, error) {
	path, field, ok := strings.Cut(ref, "#")
	if !ok || path == "" || field == "" {
		return "", fmt.Errorf("riferimento vault %q non valido: atteso vault:<percorso>#<campo>", ref)
	}
	if strings.HasPrefix(path, "/") || strings.Contains("/"+path+"/", "/../") {
		return "", fmt.Errorf("riferimento vault %q non valido", ref)
	}
	val, err := v.read(ctx, path, field)
	if errors.Is(err, errVaultForbidden) && v.cfg.Auth != VaultAuthToken {
		v.invalidate()
		val, err = v.read(ctx, path, field)
	}
	return val, err
}

var errVaultForbidden = errors.New("accesso negato da vault")

func (v *Vault) read(ctx context.Context, path, field string) (string, error) {
	token, err := v.currentToken(ctx)
	if err != nil {
		return "", err
	}
	var body struct {
		Data map[string]any `json:"data"`
	}
	if err := v.do(ctx, http.MethodGet, "/v1/"+path, token, nil, &body); err != nil {
		return "", fmt.Errorf("vault %s: %w", path, err)
	}
	data := body.Data
	// KV v2: il segreto è in data.data, accanto a data.metadata.
	if inner, ok := data["data"].(map[string]any); ok {
		if _, ok := data["metadata"]; ok {
			data = inner
		}
	}
	raw, ok := data[field]
	if !ok {
		return "", fmt.Errorf("vault %s: campo %q assente", path, field)
	}
	if s, ok := raw.(string); ok {
		return s, nil
	}
	return "", fmt.Errorf("vault %s: il campo %q non è una stringa", path, field)
}

// currentToken restituisce un token valido, facendo il login se necessario.
func (v *Vault) currentToken(ctx context.Context) (string, error) {
	if v.cfg.Auth == VaultAuthToken {
		if v.cfg.TokenFile != "" {
			// Riletto a ogni uso: un agente esterno (es. vault agent) può ruotarlo.
			b, err := os.ReadFile(v.cfg.TokenFile)
			if err != nil {
				return "", fmt.Errorf("vault: lettura token_file: %w", err)
			}
			return strings.TrimSpace(string(b)), nil
		}
		if t := os.Getenv("VAULT_TOKEN"); t != "" {
			return t, nil
		}
		return "", errors.New("vault: token assente (token_file o VAULT_TOKEN)")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.token != "" && (v.expiry.IsZero() || time.Now().Before(v.expiry)) {
		return v.token, nil
	}
	payload, err := v.loginPayload()
	if err != nil {
		return "", err
	}
	var resp struct {
		Auth struct {
			ClientToken   string `json:"client_token"`
			LeaseDuration int    `json:"lease_duration"`
		} `json:"auth"`
	}
	if err := v.do(ctx, http.MethodPost, "/v1/auth/"+v.cfg.Mount+"/login", "", payload, &resp); err != nil {
		return "", fmt.Errorf("vault: login %s: %w", v.cfg.Auth, err)
	}
	if resp.Auth.ClientToken == "" {
		return "", errors.New("vault: login senza token")
	}
	v.token = resp.Auth.ClientToken
	v.expiry = time.Time{}
	if d := resp.Auth.LeaseDuration; d > 0 {
		// Rinnovo anticipato: il token viene sostituito all'80% della durata.
		v.expiry = time.Now().Add(time.Duration(d) * time.Second * 8 / 10)
	}
	return v.token, nil
}

// loginPayload legge le credenziali di login dai file, a ogni login.
func (v *Vault) loginPayload() (map[string]string, error) {
	switch v.cfg.Auth {
	case VaultAuthAppRole:
		b, err := os.ReadFile(v.cfg.SecretIDFile)
		if err != nil {
			return nil, fmt.Errorf("vault: lettura secret_id_file: %w", err)
		}
		return map[string]string{"role_id": v.cfg.RoleID, "secret_id": strings.TrimSpace(string(b))}, nil
	default:
		b, err := os.ReadFile(v.cfg.JWTFile)
		if err != nil {
			return nil, fmt.Errorf("vault: lettura jwt_file: %w", err)
		}
		return map[string]string{"role": v.cfg.Role, "jwt": strings.TrimSpace(string(b))}, nil
	}
}

func (v *Vault) invalidate() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.token = ""
}

// do esegue una chiamata all'API di Vault. Gli errori riportano solo lo status e i messaggi
// di Vault, mai il contenuto dei segreti.
func (v *Vault) do(ctx context.Context, method, path, token string, payload, out any) error {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, v.cfg.Address+path, body)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	if v.cfg.Namespace != "" {
		req.Header.Set("X-Vault-Namespace", v.cfg.Namespace)
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Errors []string `json:"errors"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e)
		err := fmt.Errorf("status %d %s", resp.StatusCode, strings.Join(e.Errors, "; "))
		if resp.StatusCode == http.StatusForbidden {
			return fmt.Errorf("%w: %w", errVaultForbidden, err)
		}
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
