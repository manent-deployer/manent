// Package apiclient implements service.Service on top of the manent HTTP API. The CLI uses it
// with --server: requests go through the server's authentication, permissions and audit,
// without database credentials.
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

// Client calls the API of a manent server.
type Client struct {
	// BaseURL is the server address, e.g. https://manent.example.com.
	BaseURL string
	// Token is the bearer token (OIDC or static); empty with auth.mode none.
	Token string
	HTTP  *http.Client
	// PollInterval is how often Wait polls the status (default 2s).
	PollInterval time.Duration
}

var _ service.Service = (*Client)(nil)

// Options configures the client.
type Options struct {
	// Server is the server URL, e.g. https://manent.example.com.
	Server string
	// Token is the bearer token (OIDC or static); empty with auth.mode none.
	Token string
	// CAFile, if set, is the certificate of the CA that signed the server's.
	CAFile string
	// Timeout of every request (default 60s).
	Timeout time.Duration
}

// New creates a client for the given server.
func New(baseURL, token string) (*Client, error) {
	return NewWithOptions(Options{Server: baseURL, Token: token})
}

// NewWithOptions creates a client with the given options.
func NewWithOptions(o Options) (*Client, error) {
	u, err := url.Parse(o.Server)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid server %q: provide an http(s) URL", o.Server)
	}
	if o.Timeout <= 0 {
		o.Timeout = 60 * time.Second
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if o.CAFile != "" {
		pem, err := os.ReadFile(o.CAFile)
		if err != nil {
			return nil, fmt.Errorf("reading the server CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("server CA %s: no PEM certificate", o.CAFile)
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &Client{
		BaseURL: strings.TrimRight(o.Server, "/"), Token: o.Token,
		HTTP: &http.Client{Timeout: o.Timeout, Transport: transport},
	}, nil
}

// do runs the request and decodes the response into out (if not nil). HTTP errors are mapped
// to engine errors, so the CLI behaves as it does on the database.
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

// errAuth means a missing or invalid token.
var errAuth = errors.New("authentication required: provide a valid token (MANENT_TOKEN or --token-file)")

// statusError carries the server message with the matching engine error.
// The server message already contains the error prefix, which is not repeated.
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
	case http.StatusServiceUnavailable:
		base = service.ErrUnavailable
	default:
		return fmt.Errorf("server: %s", msg)
	}
	return wrapped{msg: msg, base: base}
}

// wrapped is an error with the server message and the engine error as cause.
type wrapped struct {
	msg  string
	base error
}

func (w wrapped) Error() string { return w.msg }
func (w wrapped) Unwrap() error { return w.base }

// Plan implements service.Service.
func (c *Client) Plan(ctx context.Context, d *descriptor.Descriptor) (pipeline.Plan, error) {
	var out struct {
		Plan pipeline.Plan `json:"plan"`
	}
	err := c.do(ctx, http.MethodPost, "/deployments/plan", nil, d, &out)
	return out.Plan, err
}

// Submit implements service.Service.
func (c *Client) Submit(ctx context.Context, d *descriptor.Descriptor) (*service.Submission, error) {
	var sub service.Submission
	if err := c.do(ctx, http.MethodPost, "/deployments", nil, d, &sub); err != nil {
		return nil, err
	}
	return &sub, nil
}

// Status implements service.Service.
func (c *Client) Status(ctx context.Context, id string) (*service.Status, error) {
	var st service.Status
	if err := c.do(ctx, http.MethodGet, "/deployments/"+url.PathEscape(id), nil, nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// List implements service.Service. f.Match cannot be applied remotely and is ignored.
func (c *Client) List(ctx context.Context, f service.ListFilter) ([]service.Status, error) {
	q := url.Values{}
	for k, v := range map[string]string{"schedule": f.Schedule, "application": f.Application, "environment": f.Environment, "tenant": f.Tenant, "state": f.State, "cursor": f.Cursor} {
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

// Wait polls the status until the deploy ends or ctx expires.
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

// Cancel implements service.Service.
func (c *Client) Cancel(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/deployments/"+url.PathEscape(id)+"/cancel", nil, nil, nil)
}

// ListSchedules implements service.Service.
func (c *Client) ListSchedules(ctx context.Context) ([]service.ScheduleInfo, error) {
	var out struct {
		Schedules []service.ScheduleInfo `json:"schedules"`
	}
	err := c.do(ctx, http.MethodGet, "/schedules", nil, nil, &out)
	return out.Schedules, err
}

// GetSchedule implements service.Service.
func (c *Client) GetSchedule(ctx context.Context, name string) (*service.ScheduleInfo, error) {
	var s service.ScheduleInfo
	if err := c.do(ctx, http.MethodGet, "/schedules/"+url.PathEscape(name), nil, nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// PauseSchedule implements service.Service.
func (c *Client) PauseSchedule(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodPost, "/schedules/"+url.PathEscape(name)+"/pause", nil, nil, nil)
}

// ResumeSchedule implements service.Service.
func (c *Client) ResumeSchedule(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodPost, "/schedules/"+url.PathEscape(name)+"/resume", nil, nil, nil)
}

// DeleteSchedule implements service.Service.
func (c *Client) DeleteSchedule(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/schedules/"+url.PathEscape(name), nil, nil, nil)
}

// Inventory implements service.Service.
func (c *Client) Inventory(ctx context.Context, f service.InventoryFilter) ([]service.InventoryEntry, error) {
	q := url.Values{}
	if f.Application != "" {
		q.Set("application", f.Application)
	}
	if f.Environment != "" {
		q.Set("environment", f.Environment)
	}
	if f.Tenant != "" {
		q.Set("tenant", f.Tenant)
	}
	var out struct {
		Inventory []service.InventoryEntry `json:"inventory"`
	}
	err := c.do(ctx, http.MethodGet, "/inventory", q, nil, &out)
	return out.Inventory, err
}

// Promote implements service.Service.
func (c *Client) Promote(ctx context.Context, r service.PromoteRequest) (*service.Submission, error) {
	var sub service.Submission
	if err := c.do(ctx, http.MethodPost, "/promotions", nil, r, &sub); err != nil {
		return nil, err
	}
	return &sub, nil
}

// Rollback implements service.Service.
func (c *Client) Rollback(ctx context.Context, r service.RollbackRequest) (*service.Submission, error) {
	var sub service.Submission
	if err := c.do(ctx, http.MethodPost, "/rollbacks", nil, r, &sub); err != nil {
		return nil, err
	}
	return &sub, nil
}

// Decide implements service.Service.
func (c *Client) Decide(ctx context.Context, id string, r service.DecisionRequest) error {
	op := "/approve"
	if !r.Approve {
		op = "/reject"
	}
	return c.do(ctx, http.MethodPost, "/deployments/"+url.PathEscape(id)+op, nil, map[string]string{"comment": r.Comment}, nil)
}

// Freezes implements service.Service.
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

// Approvals implements service.Service.
func (c *Client) Approvals(ctx context.Context) ([]service.Status, error) {
	var out struct {
		Approvals []service.Status `json:"approvals"`
	}
	err := c.do(ctx, http.MethodGet, "/approvals", nil, nil, &out)
	return out.Approvals, err
}

// SubmitRelease implements service.Service.
func (c *Client) SubmitRelease(ctx context.Context, r *descriptor.Release) (*service.ReleaseStatus, error) {
	var rs service.ReleaseStatus
	if err := c.do(ctx, http.MethodPost, "/releases", nil, r, &rs); err != nil {
		return nil, err
	}
	return &rs, nil
}

// ReleaseStatus implements service.Service.
func (c *Client) ReleaseStatus(ctx context.Context, id string) (*service.ReleaseStatus, error) {
	var rs service.ReleaseStatus
	if err := c.do(ctx, http.MethodGet, "/releases/"+url.PathEscape(id), nil, nil, &rs); err != nil {
		return nil, err
	}
	return &rs, nil
}

// ListReleases implements service.Service.
func (c *Client) ListReleases(ctx context.Context, limit int) ([]service.ReleaseStatus, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out struct {
		Releases []service.ReleaseStatus `json:"releases"`
	}
	err := c.do(ctx, http.MethodGet, "/releases", q, nil, &out)
	return out.Releases, err
}

// CancelRelease implements service.Service.
func (c *Client) CancelRelease(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/releases/"+url.PathEscape(id)+"/cancel", nil, nil, nil)
}

// Workers implements service.Service.
func (c *Client) Workers(ctx context.Context) ([]service.WorkerInfo, error) {
	var out struct {
		Workers []service.WorkerInfo `json:"workers"`
	}
	err := c.do(ctx, http.MethodGet, "/workers", nil, nil, &out)
	return out.Workers, err
}

// Activity implements service.Service.
func (c *Client) Activity(ctx context.Context, f service.ActivityFilter) (*service.Activity, error) {
	q := url.Values{}
	if f.Application != "" {
		q.Set("application", f.Application)
	}
	if f.Environment != "" {
		q.Set("environment", f.Environment)
	}
	if f.Tenant != "" {
		q.Set("tenant", f.Tenant)
	}
	var a service.Activity
	if err := c.do(ctx, http.MethodGet, "/activity", q, nil, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// Me implements service.Service.
func (c *Client) Me(ctx context.Context) (*service.Identity, error) {
	var out service.Identity
	if err := c.do(ctx, http.MethodGet, "/me", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Steps implements service.Service.
func (c *Client) Steps(ctx context.Context, id string) ([]service.StepRecord, error) {
	var out struct {
		Steps []service.StepRecord `json:"steps"`
	}
	err := c.do(ctx, http.MethodGet, "/deployments/"+url.PathEscape(id)+"/steps", nil, nil, &out)
	return out.Steps, err
}

// Logs implements service.Service.
func (c *Client) Logs(ctx context.Context, id string, offset int) (*service.LogPage, error) {
	var p service.LogPage
	q := url.Values{"offset": {strconv.Itoa(offset)}}
	if err := c.do(ctx, http.MethodGet, "/deployments/"+url.PathEscape(id)+"/logs", q, nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// DORA implements service.Service.
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
	if f.Tenant != "" {
		q.Set("tenant", f.Tenant)
	}
	if f.ByTenant {
		q.Set("by", "tenant")
	}
	var rep service.DORAReport
	if err := c.do(ctx, http.MethodGet, "/metrics/dora", q, nil, &rep); err != nil {
		return nil, err
	}
	return &rep, nil
}

// ValidateEnvironment implements service.Service.
func (c *Client) ValidateEnvironment(ctx context.Context, e service.EnvironmentCheck) (*service.Validation, error) {
	var v service.Validation
	if err := c.do(ctx, http.MethodPost, "/environments/validate", nil, e, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// Validate implements service.Service.
func (c *Client) Validate(ctx context.Context, d *descriptor.Descriptor) (*service.Validation, error) {
	var v service.Validation
	if err := c.do(ctx, http.MethodPost, "/deployments/validate", nil, d, &v); err != nil {
		return nil, err
	}
	return &v, nil
}
