package app

import (
	"cmp"
	"log/slog"
	"net/http"
	"time"

	"github.com/joeg-ita/manent/internal/change"
	"github.com/joeg-ita/manent/internal/chat"
	"github.com/joeg-ita/manent/internal/config"
	"github.com/joeg-ita/manent/internal/secrets"
)

// integrationTimeout is the default timeout of calls to Slack, Teams and the ITSMs.
const integrationTimeout = 15 * time.Second

// NewChatNotifier creates the chat notifier from the configured destinations; nil without
// destinations. A new chat channel is added in internal/chat.
func NewChatNotifier(cfg *config.Config, sp secrets.Provider, log *slog.Logger) *chat.Notifier {
	in := cfg.Integrations
	if len(in.Notifications) == 0 {
		return nil
	}
	client := chat.NewHTTPClient(cmp.Or(in.Timeout, integrationTimeout))
	n := &chat.Notifier{PublicURL: in.PublicURL, Language: in.Language, Secrets: sp, Client: client, Logger: log.With("component", "chat")}
	if in.Slack.BotTokenRef != "" {
		n.Slack = &chat.SlackAPI{URL: in.Slack.APIURL, TokenRef: in.Slack.BotTokenRef, Secrets: sp, Client: client}
	}
	for _, r := range in.Notifications {
		route := chat.Route{Name: r.Name, Applications: r.Applications, Environments: r.Environments, Categories: r.Events, DryRun: r.DryRun}
		switch {
		case r.Slack != nil:
			route.SlackChannel, route.SlackWebhookRef = r.Slack.Channel, r.Slack.WebhookRef
		case r.Teams != nil:
			route.TeamsWebhookRef = r.Teams.WebhookRef
		}
		n.Routes = append(n.Routes, route)
	}
	return n
}

// NewChangeProvider creates the configured change management system; nil if absent.
// A new ITSM is added here, implementing change.Provider.
func NewChangeProvider(cfg *config.Config, sp secrets.Provider) change.Provider {
	ch := cfg.Integrations.Change
	client := change.NewHTTPClient(cmp.Or(cfg.Integrations.Timeout, integrationTimeout))
	switch ch.Provider {
	case config.ChangeServiceNow:
		s := ch.ServiceNow
		start := change.SNStateImplement
		if s.StartState != nil {
			start = *s.StartState
		}
		closeStates := s.CloseStates
		if closeStates == nil {
			closeStates = []string{change.SNStateReview, change.SNStateClosed}
		}
		codes := map[string]string{"success": "successful", "failure": "unsuccessful"}
		for k, v := range s.CloseCodes {
			codes[k] = v
		}
		return &change.ServiceNow{
			URL: s.URL, Username: s.Username, PasswordRef: s.PasswordRef, TokenRef: s.TokenRef, Table: s.Table,
			Fields: s.Fields, StartState: start, CloseStates: closeStates, CloseCodes: codes, Secrets: sp, Client: client,
		}
	case config.ChangeJira:
		j := ch.Jira
		return &change.Jira{
			URL: j.URL, Username: j.Username, TokenRef: j.TokenRef, Project: j.Project, IssueType: j.IssueType, Fields: j.Fields,
			ApprovedStatuses: j.ApprovedStatuses, RejectedStatuses: j.RejectedStatuses, Transitions: j.Transitions,
			Secrets: sp, Client: client,
		}
	}
	return nil
}

// NewSlackInteractions creates the receiver of Slack approval buttons; nil without a signing
// secret. Decisions go through decider, the service with permissions.
func NewSlackInteractions(cfg *config.Config, sp secrets.Provider, decider chat.Decider, log *slog.Logger) http.Handler {
	s := cfg.Integrations.Slack
	if s.SigningSecretRef == "" {
		return nil
	}
	client := chat.NewHTTPClient(cmp.Or(cfg.Integrations.Timeout, integrationTimeout))
	ids := map[string]chat.Identity{}
	for user, ident := range s.Identities {
		ids[user] = chat.Identity{Principal: ident.Subject, Groups: ident.Groups}
	}
	return &chat.Interactions{
		SigningSecretRef: s.SigningSecretRef, Secrets: sp, Identities: ids, EmailIdentity: s.EmailIdentity,
		API:     &chat.SlackAPI{URL: s.APIURL, TokenRef: s.BotTokenRef, Secrets: sp, Client: client},
		Decider: decider, Language: cfg.Integrations.Language, Client: client,
		Logger: log.With("component", "slack"),
	}
}
