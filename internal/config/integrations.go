package config

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"time"
)

// Integrations configura notifiche in chat (Slack, Microsoft Teams), approvazioni da Slack e
// ticket di change management (ServiceNow, Jira).
type Integrations struct {
	// PublicURL è l'indirizzo dell'interfaccia web, usato nei link dei messaggi
	// (es. https://manent.example.com).
	PublicURL string `yaml:"public_url"`
	// Language dei messaggi: en (default) o it.
	Language string `yaml:"language"`
	// Timeout delle chiamate verso Slack, Teams e gli ITSM (default 15s).
	Timeout       time.Duration  `yaml:"timeout"`
	Slack         SlackSettings  `yaml:"slack"`
	Notifications []Notification `yaml:"notifications"`
	Change        ChangeSettings `yaml:"change"`
}

// SlackSettings configura l'app Slack: il bot token serve per i messaggi aggiornabili e per
// le approvazioni, il signing secret per verificare le richieste dei pulsanti.
type SlackSettings struct {
	BotTokenRef      string `yaml:"bot_token_ref"`
	SigningSecretRef string `yaml:"signing_secret_ref"`
	// Identities associa gli utenti Slack (ID, es. U024BE7LH) alle identità di manent: le
	// approvazioni da Slack valgono come quelle dell'identità associata.
	Identities map[string]SlackIdentity `yaml:"identities"`
	// EmailIdentity, se indicato, ricava l'identità degli utenti non elencati dalla loro email
	// su Slack (scope users:read.email), es. "oidc:{email}".
	EmailIdentity string `yaml:"email_identity"`
	// APIURL sostituisce https://slack.com/api (test).
	APIURL string `yaml:"api_url"`
}

// SlackIdentity è l'identità di manent di un utente Slack.
type SlackIdentity struct {
	Subject string   `yaml:"subject"`
	Groups  []string `yaml:"groups"`
}

// Notification è una destinazione dei messaggi per un insieme di applicazioni e ambienti.
type Notification struct {
	// Name identifica la destinazione: compare nei nomi degli step DBOS (notify:<name>:<evento>).
	Name         string   `yaml:"name"`
	Applications []string `yaml:"applications"`
	Environments []string `yaml:"environments"`
	// Events: deployment (avvio ed esito) e approval (richieste ed esiti). Default entrambi.
	Events []string `yaml:"events"`
	// DryRun include i dry-run (default no).
	DryRun bool `yaml:"dry_run"`
	// Slack: canale con il bot token (messaggi aggiornati e pulsanti di approvazione) oppure
	// un incoming webhook (solo notifiche).
	Slack *SlackTarget `yaml:"slack"`
	// Teams: webhook di un flusso di Workflows (Power Automate) o di un canale.
	Teams *TeamsTarget `yaml:"teams"`
}

// SlackTarget è un canale Slack.
type SlackTarget struct {
	Channel    string `yaml:"channel"`
	WebhookRef string `yaml:"webhook_ref"`
}

// TeamsTarget è un webhook di Microsoft Teams.
type TeamsTarget struct {
	WebhookRef string `yaml:"webhook_ref"`
}

// Eventi delle notifiche.
const (
	NotifyDeployment = "deployment"
	NotifyApproval   = "approval"
)

// Fornitori di change management.
const (
	ChangeServiceNow = "servicenow"
	ChangeJira       = "jira"
)

// ChangeSettings configura il sistema di change management. Le regole che lo usano sono in
// governance.rules[].change.
type ChangeSettings struct {
	Provider string `yaml:"provider"`
	// PollInterval è l'intervallo iniziale di lettura dello stato di approvazione, raddoppiato
	// fino a 5 minuti (default 1m).
	PollInterval time.Duration      `yaml:"poll_interval"`
	ServiceNow   ServiceNowSettings `yaml:"servicenow"`
	Jira         JiraSettings       `yaml:"jira"`
}

// ServiceNowSettings configura la Table API di ServiceNow.
type ServiceNowSettings struct {
	URL      string `yaml:"url"`
	Username string `yaml:"username"`
	// PasswordRef (con username, basic) oppure TokenRef (bearer OAuth).
	PasswordRef string `yaml:"password_ref"`
	TokenRef    string `yaml:"token_ref"`
	// Table delle richieste di change (default change_request).
	Table string `yaml:"table"`
	// Fields sono campi aggiuntivi del ticket; i valori accettano {application}, {environment},
	// {version}, {artifact}, {deployment}, {requested_by}.
	Fields map[string]string `yaml:"fields"`
	// StartState è lo stato impostato all'avvio del deploy (default -1, Implement); vuoto
	// per non cambiarlo.
	StartState *string `yaml:"start_state"`
	// CloseStates sono gli stati impostati in ordine alla fine (default -1→0→3: Review, Closed).
	CloseStates []string `yaml:"close_states"`
	// CloseCodes sono i close_code per esito: success (default "successful"), failure
	// (default "unsuccessful").
	CloseCodes map[string]string `yaml:"close_codes"`
}

// JiraSettings configura Jira (Cloud o Data Center) con l'API REST v2.
type JiraSettings struct {
	URL string `yaml:"url"`
	// Username (con TokenRef): basic, per Jira Cloud (email e API token). Senza username il
	// token è un personal access token (bearer), per Jira Data Center.
	Username  string `yaml:"username"`
	TokenRef  string `yaml:"token_ref"`
	Project   string `yaml:"project"`
	IssueType string `yaml:"issue_type"`
	// Fields sono campi aggiuntivi, con valori JSON e gli stessi segnaposto di ServiceNow.
	Fields map[string]any `yaml:"fields"`
	// ApprovedStatuses e RejectedStatuses sono gli stati del ticket che approvano o respingono
	// il change.
	ApprovedStatuses []string `yaml:"approved_statuses"`
	RejectedStatuses []string `yaml:"rejected_statuses"`
	// Transitions per nome: start (avvio del deploy), success e failure (fine).
	Transitions map[string]string `yaml:"transitions"`
}

