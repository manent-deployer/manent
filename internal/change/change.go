// Package change apre e chiude i ticket di change management dei deploy in un sistema ITSM
// (ServiceNow, Jira) e ne legge lo stato di approvazione.
//
// Le operazioni sono eseguite come step DBOS dai workflow: devono essere idempotenti. Il
// ticket di un deploy è riconosciuto dall'ID del deploy (correlation_id in ServiceNow,
// un'etichetta in Jira), quindi Open lo crea una volta sola anche se viene ripetuto.
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

// Stati di approvazione di un ticket.
const (
	ApprovalPending  = "pending"
	ApprovalApproved = "approved"
	ApprovalRejected = "rejected"
)

// Ticket è un ticket di change. Viene persistito da DBOS: non contiene segreti.
type Ticket struct {
	// ID è l'identificativo tecnico (sys_id in ServiceNow, id in Jira).
	ID string `json:"id"`
	// Number è il riferimento leggibile (CHG0031337, CHG-42).
	Number string `json:"number"`
	URL    string `json:"url,omitempty"`
	// State è lo stato nel sistema ITSM, come lo riporta.
	State string `json:"state,omitempty"`
	// Approval è lo stato di approvazione normalizzato (pending, approved, rejected).
	Approval string `json:"approval,omitempty"`
}

// Request descrive il deploy per cui aprire il change.
type Request struct {
	DeploymentID string
	Application  string
	Environment  string
	Artifact     string
	Version      string
	// Kind è deploy, promotion o rollback.
	Kind        string
	RequestedBy string
	// URL è il link al deploy nell'interfaccia web, se configurata.
	URL string
	// Approval indica che il deploy attende l'approvazione del change.
	Approval bool
}

// Outcome è l'esito del deploy con cui chiudere il change.
type Outcome struct {
	Success bool
	// Message descrive l'esito (es. l'errore del deploy).
	Message string
	// Rollback è l'esito del rollback automatico, se previsto.
	Rollback string
}

// Provider è un sistema di change management.
type Provider interface {
	// Name è il nome del sistema (servicenow, jira).
	Name() string
	// Open trova il ticket del deploy o lo crea.
	Open(ctx context.Context, r Request) (Ticket, error)
	// Refresh rilegge stato e approvazione del ticket.
	Refresh(ctx context.Context, t Ticket) (Ticket, error)
	// Start segnala l'avvio del deploy (es. stato Implement).
	Start(ctx context.Context, t Ticket) error
	// Close registra l'esito e chiude il ticket.
	Close(ctx context.Context, t Ticket, o Outcome) error
}

// HTTPDoer astrae il client HTTP per i test.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// expand sostituisce i segnaposto nei valori configurati dei campi.
func expand(s string, r Request) string {
	return strings.NewReplacer(
		"{application}", r.Application, "{environment}", r.Environment, "{version}", r.Version,
		"{artifact}", r.Artifact, "{deployment}", r.DeploymentID, "{requested_by}", r.RequestedBy,
	).Replace(s)
}

// expandAny applica expand alle stringhe di un valore JSON.
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

// summary è il titolo del ticket.
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

// description è la descrizione del ticket.
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

// closeNotes descrive l'esito.
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

// credentials legge un segreto al momento dell'uso.
type credentials struct {
	Secrets secrets.Provider
}

func (c credentials) resolve(ctx context.Context, ref string) (string, error) {
	if ref == "" {
		return "", nil
	}
	v, err := c.Secrets.Resolve(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("credenziali ITSM: %w", err)
	}
	return strings.TrimSpace(v), nil
}

// ErrPermanent marca gli errori che un nuovo tentativo non risolve (es. 400, 401, 403).
var ErrPermanent = errors.New("errore permanente del sistema ITSM")

// doJSON esegue una richiesta JSON e decodifica la risposta in out. Gli stati 4xx (salvo 408
// e 429) sono errori permanenti.
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

// NewHTTPClient crea il client per gli ITSM; non segue i redirect, che porterebbero le
// credenziali verso un altro host.
func NewHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
