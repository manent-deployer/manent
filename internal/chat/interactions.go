package chat

import (
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
	"strings"
	"time"

	"github.com/joeg-ita/manent/internal/auth"
	"github.com/joeg-ita/manent/internal/secrets"
	"github.com/joeg-ita/manent/internal/service"
)

// Identity è l'identità di manent di un utente Slack.
type Identity struct {
	// Principal è "<metodo>:<nome>", es. "oidc:alice".
	Principal string
	Groups    []string
}

// Decider registra una decisione su un deploy: è il servizio con i permessi (authz.Guard),
// che verifica il ruolo dell'identità e il divieto di auto-approvazione.
type Decider interface {
	Decide(ctx context.Context, id string, r service.DecisionRequest) error
}

// Interactions riceve i clic sui pulsanti dei messaggi Slack (Interactivity Request URL
// dell'app) e li trasforma in decisioni con l'identità associata all'utente.
type Interactions struct {
	SigningSecretRef string
	Secrets          secrets.Provider
	// Identities associa gli ID degli utenti Slack alle identità di manent.
	Identities map[string]Identity
	// EmailIdentity ricava l'identità dall'email dell'utente (es. "oidc:{email}"); richiede API.
	EmailIdentity string
	API           *SlackAPI
	Decider       Decider
	Language      string
	// Client invia le risposte a response_url.
	Client HTTPDoer
	// ResponseHosts sono gli host ammessi per response_url (default hooks.slack.com).
	ResponseHosts []string
	Logger        *slog.Logger
	Now           func() time.Time
}

// slackPayload è la parte usata del payload di un'interazione block_actions.
type slackPayload struct {
	Type string `json:"type"`
	User struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"user"`
	Team struct {
		ID string `json:"id"`
	} `json:"team"`
	ResponseURL string `json:"response_url"`
	Actions     []struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
	} `json:"actions"`
}

// ServeHTTP verifica la firma, risolve l'identità e registra la decisione. Risponde subito a
// Slack (entro i 3 secondi richiesti) e comunica l'esito a chi ha cliccato con un messaggio
// visibile solo a lui; il messaggio del canale viene aggiornato dal workflow di approvazione.
func (h *Interactions) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "richiesta non leggibile", http.StatusBadRequest)
		return
	}
	secret, err := h.Secrets.Resolve(r.Context(), h.SigningSecretRef)
	if err != nil {
		h.Logger.Error("signing secret di Slack non disponibile", "error", err)
		http.Error(w, "configurazione non disponibile", http.StatusServiceUnavailable)
		return
	}
	now := time.Now
	if h.Now != nil {
		now = h.Now
	}
	if err := VerifySlackSignature(strings.TrimSpace(secret), r.Header, body, now()); err != nil {
		h.Logger.Warn("interazione Slack rifiutata", "error", err, "remote", r.RemoteAddr)
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		http.Error(w, "payload non valido", http.StatusBadRequest)
		return
	}
	var p slackPayload
	if err := json.Unmarshal([]byte(form.Get("payload")), &p); err != nil {
		http.Error(w, "payload non valido", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)
	if p.Type != "block_actions" || len(p.Actions) == 0 {
		return
	}
	a := p.Actions[0]
	if a.ActionID != ActionApprove && a.ActionID != ActionReject {
		return
	}
	// La decisione continua dopo la risposta a Slack: il context della richiesta non va usato.
	go h.decide(context.WithoutCancel(r.Context()), p, a.ActionID == ActionApprove, a.Value)
}

func (h *Interactions) decide(ctx context.Context, p slackPayload, approve bool, id string) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	t := texts(h.Language)
	log := h.Logger.With("deployment", id, "slack_user", p.User.ID)
	reply := func(msg string) {
		if err := h.respond(ctx, p.ResponseURL, msg); err != nil {
			log.Warn("risposta a Slack non inviata", "error", err)
		}
	}
	ident, err := h.identity(ctx, p.User.ID)
	if err != nil {
		log.Warn("decisione da Slack rifiutata: utente senza identità di manent", "error", err)
		reply(fmt.Sprintf(t.noIdentity(), p.User.ID))
		return
	}
	method, subject, ok := strings.Cut(ident.Principal, ":")
	if !ok || method == "" || subject == "" {
		log.Error("identità di manent non valida per l'utente Slack", "identity", ident.Principal)
		reply(fmt.Sprintf(t.noIdentity(), p.User.ID))
		return
	}
	pr := &auth.Principal{Method: method, Subject: subject, Groups: ident.Groups}
	ctx = auth.WithPrincipal(ctx, pr)
	comment := "Slack @" + p.User.Username
	if err := h.Decider.Decide(ctx, id, service.DecisionRequest{Approve: approve, Comment: comment}); err != nil {
		log.Warn("decisione da Slack non registrata", "principal", pr.String(), "error", err)
		reply(t.decisionFailed(id, err))
		return
	}
	log.Info("decisione da Slack registrata", "principal", pr.String(), "approve", approve)
	reply(t.decisionDone(id, approve))
}

// identity risolve l'identità di manent dell'utente Slack: prima l'elenco esplicito, poi
// l'email, se configurata.
func (h *Interactions) identity(ctx context.Context, user string) (Identity, error) {
	if id, ok := h.Identities[user]; ok {
		return id, nil
	}
	if h.EmailIdentity == "" || h.API == nil {
		return Identity{}, errors.New("utente non associato in integrations.slack.identities")
	}
	email, err := h.API.UserEmail(ctx, user)
	if err != nil {
		return Identity{}, err
	}
	return Identity{Principal: strings.ReplaceAll(h.EmailIdentity, "{email}", strings.ToLower(email))}, nil
}

// respond invia un messaggio effimero a response_url, solo verso gli host di Slack.
func (h *Interactions) respond(ctx context.Context, responseURL, text string) error {
	u, err := url.Parse(responseURL)
	if err != nil || u.Scheme != "https" && !(len(h.ResponseHosts) > 0 && u.Scheme == "http") {
		return errors.New("response_url non valido")
	}
	hosts := h.ResponseHosts
	if len(hosts) == 0 {
		hosts = []string{"hooks.slack.com"}
	}
	ok := false
	for _, host := range hosts {
		ok = ok || strings.EqualFold(u.Host, host)
	}
	if !ok {
		return fmt.Errorf("response_url verso %s non ammesso", u.Host)
	}
	_, err = postJSON(ctx, h.Client, responseURL, nil, map[string]any{
		"response_type": "ephemeral", "replace_original": false, "text": text,
	}, nil)
	return err
}

func (t text) noIdentity() string {
	if t.lang == "it" {
		return "Il tuo utente Slack (%s) non è associato a un'identità di manent: chiedi agli amministratori di aggiungerlo."
	}
	return "Your Slack user (%s) is not linked to a manent identity: ask the administrators to add it."
}

func (t text) decisionFailed(id string, err error) string {
	if t.lang == "it" {
		return fmt.Sprintf("Decisione su %s non registrata: %v", id, err)
	}
	return fmt.Sprintf("Decision on %s not recorded: %v", id, err)
}

func (t text) decisionDone(id string, approve bool) string {
	if t.lang == "it" {
		if approve {
			return "Hai approvato " + id + "."
		}
		return "Hai respinto " + id + "."
	}
	if approve {
		return "You approved " + id + "."
	}
	return "You rejected " + id + "."
}

func hmacHex(secret, msg string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

func hmacEqual(a, b string) bool { return hmac.Equal([]byte(a), []byte(b)) }
