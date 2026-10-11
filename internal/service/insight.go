package service

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

// StepRecord è uno step registrato di un deploy.
type StepRecord struct {
	ID          int        `json:"id"`
	Name        string     `json:"name"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	// DurationMs è la durata dell'ultimo tentativo.
	DurationMs int64  `json:"duration_ms,omitempty"`
	Output     any    `json:"output,omitempty"`
	Error      string `json:"error,omitempty"`
}

// LogLine è una riga del log di un deploy.
type LogLine struct {
	Time    time.Time         `json:"time"`
	Level   string            `json:"level"`
	Step    string            `json:"step,omitempty"`
	Message string            `json:"message"`
	Attrs   map[string]string `json:"attrs,omitempty"`
}

// LogPage è una porzione del log di un deploy.
type LogPage struct {
	Lines []LogLine `json:"lines"`
	// Next è l'offset da cui leggere le righe successive.
	Next int `json:"next"`
	// Done indica che il deploy è concluso: non arriveranno altre righe.
	Done bool `json:"done"`
}

// DORAFilter seleziona i deploy su cui calcolare le metriche DORA.
type DORAFilter struct {
	Application string `json:"application,omitempty"`
	Environment string `json:"environment,omitempty"`
	// Since è l'inizio del periodo (default 30 giorni fa).
	Since time.Time `json:"since,omitzero"`
	// Match, se presente, è un filtro aggiuntivo (es. i permessi).
	Match func(Status) bool `json:"-"`
}

// DORAMetrics sono le metriche DORA di un'applicazione in un ambiente (o del totale).
type DORAMetrics struct {
	Application string `json:"application,omitempty"`
	Environment string `json:"environment,omitempty"`
	// Deployments sono i deploy reali conclusi nel periodo (riusciti o falliti; annullati esclusi).
	Deployments int `json:"deployments"`
	Failures    int `json:"failures"`
	// DeploymentsPerDay è la frequenza dei deploy riusciti.
	DeploymentsPerDay float64 `json:"deployments_per_day"`
	// ChangeFailureRate è la frazione di deploy che hanno causato un guasto: falliti o
	// seguiti da un rollback manuale.
	ChangeFailureRate float64 `json:"change_failure_rate"`
	// MTTRSeconds è il tempo medio di ripristino: da un guasto al deploy riuscito successivo.
	MTTRSeconds float64 `json:"mttr_seconds,omitempty"`
	// LeadTimeSeconds è la mediana del tempo tra il primo rilascio riuscito di una versione
	// (in qualsiasi ambiente) e il suo rilascio in questo ambiente. Non è il lead time dal
	// commit, che manent non conosce: misura quanto una versione impiega a essere promossa.
	LeadTimeSeconds float64 `json:"lead_time_seconds,omitempty"`
}

// DORAReport contiene le metriche per ambiente e il totale.
type DORAReport struct {
	Since        time.Time     `json:"since"`
	Until        time.Time     `json:"until"`
	Total        DORAMetrics   `json:"total"`
	Environments []DORAMetrics `json:"environments"`
}

// ParseSince interpreta l'inizio di un periodo: una data RFC 3339, una durata (720h) o un
// numero di giorni (30d).
func ParseSince(v string, now time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	if days, ok := strings.CutSuffix(v, "d"); ok {
		if n, err := strconv.Atoi(days); err == nil && n > 0 {
			return now.AddDate(0, 0, -n), nil
		}
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return now.Add(-d), nil
	}
	return time.Time{}, errors.New("since deve essere una data RFC 3339, una durata (720h) o un numero di giorni (30d)")
}
