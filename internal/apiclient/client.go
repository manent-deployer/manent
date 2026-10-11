// Package apiclient implementa service.Service sopra l'API HTTP di manent. La CLI lo usa con
// --server: le richieste passano da autenticazione, permessi e audit del server, senza
// credenziali del database.
package apiclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/governance"
	"github.com/joeg-ita/manent/internal/pipeline"
	"github.com/joeg-ita/manent/internal/service"
)

// Client chiama l'API di un server manent.
type Client struct {
	// BaseURL è l'indirizzo del server, es. https://manent.example.com.
	BaseURL string
	// Token è il bearer token (OIDC o statico); vuoto con auth.mode none.
	Token string
	HTTP  *http.Client
	// PollInterval è l'intervallo con cui Wait interroga lo stato (default 2s).
	PollInterval time.Duration
}

var _ service.Service = (*Client)(nil)

// Options configura il client.
type Options struct {
	// Server è l'URL del server, es. https://manent.example.com.
	Server string
	// Token è il bearer token (OIDC o statico); vuoto con auth.mode none.
	Token string
	// CAFile, se indicato, è il certificato della CA che ha firmato quello del server.
	CAFile string
	// Timeout di ogni richiesta (default 60s).
	Timeout time.Duration
}

// New crea un client per il server indicato.
func New(baseURL, token string) (*Client, error) {
	return NewWithOptions(Options{Server: baseURL, Token: token})
}

