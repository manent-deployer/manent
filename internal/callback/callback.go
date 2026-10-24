// Package callback notifies the HTTP endpoints named in the descriptor of the outcome of
// steps and deploy.
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

// States reported in events.
const (
	StatusStarted   = "started"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	// StatusSkipped is a step skipped in dry-run.
	StatusSkipped = "skipped"
)

// Headers sent with every event.
const (
	HeaderEvent     = "X-Manent-Event"
	HeaderDelivery  = "X-Manent-Delivery"
	HeaderTimestamp = "X-Manent-Timestamp"
	HeaderSignature = "X-Manent-Signature"
)

// Event is the JSON body POSTed to callbacks.
type Event struct {
	// ID is "<deployment>:<sequence>": after a recovery an event can arrive twice and the
	// receiver recognizes it by this field.
	ID           string `json:"id"`
	Type         string `json:"event"`
	Sequence     int    `json:"sequence"`
	DeploymentID string `json:"deployment_id"`
	Application  string `json:"application"`
	Environment  string `json:"environment"`
	// Tenant is the tenant of the deploy, if any.
	Tenant string `json:"tenant,omitempty"`
	DryRun bool   `json:"dry_run,omitempty"`
	// RequestedBy is the identity that requested the deploy (e.g. "oidc:alice").
	RequestedBy string `json:"requested_by,omitempty"`
	// Step is set only for step events.
	Step   string            `json:"step,omitempty"`
	Status string            `json:"status"`
	Output map[string]string `json:"output,omitempty"`
	Error  string            `json:"error,omitempty"`
	// Rollback is the outcome of the automatic rollback (completed, skipped, failed), only in the
	// final event of a failed deploy whose policy provided for it.
	Rollback string `json:"rollback,omitempty"`
	// Timestamp is the time of sending.
	Timestamp time.Time `json:"timestamp"`
}

// Notifier sends an event to a callback of the descriptor.
type Notifier interface {
	Notify(ctx context.Context, d *descriptor.Descriptor, cb descriptor.Callback, ev Event) error
}

// Policy limits the URLs callbacks can reach: the descriptor comes from whoever requests the
// deploy, who must not be able to make the worker call arbitrary internal services.
type Policy struct {
	// AllowHTTP allows http URLs (default: https only).
	AllowHTTP bool
	// AllowedHosts lists the allowed hosts, also with a leading "*." for subdomains; empty = all.
	AllowedHosts []string
}

// Check verifies that the URL complies with the policy.
func (p Policy) Check(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid callback URL: %w", err)
	}
	if u.Scheme != "https" && !(p.AllowHTTP && u.Scheme == "http") {
		return fmt.Errorf("callback %s: scheme %q not allowed", u.Host, u.Scheme)
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
	return fmt.Errorf("callback: host %q not allowed by the configuration", host)
}

// HTTPDoer abstracts the HTTP client to ease testing.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// NewHTTPClient creates the callback client. It does not follow redirects: they would bypass
// the Policy and carry the credentials to another host.
func NewHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// HTTPNotifier POSTs events with a JSON body.
type HTTPNotifier struct {
	Client  HTTPDoer
	Secrets secrets.Provider
	Policy  Policy
	Logger  *slog.Logger
}

// Notify sends the event; a non-2xx response is an error, so DBOS retries the sending.
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

	// Only host and path in the logs: the query string may contain tokens.
	target := req.URL.Host + req.URL.Path
	log := n.Logger.With("deployment", ev.DeploymentID, "callback", target, "event", ev.Type, "delivery", ev.ID)
	resp, err := n.Client.Do(req)
	if err != nil {
		log.WarnContext(ctx, "callback send failed", "error", err)
		return fmt.Errorf("callback %s: %w", target, err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		log.WarnContext(ctx, "callback refused", "status", resp.StatusCode)
		return fmt.Errorf("callback %s: status %d", target, resp.StatusCode)
	}
	log.InfoContext(ctx, "callback sent", "status", ev.Status, "step", ev.Step)
	return nil
}

// authorize adds authentication, reading the secret only at sending time.
func (n HTTPNotifier) authorize(ctx context.Context, d *descriptor.Descriptor, a *descriptor.CallbackAuth, req *http.Request, body []byte, ts time.Time) error {
	if a == nil {
		return nil
	}
	secret, err := secrets.DescriptorResolver{Provider: n.Secrets, Descriptor: d}.Secret(ctx, a.Secret)
	if err != nil {
		return fmt.Errorf("callback authentication: %w", err)
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
		return errors.New("unsupported callback authentication type: " + a.Type)
	}
	return nil
}

// Sign computes the HMAC-SHA256 signature (hex) of "<timestamp>.<body>".
// The signed timestamp lets the receiver reject replayed requests.
func Sign(secret, timestamp string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(timestamp))
	m.Write([]byte("."))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}
