package config

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"time"
)

// Integrations configures chat notifications (Slack, Microsoft Teams), approvals from Slack
// and change management tickets (ServiceNow, Jira).
type Integrations struct {
	// PublicURL is the address of the web interface, used in message links
	// (e.g. https://manent.example.com).
	PublicURL string `yaml:"public_url"`
	// Language of the messages: en (default) or it.
	Language string `yaml:"language"`
	// Timeout of calls to Slack, Teams and the ITSMs (default 15s).
	Timeout       time.Duration  `yaml:"timeout"`
	Slack         SlackSettings  `yaml:"slack"`
	Notifications []Notification `yaml:"notifications"`
	Change        ChangeSettings `yaml:"change"`
}

// SlackSettings configures the Slack app: the bot token is for updatable messages and
// approvals, the signing secret to verify button requests.
type SlackSettings struct {
	BotTokenRef      string `yaml:"bot_token_ref"`
	SigningSecretRef string `yaml:"signing_secret_ref"`
	// Identities maps Slack users (ID, e.g. U024BE7LH) to manent identities: approvals from Slack
	// count as those of the mapped identity.
	Identities map[string]SlackIdentity `yaml:"identities"`
	// EmailIdentity, if set, derives the identity of unlisted users from their Slack email
	// (scope users:read.email), e.g. "oidc:{email}".
	EmailIdentity string `yaml:"email_identity"`
	// APIURL replaces https://slack.com/api (tests).
	APIURL string `yaml:"api_url"`
}

// SlackIdentity is the manent identity of a Slack user.
type SlackIdentity struct {
	Subject string   `yaml:"subject"`
	Groups  []string `yaml:"groups"`
}

// Notification is a message destination for a set of applications and environments.
type Notification struct {
	// Name identifies the destination: it appears in DBOS step names (notify:<name>:<event>).
	Name         string   `yaml:"name"`
	Applications []string `yaml:"applications"`
	Environments []string `yaml:"environments"`
	// Events: deployment (start and outcome) and approval (requests and outcomes). Default both.
	Events []string `yaml:"events"`
	// DryRun includes dry-runs (default no).
	DryRun bool `yaml:"dry_run"`
	// Slack: channel with the bot token (updated messages and approval buttons) or an incoming
	// webhook (notifications only).
	Slack *SlackTarget `yaml:"slack"`
	// Teams: webhook of a Workflows flow (Power Automate) or of a channel.
	Teams *TeamsTarget `yaml:"teams"`
}

// SlackTarget is a Slack channel.
type SlackTarget struct {
	Channel    string `yaml:"channel"`
	WebhookRef string `yaml:"webhook_ref"`
}

// TeamsTarget is a Microsoft Teams webhook.
type TeamsTarget struct {
	WebhookRef string `yaml:"webhook_ref"`
}

// Notification events.
const (
	NotifyDeployment = "deployment"
	NotifyApproval   = "approval"
)

// Change management vendors.
const (
	ChangeServiceNow = "servicenow"
	ChangeJira       = "jira"
)

// ChangeSettings configures the change management system. The rules that use it are in
// governance.rules[].change.
type ChangeSettings struct {
	Provider string `yaml:"provider"`
	// PollInterval is the initial interval for reading the approval state, doubled up to 5
	// minutes (default 1m).
	PollInterval time.Duration      `yaml:"poll_interval"`
	ServiceNow   ServiceNowSettings `yaml:"servicenow"`
	Jira         JiraSettings       `yaml:"jira"`
}

