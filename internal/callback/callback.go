// Package callback notifica agli endpoint HTTP indicati nel descrittore l'esito degli step e del deploy.
package callback

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/secrets"
)

// Stati riportati negli eventi.
const (
	StatusStarted   = "started"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	// StatusSkipped indica uno step saltato in dry-run.
	StatusSkipped = "skipped"
)

// Header inviati con ogni evento.
const (
	HeaderEvent     = "X-Manent-Event"
	HeaderDelivery  = "X-Manent-Delivery"
	HeaderTimestamp = "X-Manent-Timestamp"
	HeaderSignature = "X-Manent-Signature"
)

// Event è il corpo JSON inviato in POST ai callback.
type Event struct {
	// ID è "<deployment>:<sequence>": in caso di recovery un evento può arrivare due volte
	// e il destinatario lo riconosce da questo campo.
	ID           string `json:"id"`
	Type         string `json:"event"`
	Sequence     int    `json:"sequence"`
	DeploymentID string `json:"deployment_id"`
	Application  string `json:"application"`
	Environment  string `json:"environment"`
	DryRun       bool   `json:"dry_run,omitempty"`
	// RequestedBy è l'identità che ha richiesto il deploy (es. "oidc:alice").
	RequestedBy string `json:"requested_by,omitempty"`
	// Step è valorizzato solo per gli eventi di tipo step.
	Step   string            `json:"step,omitempty"`
	Status string            `json:"status"`
	Output map[string]string `json:"output,omitempty"`
	Error  string            `json:"error,omitempty"`
	// Rollback è l'esito del rollback automatico (completed, skipped, failed), solo nell'evento
	// finale di un deploy fallito per cui la policy lo prevedeva.
	Rollback string `json:"rollback,omitempty"`
	// Timestamp è l'orario dell'invio.
	Timestamp time.Time `json:"timestamp"`
}

// Notifier invia un evento a un callback del descrittore.
type Notifier interface {
	Notify(ctx context.Context, d *descriptor.Descriptor, cb descriptor.Callback, ev Event) error
}

// Policy limita gli URL raggiungibili dai callback: il descrittore arriva da chi richiede il deploy
// e non deve poter far chiamare al worker servizi interni arbitrari.
type Policy struct {
	// AllowHTTP ammette URL http (default: solo https).
	AllowHTTP bool
	// AllowedHosts elenca gli host ammessi, anche con "*." iniziale per i sottodomini; vuoto = tutti.
	AllowedHosts []string
}

// Check verifica che l'URL rispetti la policy.
func (p Policy) Check(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("URL di callback non valido: %w", err)
	}
	if u.Scheme != "https" && !(p.AllowHTTP && u.Scheme == "http") {
		return fmt.Errorf("callback %s: schema %q non ammesso", u.Host, u.Scheme)
	}
	if len(p.AllowedHosts) == 0 {
		return nil
	}
	host := strings.ToLower(u.Hostname())
	for _, h := range p.AllowedHosts {
		h = strings.ToLower(h)
		if host == h || (strings.HasPrefix(h, "*.") && strings.HasSuffix(host, h[1:])) {
			return nil
		}
	}
	return fmt.Errorf("callback: host %q non ammesso dalla configurazione", host)
}

// HTTPDoer astrae il client HTTP per facilitare i test.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// NewHTTPClient crea il client per i callback. Non segue i redirect: aggirerebbero la Policy
// e porterebbero le credenziali verso un altro host.
func NewHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// HTTPNotifier invia gli eventi in POST con corpo JSON.
type HTTPNotifier struct {
	Client  HTTPDoer
	Secrets secrets.Provider
	Policy  Policy
	Logger  *slog.Logger
}

// Notify invia l'evento; una risposta diversa da 2xx è un errore, così DBOS ritenta l'invio.
func (n HTTPNotifier) Notify(ctx context.Context, d *descriptor.Descriptor, cb descriptor.Callback, ev Event) error {
	if err := n.Policy.Check(cb.URL); err != nil {
		return err
	}
	body, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cb.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "manent")
	req.Header.Set(HeaderEvent, ev.Type)
	req.Header.Set(HeaderDelivery, ev.ID)
	if err := n.authorize(ctx, d, cb.Auth, req, body, ev.Timestamp); err != nil {
		return err
	}

	// Nei log solo host e percorso: la query string potrebbe contenere token.
	target := req.URL.Host + req.URL.Path
	log := n.Logger.With("deployment", ev.DeploymentID, "callback", target, "event", ev.Type, "delivery", ev.ID)
	resp, err := n.Client.Do(req)
	if err != nil {
		log.WarnContext(ctx, "invio callback fallito", "error", err)
		return fmt.Errorf("callback %s: %w", target, err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		log.WarnContext(ctx, "callback rifiutato", "status", resp.StatusCode)
		return fmt.Errorf("callback %s: status %d", target, resp.StatusCode)
	}
	log.InfoContext(ctx, "callback inviato", "status", ev.Status, "step", ev.Step)
	return nil
}

// authorize aggiunge l'autenticazione leggendo il segreto solo al momento dell'invio.
func (n HTTPNotifier) authorize(ctx context.Context, d *descriptor.Descriptor, a *descriptor.CallbackAuth, req *http.Request, body []byte, ts time.Time) error {
	if a == nil {
		return nil
	}
	secret, err := secrets.DescriptorResolver{Provider: n.Secrets, Descriptor: d}.Secret(ctx, a.Secret)
	if err != nil {
		return fmt.Errorf("autenticazione callback: %w", err)
	}
	secret = strings.TrimSpace(secret)
	switch a.Type {
	case descriptor.CallbackAuthBearer:
		req.Header.Set("Authorization", "Bearer "+secret)
	case descriptor.CallbackAuthBasic:
		req.SetBasicAuth(a.Username, secret)
	case descriptor.CallbackAuthHeader:
		req.Header.Set(a.Header, secret)
	case descriptor.CallbackAuthHMAC:
		unix := strconv.FormatInt(ts.Unix(), 10)
		req.Header.Set(HeaderTimestamp, unix)
		req.Header.Set(HeaderSignature, "sha256="+Sign(secret, unix, body))
	default:
		return errors.New("tipo di autenticazione callback non supportato: " + a.Type)
	}
	return nil
}

// Sign calcola la firma HMAC-SHA256 (esadecimale) di "<timestamp>.<body>".
// Il timestamp firmato permette al destinatario di rifiutare le richieste ripetute.
func Sign(secret, timestamp string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(timestamp))
	m.Write([]byte("."))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}
