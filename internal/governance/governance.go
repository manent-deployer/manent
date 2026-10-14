// Package governance applica le regole di rilascio per applicazione e ambiente: promozione
// obbligatoria da altri ambienti, approvazioni e finestre di freeze.
package governance

import (
	"errors"
	"fmt"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/joeg-ita/manent/internal/glob"
)

// DefaultApprovalTimeout è la durata di default di una richiesta di approvazione.
const DefaultApprovalTimeout = 72 * time.Hour

// Approvals stabilisce le approvazioni richieste prima di un deploy.
type Approvals struct {
	// Required è il numero di approvazioni di identità distinte (0 = nessuna approvazione).
	Required int `json:"required"`
	// AllowSelf ammette l'approvazione di chi ha richiesto il deploy.
	AllowSelf bool `json:"allow_self,omitempty"`
	// Timeout dopo cui la richiesta scade e il deploy non viene eseguito.
	Timeout time.Duration `json:"timeout"`
}

// RequiredOrZero restituisce il numero di approvazioni richieste (0 senza regola).
func (a *Approvals) RequiredOrZero() int {
	if a == nil {
		return 0
	}
	return a.Required
}

// Rule sono le regole di rilascio di un insieme di applicazioni e ambienti (pattern con "*").
type Rule struct {
	Name         string
	Applications []string
	Environments []string
	// Requires elenca gli ambienti in cui lo stesso artefatto (nome, versione e sha256) deve
	// essere già stato rilasciato con successo.
	Requires  []string
	Approvals Approvals
	// Change, se presente, apre un ticket di change management per ogni deploy.
	Change *Change
}

// Change è la regola di change management di un deploy, fissata all'accodamento.
type Change struct {
	// Approval: il deploy attende l'approvazione del change nel sistema ITSM.
	Approval bool `json:"approval,omitempty"`
	// Timeout dell'attesa dell'approvazione.
	Timeout time.Duration `json:"timeout,omitempty"`
}

// Window è una finestra di freeze: assoluta (From-To) o ricorrente (Cron per Duration).
type Window struct {
	Name         string
	Reason       string
	Applications []string
	Environments []string
	// Regions, se presente, limita la finestra ad alcune region: vale per i passi di un
	// rollout su quelle region, non per l'accodamento del deploy.
	Regions  []string
	From, To time.Time
	// Cron è l'inizio di ogni occorrenza (5 campi), interpretato in Timezone.
	Cron     string
	Duration time.Duration
	Timezone string

	schedule cron.Schedule
}

// WindowStatus descrive una finestra di freeze rispetto a un istante.
type WindowStatus struct {
	Name         string    `json:"name"`
	Reason       string    `json:"reason,omitempty"`
	Applications []string  `json:"applications"`
	Environments []string  `json:"environments"`
	Regions      []string  `json:"regions,omitempty"`
	Active       bool      `json:"active"`
	Start        time.Time `json:"start,omitzero"`
	End          time.Time `json:"end,omitzero"`
	Recurring    bool      `json:"recurring,omitempty"`
}

// Policy contiene regole e finestre di freeze.
type Policy struct {
	Rules   []Rule
	Freezes []Window
}

