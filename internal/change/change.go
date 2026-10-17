// Package change opens and closes the change management tickets of deploys in an ITSM
// (ServiceNow, Jira) and reads their approval state.
//
// Operations run as DBOS steps of the workflows: they must be idempotent. A deploy's ticket is
// recognized by the deploy ID (correlation_id in ServiceNow, a label in Jira), so Open creates
// it only once even when repeated.
package change

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/joeg-ita/manent/internal/secrets"
)

// Approval states of a ticket.
const (
	ApprovalPending  = "pending"
	ApprovalApproved = "approved"
	ApprovalRejected = "rejected"
)

// Ticket is a change ticket. DBOS persists it: it holds no secrets.
type Ticket struct {
	// ID is the technical identifier (sys_id in ServiceNow, id in Jira).
	ID string `json:"id"`
	// Number is the human-readable reference (CHG0031337, CHG-42).
	Number string `json:"number"`
	URL    string `json:"url,omitempty"`
	// State is the state in the ITSM, as reported by it.
	State string `json:"state,omitempty"`
	// Approval is the normalized approval state (pending, approved, rejected).
	Approval string `json:"approval,omitempty"`
}

// Request describes the deploy to open the change for.
type Request struct {
	DeploymentID string
	Application  string
	Environment  string
	Artifact     string
	Version      string
	// Kind is deploy, promotion or rollback.
	Kind        string
	RequestedBy string
	// URL is the link to the deploy in the web interface, if configured.
	URL string
	// Approval means the deploy waits for the change approval.
	Approval bool
}

// Outcome is the deploy outcome to close the change with.
type Outcome struct {
	Success bool
	// Message describes the outcome (e.g. the deploy error).
	Message string
	// Rollback is the outcome of the automatic rollback, if any.
	Rollback string
}

// Provider is a change management system.
type Provider interface {
	// Name is the system name (servicenow, jira).
	Name() string
	// Open finds the deploy's ticket or creates it.
	Open(ctx context.Context, r Request) (Ticket, error)
	// Refresh reads the ticket's state and approval again.
	Refresh(ctx context.Context, t Ticket) (Ticket, error)
	// Start reports the deploy start (e.g. Implement state).
	Start(ctx context.Context, t Ticket) error
	// Close records the outcome and closes the ticket.
	Close(ctx context.Context, t Ticket, o Outcome) error
}

// HTTPDoer abstracts the HTTP client for tests.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// expand replaces the placeholders in the configured field values.
func expand(s string, r Request) string {
	return strings.NewReplacer(
		"{application}", r.Application, "{environment}", r.Environment, "{version}", r.Version,
		"{artifact}", r.Artifact, "{deployment}", r.DeploymentID, "{requested_by}", r.RequestedBy,
	).Replace(s)
}

// expandAny applies expand to the strings of a JSON value.
func expandAny(v any, r Request) any {
	switch x := v.(type) {
	case string:
		return expand(x, r)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = expandAny(e, r)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = expandAny(e, r)
		}
		return out
	}
	return v
}

// summary is the ticket title.
func summary(r Request) string {
	kind := "Deploy"
	switch r.Kind {
	case "promotion":
		kind = "Promotion"
	case "rollback":
		kind = "Rollback"
	}
	return fmt.Sprintf("%s %s %s to %s (manent)", kind, r.Artifact, r.Version, r.Environment)
}

// description is the ticket description.
func description(r Request) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Application: %s\nEnvironment: %s\nArtifact: %s %s\nRequested by: %s\nDeployment: %s\n",
		r.Application, r.Environment, r.Artifact, r.Version, r.RequestedBy, r.DeploymentID)
	if r.URL != "" {
		fmt.Fprintf(&b, "Details: %s\n", r.URL)
	}
	b.WriteString("\nCreated and updated by manent.")
	return b.String()
}

// closeNotes describes the outcome.
func closeNotes(o Outcome) string {
	msg := "Deployment completed successfully."
	if !o.Success {
		msg = "Deployment failed: " + o.Message
		if o.Rollback != "" {
			msg += "\nAutomatic rollback: " + o.Rollback
		}
	}
	return msg
}

// credentials reads a secret at use time.
type credentials struct {
	Secrets secrets.Provider
}

func (c credentials) resolve(ctx context.Context, ref string) (string, error) {
	if ref == "" {
		return "", nil
	}
	v, err := c.Secrets.Resolve(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("ITSM credentials: %w", err)
	}
	return strings.TrimSpace(v), nil
}

// ErrPermanent marks the errors a retry cannot fix (e.g. 400, 401, 403).
var ErrPermanent = errors.New("permanent error of the ITSM system")

// doJSON runs a JSON request and decodes the response into out. 4xx statuses (except 408
// and 429) are permanent errors.
func doJSON(ctx context.Context, client HTTPDoer, method, url string, auth func(*http.Request), body, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "manent")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	auth(req)
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(data))
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		err := fmt.Errorf("%s %s: %d %s", method, req.URL.Path, resp.StatusCode, msg)
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusRequestTimeout && resp.StatusCode != http.StatusTooManyRequests {
			err = fmt.Errorf("%w: %w", ErrPermanent, err)
		}
		return resp.StatusCode, err
	}
	if out == nil || len(data) == 0 {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, json.Unmarshal(data, out)
}

// NewHTTPClient creates the ITSM client; it does not follow redirects, which would carry the
// credentials to another host.
func NewHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