// ServiceNowSettings configures the ServiceNow Table API.
type ServiceNowSettings struct {
	URL      string `yaml:"url"`
	Username string `yaml:"username"`
	// PasswordRef (with username, basic) or TokenRef (OAuth bearer).
	PasswordRef string `yaml:"password_ref"`
	TokenRef    string `yaml:"token_ref"`
	// Table of change requests (default change_request).
	Table string `yaml:"table"`
	// Fields are extra ticket fields; values accept {application}, {environment}, {version},
	// {artifact}, {deployment}, {requested_by}.
	Fields map[string]string `yaml:"fields"`
	// StartState is the state set when the deploy starts (default -1, Implement); empty to leave
	// it unchanged.
	StartState *string `yaml:"start_state"`
	// CloseStates are the states set in order at the end (default -1→0→3: Review, Closed).
	CloseStates []string `yaml:"close_states"`
	// CloseCodes are the close_code per outcome: success (default "successful"), failure
	// (default "unsuccessful").
	CloseCodes map[string]string `yaml:"close_codes"`
}

// JiraSettings configures Jira (Cloud or Data Center) with the REST API v2.
type JiraSettings struct {
	URL string `yaml:"url"`
	// Username (with TokenRef): basic, for Jira Cloud (email and API token). Without username the
	// token is a personal access token (bearer), for Jira Data Center.
	Username  string `yaml:"username"`
	TokenRef  string `yaml:"token_ref"`
	Project   string `yaml:"project"`
	IssueType string `yaml:"issue_type"`
	// Fields are extra fields, with JSON values and the same placeholders as ServiceNow.
	Fields map[string]any `yaml:"fields"`
	// ApprovedStatuses and RejectedStatuses are the ticket statuses that approve or reject the change.
	ApprovedStatuses []string `yaml:"approved_statuses"`
	RejectedStatuses []string `yaml:"rejected_statuses"`
	// Transitions by name: start (deploy start), success and failure (end).
	Transitions map[string]string `yaml:"transitions"`
}

// ChangeRule is the part of a governance rule about change management.
type ChangeRule struct {
	// Approval: the deploy waits for the change to be approved in the ITSM (besides manent
	// approvals, if required).
	Approval bool `yaml:"approval"`
	// Timeout of the approval wait (default 72h).
	Timeout time.Duration `yaml:"timeout"`
}

var (
	notificationNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
	subjectRe          = regexp.MustCompile(`^[a-z]+:.+$`)
)

// HasChange reports whether a change management system is configured.
func (i Integrations) HasChange() bool { return i.Change.Provider != "" }

// validateIntegrations checks notifications, Slack and change management.
func (c *Config) validateIntegrations() []error {
	in := c.Integrations
	var errs []error
	if in.PublicURL != "" {
		if u, err := url.Parse(in.PublicURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			errs = append(errs, fmt.Errorf("invalid integrations.public_url %q", in.PublicURL))
		}
	}
	if in.Language != "" && in.Language != "en" && in.Language != "it" {
		errs = append(errs, fmt.Errorf("invalid integrations.language %q (en|it)", in.Language))
	}
	seen := map[string]bool{}
	for i, n := range in.Notifications {
		p := fmt.Sprintf("integrations.notifications[%d]", i)
		if !notificationNameRe.MatchString(n.Name) {
			errs = append(errs, fmt.Errorf("invalid %s.name %q: lowercase letters, digits and '-'", p, n.Name))
		}
		if seen[n.Name] {
			errs = append(errs, fmt.Errorf("duplicate %s.name %q", p, n.Name))
		}
		seen[n.Name] = true
		if len(n.Applications) == 0 || len(n.Environments) == 0 {
			errs = append(errs, fmt.Errorf("%s: applications and environments are required (\"*\" for all)", p))
		}
		for _, e := range n.Events {
			if e != NotifyDeployment && e != NotifyApproval {
				errs = append(errs, fmt.Errorf("%s.events: unknown %q (deployment|approval)", p, e))
			}
		}
		switch {
		case (n.Slack == nil) == (n.Teams == nil):
			errs = append(errs, fmt.Errorf("%s: provide slack or teams", p))
		case n.Slack != nil && (n.Slack.Channel == "") == (n.Slack.WebhookRef == ""):
			errs = append(errs, fmt.Errorf("%s.slack: provide channel (with integrations.slack.bot_token_ref) or webhook_ref", p))
		case n.Slack != nil && n.Slack.Channel != "" && in.Slack.BotTokenRef == "":
			errs = append(errs, fmt.Errorf("%s.slack.channel requires integrations.slack.bot_token_ref", p))
		case n.Teams != nil && n.Teams.WebhookRef == "":
			errs = append(errs, fmt.Errorf("%s.teams.webhook_ref is required", p))
		}
	}
	for id, ident := range in.Slack.Identities {
		if !subjectRe.MatchString(ident.Subject) {
			errs = append(errs, fmt.Errorf("integrations.slack.identities.%s: subject in the form <method>:<name> (e.g. oidc:alice)", id))
		}
	}
	if in.Slack.SigningSecretRef != "" && in.Slack.BotTokenRef == "" {
		errs = append(errs, errors.New("integrations.slack.signing_secret_ref requires bot_token_ref"))
	}
	errs = append(errs, c.validateChange()...)
	return errs
}

