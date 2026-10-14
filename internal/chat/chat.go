// Package chat invia le notifiche dei deploy e delle approvazioni a Slack e Microsoft Teams e
// riceve le decisioni prese con i pulsanti dei messaggi Slack.
//
// Le destinazioni (Route) sono scelte all'accodamento del deploy per applicazione e ambiente
// e salvate nella richiesta del workflow: l'invio è uno step DBOS, quindi la sequenza degli
// step non dipende dalla configurazione letta durante una recovery. Un invio fallito non fa
// mai fallire il deploy.
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

	"github.com/joeg-ita/manent/internal/glob"
	"github.com/joeg-ita/manent/internal/secrets"
)

// Eventi notificati.
const (
	EventStarted           = "started"
	EventSucceeded         = "succeeded"
	EventFailed            = "failed"
	EventApprovalRequested = "approval_requested"
	EventApprovalApproved  = "approval_approved"
	EventApprovalRejected  = "approval_rejected"
	EventApprovalExpired   = "approval_expired"
)

// Categorie di eventi delle destinazioni.
const (
	CategoryDeployment = "deployment"
	CategoryApproval   = "approval"
)

// Category restituisce la categoria di un evento.
func Category(event string) string {
	if strings.HasPrefix(event, "approval_") {
		return CategoryApproval
	}
	return CategoryDeployment
}

// Message è il contenuto di una notifica.
type Message struct {
	Event        string
	DeploymentID string
	Application  string
	Environment  string
	Artifact     string
	Version      string
	// Kind è deploy, promotion o rollback.
	Kind        string
	RequestedBy string
	DryRun      bool
	Error       string
	Rollback    string
	Approval    *Approval
	Change      *Change
}

// Approval descrive un'approvazione.
type Approval struct {
	Required  int
	Decisions []Decision
	ExpiresAt time.Time
	// Change indica che serve anche l'approvazione del change nel sistema ITSM.
	Change bool
}

// Decision è la decisione di un approvatore.
type Decision struct {
	Approver string
	Approve  bool
	Comment  string
}

// Change è il ticket di change del deploy.
type Change struct {
	Number string
	URL    string
}

// Ref identifica un messaggio inviato, per aggiornarlo in seguito (solo Slack con bot token).
// Viene persistito da DBOS come output dello step.
type Ref struct {
	Channel string `json:"channel,omitempty"`
	TS      string `json:"ts,omitempty"`
}

// Route è una destinazione delle notifiche.
type Route struct {
	Name         string
	Applications []string
	Environments []string
	// Categories: deployment, approval (vuoto = entrambe).
	Categories []string
	DryRun     bool
	// Esattamente una delle tre destinazioni.
	SlackChannel    string
	SlackWebhookRef string
	TeamsWebhookRef string
}

func (r Route) wants(event string) bool {
	return len(r.Categories) == 0 || slices.Contains(r.Categories, Category(event))
}

// HTTPDoer astrae il client HTTP per i test.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// NewHTTPClient crea il client per Slack e Teams; non segue i redirect, che porterebbero
// token e URL dei webhook verso un altro host.
func NewHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Notifier invia i messaggi alle destinazioni configurate.
type Notifier struct {
	Routes []Route
	// PublicURL è l'indirizzo dell'interfaccia web per i link ("" = nessun link).
	PublicURL string
	Language  string
	// Slack è il client dell'API di Slack (bot token); nil se non configurato.
	Slack   *SlackAPI
	Secrets secrets.Provider
	Client  HTTPDoer
	Logger  *slog.Logger
}

// Match restituisce, in ordine di configurazione, i nomi delle destinazioni di un deploy.
func (n *Notifier) Match(application, environment string, dryRun bool) []string {
	if n == nil {
		return nil
	}
	var out []string
	for _, r := range n.Routes {
		if dryRun && !r.DryRun {
			continue
		}
		if glob.Any(r.Applications, application) && glob.Any(r.Environments, environment) {
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

// Wants indica se la destinazione riceve l'evento. Una destinazione rimossa dalla
// configurazione non riceve più nulla.
func (n *Notifier) Wants(route, event string) bool {
	r, ok := n.route(route)
	return ok && r.wants(event)
}

// Send invia il messaggio alla destinazione. Con il bot di Slack e prev valorizzato aggiorna
// il messaggio precedente (es. l'avvio del deploy diventa il suo esito); restituisce il
// riferimento al messaggio, se aggiornabile.
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
			return nil, errors.New("destinazione Slack senza bot token")
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
	return nil, fmt.Errorf("destinazione %s senza canale", route)
}

// link è il collegamento al deploy nell'interfaccia web.
func (n *Notifier) link(id string) string {
	if n.PublicURL == "" || id == "" {
		return ""
	}
	return strings.TrimRight(n.PublicURL, "/") + "/deployments/" + id
}

// postWebhook invia il corpo JSON a un webhook letto dai segreti al momento dell'uso.
func (n *Notifier) postWebhook(ctx context.Context, ref string, body any) error {
	u, err := n.Secrets.Resolve(ctx, ref)
	if err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	u = strings.TrimSpace(u)
	if !strings.HasPrefix(u, "https://") {
		return errors.New("webhook: è ammesso solo https")
	}
	_, err = postJSON(ctx, n.Client, u, nil, body, nil)
	return err
}