// New verifica regole e finestre e prepara le espressioni cron.
func New(rules []Rule, windows []Window) (*Policy, error) {
	var errs []error
	for i, r := range rules {
		if len(r.Applications) == 0 || len(r.Environments) == 0 {
			errs = append(errs, fmt.Errorf("governance.rules[%d]: applications ed environments obbligatori (\"*\" per tutti)", i))
		}
		if r.Approvals.Required < 0 {
			errs = append(errs, fmt.Errorf("governance.rules[%d].approvals.required deve essere >= 0", i))
		}
		if r.Approvals.Required > 0 && r.Approvals.Timeout <= 0 {
			rules[i].Approvals.Timeout = DefaultApprovalTimeout
		}
	}
	for i := range windows {
		w := &windows[i]
		if w.Name == "" || len(w.Applications) == 0 || len(w.Environments) == 0 {
			errs = append(errs, fmt.Errorf("governance.freezes[%d]: name, applications ed environments obbligatori", i))
		}
		switch {
		case w.Cron != "" && (!w.From.IsZero() || !w.To.IsZero()):
			errs = append(errs, fmt.Errorf("governance.freezes[%d]: from/to e cron sono alternativi", i))
		case w.Cron != "":
			if w.Duration <= 0 {
				errs = append(errs, fmt.Errorf("governance.freezes[%d]: duration obbligatoria con cron", i))
			}
			expr := w.Cron
			if w.Timezone != "" {
				expr = "CRON_TZ=" + w.Timezone + " " + expr
			}
			s, err := cron.ParseStandard(expr)
			if err != nil {
				errs = append(errs, fmt.Errorf("governance.freezes[%d].cron non valido: %w", i, err))
			}
			w.schedule = s
		case w.From.IsZero() || w.To.IsZero() || !w.To.After(w.From):
			errs = append(errs, fmt.Errorf("governance.freezes[%d]: indicare from e to (to successivo a from) oppure cron e duration", i))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &Policy{Rules: rules, Freezes: windows}, nil
}

// Rule restituisce la prima regola che corrisponde all'applicazione e all'ambiente, o una
// regola vuota (nessun vincolo).
func (p *Policy) Rule(application, environment string) Rule {
	if p == nil {
		return Rule{}
	}
	for _, r := range p.Rules {
		if glob.Any(r.Applications, application) && glob.Any(r.Environments, environment) {
			return r
		}
	}
	return Rule{}
}

// Frozen restituisce la prima finestra di freeze attiva all'istante t, o nil.
func (p *Policy) Frozen(application, environment string, t time.Time) *WindowStatus {
	return p.FrozenIn(application, environment, "", t)
}

// FrozenIn restituisce il freeze attivo per l'applicazione e l'ambiente in una region: le
// finestre senza region valgono ovunque, quelle con region solo lì. Con region "" valgono
// solo le finestre senza region.
func (p *Policy) FrozenIn(application, environment, region string, t time.Time) *WindowStatus {
	if p == nil {
		return nil
	}
	for _, w := range p.Freezes {
		if !w.applies(application, environment) {
			continue
		}
		if len(w.Regions) > 0 && (region == "" || !glob.Any(w.Regions, region)) {
			continue
		}
		if st := w.status(t); st.Active {
			return &st
		}
	}
	return nil
}

// Windows restituisce lo stato delle finestre che riguardano l'applicazione e l'ambiente
// (tutte se vuoti): quelle attive e quelle future, con la prossima occorrenza.
func (p *Policy) Windows(application, environment string, t time.Time) []WindowStatus {
	if p == nil {
		return nil
	}
	out := []WindowStatus{}
	for _, w := range p.Freezes {
		if (application != "" || environment != "") && !w.applies(application, environment) {
			continue
		}
		st := w.status(t)
		if st.Active || st.Start.After(t) {
			out = append(out, st)
		}
	}
	return out
}

// applies indica se la finestra riguarda l'applicazione e l'ambiente. Un valore vuoto
// corrisponde a qualsiasi pattern, per gli elenchi filtrati solo per uno dei due.
func (w Window) applies(application, environment string) bool {
	return (application == "" || glob.Any(w.Applications, application)) &&
		(environment == "" || glob.Any(w.Environments, environment))
}

// status calcola l'occorrenza attiva all'istante t o, se nessuna, la successiva.
func (w Window) status(t time.Time) WindowStatus {
	st := WindowStatus{Name: w.Name, Reason: w.Reason, Applications: w.Applications, Environments: w.Environments, Regions: w.Regions}
	if w.schedule == nil {
		st.Start, st.End = w.From, w.To
		st.Active = !t.Before(w.From) && t.Before(w.To)
		return st
	}
	st.Recurring = true
	// L'occorrenza attiva è quella iniziata nell'ultima Duration: Next restituisce il primo
	// inizio strettamente successivo all'istante indicato.
	start := w.schedule.Next(t.Add(-w.Duration))
	if !start.After(t) {
		st.Active, st.Start, st.End = true, start, start.Add(w.Duration)
		return st
	}
	st.Start, st.End = start, start.Add(w.Duration)
	return st
}