func (c *Config) validateChange() []error {
	ch := c.Integrations.Change
	var errs []error
	rulesWithChange := slices.ContainsFunc(c.Governance.Rules, func(r GovernanceRule) bool { return r.Change != nil })
	switch ch.Provider {
	case "":
		if rulesWithChange {
			errs = append(errs, errors.New("governance.rules[].change requires integrations.change.provider"))
		}
		return errs
	case ChangeServiceNow:
		s := ch.ServiceNow
		if !validURL(s.URL) {
			errs = append(errs, errors.New("integrations.change.servicenow.url is required (https://<instance>.service-now.com)"))
		}
		if (s.Username != "" && s.PasswordRef != "") == (s.TokenRef != "") {
			errs = append(errs, errors.New("integrations.change.servicenow: provide username and password_ref or token_ref"))
		}
		for _, k := range []string{"correlation_id", "correlation_display"} {
			if _, ok := s.Fields[k]; ok {
				errs = append(errs, fmt.Errorf("integrations.change.servicenow.fields.%s is set by manent: it identifies the deploy's change", k))
			}
		}
	case ChangeJira:
		j := ch.Jira
		if !validURL(j.URL) {
			errs = append(errs, errors.New("integrations.change.jira.url is required"))
		}
		if j.TokenRef == "" || j.Project == "" || j.IssueType == "" {
			errs = append(errs, errors.New("integrations.change.jira: token_ref, project and issue_type are required"))
		}
		for k, use := range map[string]string{"project": "project", "issuetype": "issue_type"} {
			if _, ok := j.Fields[k]; ok {
				errs = append(errs, fmt.Errorf("integrations.change.jira.fields.%s is set by manent: use integrations.change.jira.%s", k, use))
			}
		}
		if l, ok := j.Fields["labels"]; ok {
			if list, isList := l.([]any); !isList || slices.ContainsFunc(list, func(v any) bool { _, isString := v.(string); return !isString }) {
				errs = append(errs, errors.New("integrations.change.jira.fields.labels must be a list of strings (added to manent's labels)"))
			}
		}
		for k := range j.Transitions {
			if k != "start" && k != "success" && k != "failure" {
				errs = append(errs, fmt.Errorf("unknown integrations.change.jira.transitions.%s (start|success|failure)", k))
			}
		}
		needsApproval := slices.ContainsFunc(c.Governance.Rules, func(r GovernanceRule) bool { return r.Change != nil && r.Change.Approval })
		if needsApproval && len(j.ApprovedStatuses) == 0 {
			errs = append(errs, errors.New("integrations.change.jira.approved_statuses is required with governance.rules[].change.approval"))
		}
	default:
		errs = append(errs, fmt.Errorf("unknown integrations.change.provider %q (servicenow|jira)", ch.Provider))
	}
	return errs
}

func validURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != ""
}
