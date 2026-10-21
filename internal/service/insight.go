package service

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

// StepRecord is a recorded step of a deploy.
type StepRecord struct {
	ID          int        `json:"id"`
	Name        string     `json:"name"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	// DurationMs is the duration of the last attempt.
	DurationMs int64  `json:"duration_ms,omitempty"`
	Output     any    `json:"output,omitempty"`
	Error      string `json:"error,omitempty"`
}

// LogLine is a line of a deploy log.
type LogLine struct {
	Time    time.Time         `json:"time"`
	Level   string            `json:"level"`
	Step    string            `json:"step,omitempty"`
	Message string            `json:"message"`
	Attrs   map[string]string `json:"attrs,omitempty"`
}

// LogPage is a portion of a deploy log.
type LogPage struct {
	Lines []LogLine `json:"lines"`
	// Next is the offset to read the next lines from.
	Next int `json:"next"`
	// Done means the deploy has ended: no more lines will come.
	Done bool `json:"done"`
}

// DORAFilter selects the deploys to compute the DORA metrics on.
type DORAFilter struct {
	Application string `json:"application,omitempty"`
	Environment string `json:"environment,omitempty"`
	Tenant      string `json:"tenant,omitempty"`
	Artifact    string `json:"artifact,omitempty"`
	// ByTenant computes the metrics per tenant too.
	ByTenant bool `json:"by_tenant,omitempty"`
	// Since is the start of the period (default 30 days ago).
	Since time.Time `json:"since,omitzero"`
	// Match, if set, is an extra filter (e.g. permissions).
	Match func(Status) bool `json:"-"`
}

// DORAMetrics are the DORA metrics of an application in an environment (or of the total).
type DORAMetrics struct {
	Application string `json:"application,omitempty"`
	// Artifact is the artifact of the metrics.
	Artifact    string `json:"artifact,omitempty"`
	Environment string `json:"environment,omitempty"`
	Tenant      string `json:"tenant,omitempty"`
	// Deployments are the real deploys ended in the period (successful or failed; cancelled excluded).
	Deployments int `json:"deployments"`
	Failures    int `json:"failures"`
	// DeploymentsPerDay is the frequency of successful deploys.
	DeploymentsPerDay float64 `json:"deployments_per_day"`
	// ChangeFailureRate is the fraction of deploys that caused a failure: failed or followed by a
	// manual rollback.
	ChangeFailureRate float64 `json:"change_failure_rate"`
	// MTTRSeconds is the mean time to restore: from a failure to the next successful deploy.
	MTTRSeconds float64 `json:"mttr_seconds,omitempty"`
	// LeadTimeSeconds is the median time between a version's first successful release (in any
	// environment) and its release in this environment. It is not the lead time from the commit,
	// which manent does not know: it measures how long a version takes to be promoted.
	LeadTimeSeconds float64 `json:"lead_time_seconds,omitempty"`
}

// DORAReport holds the metrics per environment and the total.
type DORAReport struct {
	Since        time.Time     `json:"since"`
	Until        time.Time     `json:"until"`
	Total        DORAMetrics   `json:"total"`
	Environments []DORAMetrics `json:"environments"`
}

// ParseSince parses the start of a period: an RFC 3339 date, a duration (720h) or a number
// of days (30d).
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
	return time.Time{}, errors.New("since must be an RFC 3339 date, a duration (720h) or a number of days (30d)")
}
