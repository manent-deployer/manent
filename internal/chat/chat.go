// Package chat sends deploy and approval notifications to Slack and Microsoft Teams and
// receives the decisions taken with the buttons of Slack messages.
//
// Destinations (Route) are chosen when the deploy is enqueued, by application and environment,
// and saved in the workflow request: sending is a DBOS step, so the step sequence does not
// depend on the configuration read during a recovery. A failed send never fails the deploy.
package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/manent-deployer/manent/internal/glob"
	"github.com/manent-deployer/manent/internal/pipeline"
	"github.com/manent-deployer/manent/internal/secrets"
	"github.com/manent-deployer/manent/internal/tenant"
)

// Notified events.
const (
	EventStarted           = "started"
	EventSucceeded         = "succeeded"
	EventFailed            = "failed"
	EventApprovalRequested = "approval_requested"
	EventApprovalApproved  = "approval_approved"
	EventApprovalRejected  = "approval_rejected"
	EventApprovalExpired   = "approval_expired"
)

// Event categories of the destinations.
const (
	CategoryDeployment = "deployment"
	CategoryApproval   = "approval"
)

// Category returns the category of an event.
func Category(event string) string {
	if strings.HasPrefix(event, "approval_") {
		return CategoryApproval
	}
	return CategoryDeployment
}

// Message is the content of a notification.
type Message struct {
	Event        string
	DeploymentID string
	Application  string
	Environment  string
	// Tenant is the tenant of the deploy, if any.
	Tenant   string
	Artifact string
	Version  string
	// Kind is deploy, promotion or rollback.
	Kind        string
	RequestedBy string
	DryRun      bool
	Error       string
	Rollback    string
	Approval    *Approval
	Change      *Change
}

// Approval describes an approval.
type Approval struct {
	Required  int
	Decisions []Decision
	ExpiresAt time.Time
	// Change means the change approval in the ITSM is also needed.
	Change bool
}

// Decision is the decision of an approver.
type Decision struct {
	Approver string
	Approve  bool
	Comment  string
}

// Change is the deploy's change ticket.
type Change struct {
	Number string
	URL    string
}

// Ref identifies a sent message, to update it later (only Slack with a bot token).
// DBOS persists it as the step output.
type Ref struct {
	Channel string `json:"channel,omitempty"`
	TS      string `json:"ts,omitempty"`
}

// Route is a notification destination.
type Route struct {
	Name         string
	Applications []string
	Environments []string
	// Categories: deployment, approval (empty = both).
	Categories []string
	DryRun     bool
	// Exactly one of the three destinations.
	SlackChannel    string
	SlackWebhookRef string
	TeamsWebhookRef string
}

func (r Route) wants(event string) bool {
	return len(r.Categories) == 0 || slices.Contains(r.Categories, Category(event))
}

// HTTPDoer abstracts the HTTP client for tests.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// NewHTTPClient creates the client for Slack and Teams; it does not follow redirects, which
// would carry tokens and webhook URLs to another host.
func NewHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Notifier sends messages to the configured destinations.
type Notifier struct {
	Routes []Route
	// PublicURL is the web interface address for links ("" = no link).
	PublicURL string
	Language  string
	// Slack is the Slack API client (bot token); nil if not configured.
	Slack   *SlackAPI
	Secrets secrets.Provider
	Client  HTTPDoer
	Logger  *slog.Logger
}

// Match returns, in configuration order, the names of the destinations of a deploy of the
// tenant's application; application patterns are qualified (tenant.AppMatches).
func (n *Notifier) Match(tenantName, application, environment string, dryRun bool) []string {
	if n == nil {
		return nil
	}
	var out []string
	for _, r := range n.Routes {
		if dryRun && !r.DryRun {
			continue
		}
		if tenant.AnyApp(r.Applications, tenantName, application) && glob.Any(r.Environments, environment) {
			out = append(out, r.Name)
		}
	}
	return out
}

func (n *Notifier) route(name string) (Route, bool) {
	i := slices.IndexFunc(n.Routes, func(r Route) bool { return r.Name == name })
	if i < 0 {
		return Route{}, false
	}
	return n.Routes[i], true
}

// Wants reports whether the destination receives the event. A destination removed from the
// configuration receives nothing any more.
func (n *Notifier) Wants(route, event string) bool {
	r, ok := n.route(route)
	return ok && r.wants(event)
}

// Send sends the message to the destination. With the Slack bot and prev set it updates the
// previous message (e.g. the deploy start becomes its outcome); it returns the reference to
// the message, if it can be updated.
func (n *Notifier) Send(ctx context.Context, route string, msg Message, prev *Ref) (*Ref, error) {
	r, ok := n.route(route)
	if !ok {
		return nil, nil
	}
	t := texts(n.Language)
	link := n.link(msg.DeploymentID)
	switch {
	case r.SlackChannel != "":
		if n.Slack == nil {
			return nil, errors.New("Slack destination without bot token")
		}
		blocks := slackBlocks(t, msg, link, true)
		text := headline(t, msg)
		if prev != nil && prev.TS != "" {
			return prev, n.Slack.Update(ctx, *prev, text, blocks)
		}
		return n.Slack.Post(ctx, r.SlackChannel, text, blocks)
	case r.SlackWebhookRef != "":
		return nil, n.postWebhook(ctx, r.SlackWebhookRef, map[string]any{"text": headline(t, msg), "blocks": slackBlocks(t, msg, link, false)})
	case r.TeamsWebhookRef != "":
		return nil, n.postWebhook(ctx, r.TeamsWebhookRef, teamsCard(t, msg, link))
	}
	return nil, fmt.Errorf("destination %s without channel", route)
}

// link is the link to the deploy in the web interface.
func (n *Notifier) link(id string) string {
	if n.PublicURL == "" || id == "" {
		return ""
	}
	return strings.TrimRight(n.PublicURL, "/") + "/deployments/" + id
}

// postWebhook sends the JSON body to a webhook read from the secrets at use time.
func (n *Notifier) postWebhook(ctx context.Context, ref string, body any) error {
	u, err := n.Secrets.Resolve(ctx, ref)
	if err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	u = strings.TrimSpace(u)
	if !strings.HasPrefix(u, "https://") {
		// A retry cannot fix the configured URL.
		return pipeline.Permanent(errors.New("webhook: only https is allowed"))
	}
	_, err = postJSON(ctx, n.Client, u, nil, body, nil)
	return err
}
