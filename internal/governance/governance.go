// Package governance applies the release rules per application and environment: mandatory
// promotion from other environments, approvals and freeze windows.
package governance

import (
	"errors"
	"fmt"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/joeg-ita/manent/internal/glob"
)

// DefaultApprovalTimeout is the default duration of an approval request.
const DefaultApprovalTimeout = 72 * time.Hour

// Approvals sets the approvals required before a deploy.
type Approvals struct {
	// Required is the number of approvals by distinct identities (0 = no approval).
	Required int `json:"required"`
	// AllowSelf allows approval by whoever requested the deploy.
	AllowSelf bool `json:"allow_self,omitempty"`
	// Timeout after which the request expires and the deploy does not run.
	Timeout time.Duration `json:"timeout"`
	// When is the moment of the approvals: before (default) before the deploy; rollout moves them
	// to the rollout's approval points (approvalsBetween), if any.
	When string `json:"when,omitempty"`
}

// Moments of the approvals (Approvals.When).
const (
	ApprovalsBefore  = "before"
	ApprovalsRollout = "rollout"
)

// InRollout reports whether approvals must be asked at the rollout's approval points.
func (a *Approvals) InRollout() bool { return a != nil && a.When == ApprovalsRollout }

// RequiredOrZero returns the number of required approvals (0 without a rule).
func (a *Approvals) RequiredOrZero() int {
	if a == nil {
		return 0
	}
	return a.Required
}

// Rule are the release rules of a set of applications and environments ("*" patterns).
type Rule struct {
	Name         string
	Applications []string
	Environments []string
	// Requires lists the environments where the same artifact (name, version and sha256) must
	// already have been released successfully.
	Requires  []string
	Approvals Approvals
	// Change, if set, opens a change management ticket for every deploy.
	Change *Change
}

// Change is the change management rule of a deploy, fixed at submission.
type Change struct {
	// Approval: the deploy waits for the change approval in the ITSM.
	Approval bool `json:"approval,omitempty"`
	// Timeout of the approval wait.
	Timeout time.Duration `json:"timeout,omitempty"`
}

// Window is a freeze window: absolute (From-To) or recurring (Cron for Duration).
type Window struct {
	Name         string
	Reason       string
	Applications []string
	Environments []string
	// Regions, if set, limits the window to some regions: it applies to the rollout passes on
	// those regions, not to the deploy submission.
	Regions  []string
	From, To time.Time
	// Cron is the start of every occurrence (5 fields), interpreted in Timezone.
	Cron     string
	Duration time.Duration
	Timezone string

	schedule cron.Schedule
}

// WindowStatus describes a freeze window relative to an instant.
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

// Policy holds rules and freeze windows.
type Policy struct {
	Rules   []Rule
	Freezes []Window
}

// New checks rules and windows and prepares the cron expressions.
func New(rules []Rule, windows []Window) (*Policy, error) {
	var errs []error
	for i, r := range rules {
		if len(r.Applications) == 0 || len(r.Environments) == 0 {
			errs = append(errs, fmt.Errorf("governance.rules[%d]: applications ed environments obbligatori (\"*\" per tutti)", i))
		}
		if r.Approvals.Required < 0 {
			errs = append(errs, fmt.Errorf("governance.rules[%d].approvals.required deve essere >= 0", i))
		}
		if w := r.Approvals.When; w != "" && w != ApprovalsBefore && w != ApprovalsRollout {
			errs = append(errs, fmt.Errorf("governance.rules[%d].approvals.when %q non valido (before o rollout)", i, w))
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

// Rule returns the first rule matching the application and environment, or an empty rule
// (no constraint).
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

// Frozen returns the first freeze window active at instant t, or nil.
func (p *Policy) Frozen(application, environment string, t time.Time) *WindowStatus {
	return p.FrozenIn(application, environment, "", t)
}

// FrozenIn returns the freeze active for the application and environment in a region:
// windows without regions apply everywhere, those with regions only there. With region ""
// only windows without regions apply.
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

// Windows returns the state of the windows concerning the application and environment (all
// if empty): active and future ones, with the next occurrence.
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

// applies reports whether the window concerns the application and environment. An empty
// value matches any pattern, for listings filtered on only one of the two.
func (w Window) applies(application, environment string) bool {
	return (application == "" || glob.Any(w.Applications, application)) &&
		(environment == "" || glob.Any(w.Environments, environment))
}

// status computes the occurrence active at instant t or, if none, the next one.
func (w Window) status(t time.Time) WindowStatus {
	st := WindowStatus{Name: w.Name, Reason: w.Reason, Applications: w.Applications, Environments: w.Environments, Regions: w.Regions}
	if w.schedule == nil {
		st.Start, st.End = w.From, w.To
		st.Active = !t.Before(w.From) && t.Before(w.To)
		return st
	}
	st.Recurring = true
	// The active occurrence is the one started within the last Duration: Next returns the first
	// start strictly after the given instant.
	start := w.schedule.Next(t.Add(-w.Duration))
	if !start.After(t) {
		st.Active, st.Start, st.End = true, start, start.Add(w.Duration)
		return st
	}
	st.Start, st.End = start, start.Add(w.Duration)
	return st
}
