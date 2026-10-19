package change

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/joeg-ita/manent/internal/secrets"
)

// Jira creates an issue for every deploy with the REST API v2 (Jira Cloud and Data Center).
// The deploy's issue has the label "manent-<deploy id>"; approval is derived from the status.
type Jira struct {
	URL string
	// Username (with TokenRef): basic, for Jira Cloud. Without it: TokenRef is a PAT (bearer).
	Username         string
	TokenRef         string
	Project          string
	IssueType        string
	Fields           map[string]any
	ApprovedStatuses []string
	RejectedStatuses []string
	// Transitions by name: start, success, failure.
	Transitions map[string]string
	Secrets     secrets.Provider
	Client      HTTPDoer
}

// Name returns the system name.
func (j *Jira) Name() string { return "jira" }

var labelRe = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// Label is the label that ties the issue to the deploy.
func Label(deploymentID string) string {
	return "manent-" + labelRe.ReplaceAllString(deploymentID, "-")
}

// issueLabels returns the labels of a new issue: manent's and the configured ones, without duplicates.
func issueLabels(configured any, deploymentID string) []string {
	labels := []string{"manent", Label(deploymentID)}
	if list, ok := configured.([]any); ok {
		for _, l := range list {
			if s, ok := l.(string); ok && s != "" && !slices.Contains(labels, s) {
				labels = append(labels, s)
			}
		}
	}
	return labels
}

func (j *Jira) api(path string) string { return strings.TrimRight(j.URL, "/") + "/rest/api/2" + path }

func (j *Jira) auth(ctx context.Context) (func(*http.Request), error) {
	tok, err := credentials{Secrets: j.Secrets}.resolve(ctx, j.TokenRef)
	if err != nil {
		return nil, err
	}
	if j.Username != "" {
		return func(r *http.Request) { r.SetBasicAuth(j.Username, tok) }, nil
	}
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }, nil
}

type jiraIssue struct {
	ID     string `json:"id"`
	Key    string `json:"key"`
	Fields struct {
		Status struct {
			Name string `json:"name"`
		} `json:"status"`
	} `json:"fields"`
}

func (j *Jira) ticket(i jiraIssue) Ticket {
	t := Ticket{ID: i.ID, Number: i.Key, State: i.Fields.Status.Name, Approval: ApprovalPending,
		URL: strings.TrimRight(j.URL, "/") + "/browse/" + url.PathEscape(i.Key)}
	switch {
	case containsFold(j.ApprovedStatuses, t.State):
		t.Approval = ApprovalApproved
	case containsFold(j.RejectedStatuses, t.State):
		t.Approval = ApprovalRejected
	}
	return t
}

func containsFold(list []string, s string) bool {
	return slices.ContainsFunc(list, func(e string) bool { return strings.EqualFold(e, s) })
}

// search looks for the deploy's issue. Jira Cloud exposes /search/jql, Jira Data Center /search.
func (j *Jira) search(ctx context.Context, auth func(*http.Request), deploymentID string) (*jiraIssue, error) {
	q := url.Values{
		"jql":        {fmt.Sprintf(`project = "%s" AND labels = "%s" ORDER BY created DESC`, j.Project, Label(deploymentID))},
		"fields":     {"status"},
		"maxResults": {"1"},
	}
	var res struct {
		Issues []jiraIssue `json:"issues"`
	}
	status, err := doJSON(ctx, j.Client, http.MethodGet, j.api("/search/jql?"+q.Encode()), auth, nil, &res)
	if status == http.StatusNotFound || status == http.StatusGone {
		_, err = doJSON(ctx, j.Client, http.MethodGet, j.api("/search?"+q.Encode()), auth, nil, &res)
	}
	if err != nil {
		return nil, err
	}
	if len(res.Issues) == 0 {
		return nil, nil
	}
	return &res.Issues[0], nil
}