// ChangeRule è la parte di una regola di governance dedicata al change management.
type ChangeRule struct {
	// Approval: il deploy attende che il change sia approvato nell'ITSM (oltre alle
	// approvazioni di manent, se richieste).
	Approval bool `yaml:"approval"`
	// Timeout dell'attesa dell'approvazione (default 72h).
	Timeout time.Duration `yaml:"timeout"`
}

var (
	notificationNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
	subjectRe          = regexp.MustCompile(`^[a-z]+:.+$`)
)

// HasChange indica se è configurato un sistema di change management.
func (i Integrations) HasChange() bool { return i.Change.Provider != "" }

// validateIntegrations verifica notifiche, Slack e change management.
func (c *Config) validateIntegrations() []error {
	in := c.Integrations
	var errs []error
	if in.PublicURL != "" {
		if u, err := url.Parse(in.PublicURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			errs = append(errs, fmt.Errorf("integrations.public_url %q non valido", in.PublicURL))
		}
	}
	if in.Language != "" && in.Language != "en" && in.Language != "it" {
		errs = append(errs, fmt.Errorf("integrations.language %q non valido (en|it)", in.Language))
	}
	seen := map[string]bool{}
	for i, n := range in.Notifications {
		p := fmt.Sprintf("integrations.notifications[%d]", i)
		if !notificationNameRe.MatchString(n.Name) {
			errs = append(errs, fmt.Errorf("%s.name %q non valido: minuscole, cifre e '-'", p, n.Name))
		}
		if seen[n.Name] {
			errs = append(errs, fmt.Errorf("%s.name %q duplicato", p, n.Name))
		}
		seen[n.Name] = true
		if len(n.Applications) == 0 || len(n.Environments) == 0 {
			errs = append(errs, fmt.Errorf("%s: applications ed environments obbligatori (\"*\" per tutti)", p))
		}
		for _, e := range n.Events {
			if e != NotifyDeployment && e != NotifyApproval {
				errs = append(errs, fmt.Errorf("%s.events: %q sconosciuto (deployment|approval)", p, e))
			}
		}
		switch {
		case (n.Slack == nil) == (n.Teams == nil):
			errs = append(errs, fmt.Errorf("%s: indicare slack oppure teams", p))
		case n.Slack != nil && (n.Slack.Channel == "") == (n.Slack.WebhookRef == ""):
			errs = append(errs, fmt.Errorf("%s.slack: indicare channel (con integrations.slack.bot_token_ref) oppure webhook_ref", p))
		case n.Slack != nil && n.Slack.Channel != "" && in.Slack.BotTokenRef == "":
			errs = append(errs, fmt.Errorf("%s.slack.channel richiede integrations.slack.bot_token_ref", p))
		case n.Teams != nil && n.Teams.WebhookRef == "":
			errs = append(errs, fmt.Errorf("%s.teams.webhook_ref obbligatorio", p))
		}
	}
	for id, ident := range in.Slack.Identities {
		if !subjectRe.MatchString(ident.Subject) {
			errs = append(errs, fmt.Errorf("integrations.slack.identities.%s: subject nella forma <metodo>:<nome> (es. oidc:alice)", id))
		}
	}
	if in.Slack.SigningSecretRef != "" && in.Slack.BotTokenRef == "" {
		errs = append(errs, errors.New("integrations.slack.signing_secret_ref richiede bot_token_ref"))
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
			errs = append(errs, errors.New("governance.rules[].change richiede integrations.change.provider"))
		}
		return errs
	case ChangeServiceNow:
		s := ch.ServiceNow
		if !validURL(s.URL) {
			errs = append(errs, errors.New("integrations.change.servicenow.url obbligatorio (https://<istanza>.service-now.com)"))
		}
		if (s.Username != "" && s.PasswordRef != "") == (s.TokenRef != "") {
			errs = append(errs, errors.New("integrations.change.servicenow: indicare username e password_ref oppure token_ref"))
		}
	case ChangeJira:
		j := ch.Jira
		if !validURL(j.URL) {
			errs = append(errs, errors.New("integrations.change.jira.url obbligatorio"))
		}
		if j.TokenRef == "" || j.Project == "" || j.IssueType == "" {
			errs = append(errs, errors.New("integrations.change.jira: token_ref, project e issue_type obbligatori"))
		}
		for k := range j.Transitions {
			if k != "start" && k != "success" && k != "failure" {
				errs = append(errs, fmt.Errorf("integrations.change.jira.transitions.%s sconosciuta (start|success|failure)", k))
			}
		}
		needsApproval := slices.ContainsFunc(c.Governance.Rules, func(r GovernanceRule) bool { return r.Change != nil && r.Change.Approval })
		if needsApproval && len(j.ApprovedStatuses) == 0 {
			errs = append(errs, errors.New("integrations.change.jira.approved_statuses obbligatorio con governance.rules[].change.approval"))
		}
	default:
		errs = append(errs, fmt.Errorf("integrations.change.provider %q sconosciuto (servicenow|jira)", ch.Provider))
	}
	return errs
}

func validURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != ""
}