// NewWithOptions crea un client con le opzioni indicate.
func NewWithOptions(o Options) (*Client, error) {
	u, err := url.Parse(o.Server)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("server %q non valido: indicare un URL http(s)", o.Server)
	}
	if o.Timeout <= 0 {
		o.Timeout = 60 * time.Second
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if o.CAFile != "" {
		pem, err := os.ReadFile(o.CAFile)
		if err != nil {
			return nil, fmt.Errorf("lettura della CA del server: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("CA del server %s: nessun certificato PEM", o.CAFile)
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &Client{
		BaseURL: strings.TrimRight(o.Server, "/"), Token: o.Token,
		HTTP: &http.Client{Timeout: o.Timeout, Transport: transport},
	}, nil
}

// do esegue la richiesta e decodifica la risposta in out (se non nil). Gli errori HTTP sono
// tradotti negli errori dell'engine, così la CLI si comporta come con il database.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	u := c.BaseURL + "/api/v1" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		msg := strings.TrimSpace(e.Error)
		if msg == "" {
			msg = resp.Status
		}
		return statusError(resp.StatusCode, msg)
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

// errAuth indica un token mancante o non valido.
var errAuth = errors.New("autenticazione richiesta: indicare un token valido (MANENT_TOKEN o --token-file)")

// statusError riporta il messaggio del server con l'errore dell'engine corrispondente.
// Il messaggio del server contiene già il prefisso dell'errore, che non viene ripetuto.
func statusError(code int, msg string) error {
	var base error
	switch code {
	case http.StatusUnauthorized:
		return errAuth
	case http.StatusForbidden:
		base = service.ErrForbidden
	case http.StatusNotFound:
		base = service.ErrNotFound
	case http.StatusUnprocessableEntity:
		base = service.ErrInvalidRequest
	default:
		return fmt.Errorf("server: %s", msg)
	}
	return wrapped{msg: msg, base: base}
}

// wrapped è un errore con il messaggio del server e l'errore dell'engine come causa.
type wrapped struct {
	msg  string
	base error
}

func (w wrapped) Error() string { return w.msg }
func (w wrapped) Unwrap() error { return w.base }

// Plan implementa service.Service.
func (c *Client) Plan(ctx context.Context, d *descriptor.Descriptor) (pipeline.Plan, error) {
	var out struct {
		Plan pipeline.Plan `json:"plan"`
	}
	err := c.do(ctx, http.MethodPost, "/deployments/plan", nil, d, &out)
	return out.Plan, err
}

// Submit implementa service.Service.
func (c *Client) Submit(ctx context.Context, d *descriptor.Descriptor) (*service.Submission, error) {
	var sub service.Submission
	if err := c.do(ctx, http.MethodPost, "/deployments", nil, d, &sub); err != nil {
		return nil, err
	}
	return &sub, nil
}

// Status implementa service.Service.
func (c *Client) Status(ctx context.Context, id string) (*service.Status, error) {
	var st service.Status
	if err := c.do(ctx, http.MethodGet, "/deployments/"+url.PathEscape(id), nil, nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// List implementa service.Service. f.Match non è applicabile a distanza e viene ignorato.
func (c *Client) List(ctx context.Context, f service.ListFilter) ([]service.Status, error) {
	q := url.Values{}
	for k, v := range map[string]string{"schedule": f.Schedule, "application": f.Application, "environment": f.Environment, "state": f.State} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	var out struct {
		Deployments []service.Status `json:"deployments"`
	}
	err := c.do(ctx, http.MethodGet, "/deployments", q, nil, &out)
	return out.Deployments, err
}

// Wait interroga lo stato fino alla conclusione del deploy o alla scadenza di ctx.
func (c *Client) Wait(ctx context.Context, id string) (*service.Result, error) {
	interval := c.PollInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	for {
		st, err := c.Status(ctx, id)
		if err != nil {
			return nil, err
		}
		if service.Terminal(st.State) {
			if st.State == service.StateSuccess {
				return st.Result, nil
			}
			msg := st.State
			if st.Error != "" {
				msg += ": " + st.Error
			}
			return nil, errors.New(msg)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// Cancel implementa service.Service.
func (c *Client) Cancel(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/deployments/"+url.PathEscape(id)+"/cancel", nil, nil, nil)
}

// ListSchedules implementa service.Service.
func (c *Client) ListSchedules(ctx context.Context) ([]service.ScheduleInfo, error) {
	var out struct {
		Schedules []service.ScheduleInfo `json:"schedules"`
	}
	err := c.do(ctx, http.MethodGet, "/schedules", nil, nil, &out)
	return out.Schedules, err
}

// GetSchedule implementa service.Service.
func (c *Client) GetSchedule(ctx context.Context, name string) (*service.ScheduleInfo, error) {
	var s service.ScheduleInfo
	if err := c.do(ctx, http.MethodGet, "/schedules/"+url.PathEscape(name), nil, nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// PauseSchedule implementa service.Service.
func (c *Client) PauseSchedule(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodPost, "/schedules/"+url.PathEscape(name)+"/pause", nil, nil, nil)
}

// ResumeSchedule implementa service.Service.
func (c *Client) ResumeSchedule(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodPost, "/schedules/"+url.PathEscape(name)+"/resume", nil, nil, nil)
}

// DeleteSchedule implementa service.Service.
func (c *Client) DeleteSchedule(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/schedules/"+url.PathEscape(name), nil, nil, nil)
}

// Inventory implementa service.Service.
func (c *Client) Inventory(ctx context.Context, f service.InventoryFilter) ([]service.InventoryEntry, error) {
	q := url.Values{}
	if f.Application != "" {
		q.Set("application", f.Application)
	}
	if f.Environment != "" {
		q.Set("environment", f.Environment)
	}
	var out struct {
		Inventory []service.InventoryEntry `json:"inventory"`
	}
	err := c.do(ctx, http.MethodGet, "/inventory", q, nil, &out)
	return out.Inventory, err
}

// Promote implementa service.Service.
func (c *Client) Promote(ctx context.Context, r service.PromoteRequest) (*service.Submission, error) {
	var sub service.Submission
	if err := c.do(ctx, http.MethodPost, "/promotions", nil, r, &sub); err != nil {
		return nil, err
	}
	return &sub, nil
}

// Rollback implementa service.Service.
func (c *Client) Rollback(ctx context.Context, r service.RollbackRequest) (*service.Submission, error) {
	var sub service.Submission
	if err := c.do(ctx, http.MethodPost, "/rollbacks", nil, r, &sub); err != nil {
		return nil, err
	}
	return &sub, nil
}

// Decide implementa service.Service.
func (c *Client) Decide(ctx context.Context, id string, r service.DecisionRequest) error {
	op := "/approve"
	if !r.Approve {
		op = "/reject"
	}
	return c.do(ctx, http.MethodPost, "/deployments/"+url.PathEscape(id)+op, nil, map[string]string{"comment": r.Comment}, nil)
}

// Freezes implementa service.Service.
func (c *Client) Freezes(ctx context.Context, application, environment string) ([]governance.WindowStatus, error) {
	q := url.Values{}
	if application != "" {
		q.Set("application", application)
	}
	if environment != "" {
		q.Set("environment", environment)
	}
	var out struct {
		Freezes []governance.WindowStatus `json:"freezes"`
	}
	err := c.do(ctx, http.MethodGet, "/freezes", q, nil, &out)
	return out.Freezes, err
}

// Steps implementa service.Service.
func (c *Client) Steps(ctx context.Context, id string) ([]service.StepRecord, error) {
	var out struct {
		Steps []service.StepRecord `json:"steps"`
	}
	err := c.do(ctx, http.MethodGet, "/deployments/"+url.PathEscape(id)+"/steps", nil, nil, &out)
	return out.Steps, err
}

// Logs implementa service.Service.
func (c *Client) Logs(ctx context.Context, id string, offset int) (*service.LogPage, error) {
	var p service.LogPage
	q := url.Values{"offset": {strconv.Itoa(offset)}}
	if err := c.do(ctx, http.MethodGet, "/deployments/"+url.PathEscape(id)+"/logs", q, nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// DORA implementa service.Service.
func (c *Client) DORA(ctx context.Context, f service.DORAFilter) (*service.DORAReport, error) {
	q := url.Values{}
	if f.Application != "" {
		q.Set("application", f.Application)
	}
	if f.Environment != "" {
		q.Set("environment", f.Environment)
	}
	if !f.Since.IsZero() {
		q.Set("since", f.Since.Format(time.RFC3339))
	}
	var rep service.DORAReport
	if err := c.do(ctx, http.MethodGet, "/metrics/dora", q, nil, &rep); err != nil {
		return nil, err
	}
	return &rep, nil
}

// Validate implementa service.Service.
func (c *Client) Validate(ctx context.Context, d *descriptor.Descriptor) (*service.Validation, error) {
	var v service.Validation
	if err := c.do(ctx, http.MethodPost, "/deployments/validate", nil, d, &v); err != nil {
		return nil, err
	}
	return &v, nil
}