// Open looks for the deploy's issue and creates it if missing.
func (j *Jira) Open(ctx context.Context, r Request) (Ticket, error) {
	auth, err := j.auth(ctx)
	if err != nil {
		return Ticket{}, err
	}
	found, err := j.search(ctx, auth, r.DeploymentID)
	if err != nil {
		return Ticket{}, fmt.Errorf("searching the issue: %w", err)
	}
	if found != nil {
		return j.Refresh(ctx, j.ticket(*found))
	}
	fields := map[string]any{
		"project":     map[string]any{"key": j.Project},
		"issuetype":   map[string]any{"name": j.IssueType},
		"summary":     summary(r),
		"description": description(r),
		"labels":      []string{"manent", Label(r.DeploymentID)},
	}
	for _, k := range slices.Sorted(maps.Keys(j.Fields)) {
		fields[k] = expandAny(j.Fields[k], r)
	}
	// Configured labels are added to manent's: the manent-<id> label is how search finds the
	// issue again, so it must never be replaced.
	fields["labels"] = issueLabels(fields["labels"], r.DeploymentID)
	var created jiraIssue
	if _, err := doJSON(ctx, j.Client, http.MethodPost, j.api("/issue"), auth, map[string]any{"fields": fields}, &created); err != nil {
		return Ticket{}, fmt.Errorf("creating the issue: %w", err)
	}
	return j.Refresh(ctx, Ticket{ID: created.ID, Number: created.Key})
}

// Refresh reads the issue status again.
func (j *Jira) Refresh(ctx context.Context, t Ticket) (Ticket, error) {
	auth, err := j.auth(ctx)
	if err != nil {
		return t, err
	}
	var i jiraIssue
	if _, err := doJSON(ctx, j.Client, http.MethodGet, j.api("/issue/"+url.PathEscape(t.Number)+"?fields=status"), auth, nil, &i); err != nil {
		return t, fmt.Errorf("reading issue %s: %w", t.Number, err)
	}
	return j.ticket(i), nil
}

// transition runs the transition configured for the event; none if not configured or already
// done (the issue is already in the target status).
func (j *Jira) transition(ctx context.Context, t Ticket, event string) error {
	name := j.Transitions[event]
	if name == "" {
		return nil
	}
	auth, err := j.auth(ctx)
	if err != nil {
		return err
	}
	var list struct {
		Transitions []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			To   struct {
				Name string `json:"name"`
			} `json:"to"`
		} `json:"transitions"`
	}
	if _, err := doJSON(ctx, j.Client, http.MethodGet, j.api("/issue/"+url.PathEscape(t.Number)+"/transitions"), auth, nil, &list); err != nil {
		return err
	}
	for _, tr := range list.Transitions {
		if strings.EqualFold(tr.Name, name) {
			_, err := doJSON(ctx, j.Client, http.MethodPost, j.api("/issue/"+url.PathEscape(t.Number)+"/transitions"), auth,
				map[string]any{"transition": map[string]any{"id": tr.ID}}, nil)
			return err
		}
	}
	cur, err := j.Refresh(ctx, t)
	if err == nil && strings.EqualFold(cur.State, name) {
		return nil
	}
	return fmt.Errorf("%w: transition %q not available for %s in state %q", ErrPermanent, name, t.Number, cur.State)
}

// Start runs the start transition.
func (j *Jira) Start(ctx context.Context, t Ticket) error {
	if err := j.transition(ctx, t, "start"); err != nil {
		return fmt.Errorf("starting issue %s: %w", t.Number, err)
	}
	return nil
}

// Close adds a comment with the outcome and runs the closing transition.
func (j *Jira) Close(ctx context.Context, t Ticket, o Outcome) error {
	auth, err := j.auth(ctx)
	if err != nil {
		return err
	}
	if _, err := doJSON(ctx, j.Client, http.MethodPost, j.api("/issue/"+url.PathEscape(t.Number)+"/comment"), auth,
		map[string]any{"body": closeNotes(o)}, nil); err != nil {
		return fmt.Errorf("commenting on issue %s: %w", t.Number, err)
	}
	event := "success"
	if !o.Success {
		event = "failure"
	}
	if err := j.transition(ctx, t, event); err != nil {
		return fmt.Errorf("closing issue %s: %w", t.Number, err)
	}
	return nil
}
