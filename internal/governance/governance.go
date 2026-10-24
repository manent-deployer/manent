// Package governance applies the release rules per application and environment: mandatory
// promotion from other environments, approvals and freeze windows.
package governance

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/manent-deployer/manent/internal/glob"
	"github.com/manent-deployer/manent/internal/labels"
	tenantpkg "github.com/manent-deployer/manent/internal/tenant"
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
	// Approvers, if set, limits who can approve besides the deployment.approve permission.
	Approvers *Approvers `json:"approvers,omitempty"`
}

// Approvers are the identities allowed to approve a deploy: an approver must match one of the
// subjects ("*" patterns on "<method>:<name>") or belong to one of the groups.
type Approvers struct {
	Groups   []string `json:"groups,omitempty"`
	Subjects []string `json:"subjects,omitempty"`
}

// Allows reports whether the identity (subject and groups) can approve. A nil or empty set
// allows everyone.
func (a *Approvers) Allows(subject string, groups []string) bool {
	if a == nil || (len(a.Groups) == 0 && len(a.Subjects) == 0) {
		return true
	}
	if glob.Any(a.Subjects, subject) {
		return true
	}
	for _, g := range groups {
		if glob.Any(a.Groups, g) {
			return true
		}
	}
	return false
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
	// Tenants, if set, is a selector on the tenant (tags and "name"): the rule applies only to
	// the deploys of the matching tenants.
	Tenants string
	// Artifacts, if set, are patterns on artifact.name: the rule applies only to those artifacts.
	Artifacts []string
	// Selector, if set, is an application selector (application tags, application, tenant,
	// tenant.<tag>): the rule applies only to the matching applications.
	Selector string
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
	Regions []string
	// Tenants, if set, is a selector limiting the window to the deploys of matching tenants.
	Tenants string
	// Artifacts, if set, are patterns on artifact.name limiting the window to those artifacts.
	Artifacts []string
	// Selector, if set, is an application selector limiting the window to the matching
	// applications.
	Selector string
	From, To time.Time
	// Cron is the start of every occurrence (5 fields), interpreted in Timezone.
	Cron     string
	Duration time.Duration
	Timezone string
	// Hard windows cannot be exempted by the policy nor overridden with freezeOverride.
	Hard bool
	// CreatedBy and CreatedAt describe a window created at runtime (API or CLI).
	CreatedBy string
	CreatedAt time.Time

	schedule cron.Schedule
}

// WindowStatus describes a freeze window relative to an instant.
type WindowStatus struct {
	Name         string    `json:"name"`
	Reason       string    `json:"reason,omitempty"`
	Applications []string  `json:"applications"`
	Environments []string  `json:"environments"`
	Regions      []string  `json:"regions,omitempty"`
	Tenants      string    `json:"tenants,omitempty"`
	Artifacts    []string  `json:"artifacts,omitempty"`
	Selector     string    `json:"selector,omitempty"`
	Active       bool      `json:"active"`
	Start        time.Time `json:"start,omitzero"`
	End          time.Time `json:"end,omitzero"`
	Recurring    bool      `json:"recurring,omitempty"`
	Hard         bool      `json:"hard,omitempty"`
	// Dynamic marks a window created at runtime, with its author.
	Dynamic   bool   `json:"dynamic,omitempty"`
	CreatedBy string `json:"created_by,omitempty"`
}

// Policy holds rules and freeze windows.
type Policy struct {
	Rules   []Rule
	Freezes []Window
}

// New checks rules and windows and prepares the cron expressions.
func New(rules []Rule, windows []Window) (*Policy, error) {
	errs := checkRules(rules)
	errs = append(errs, PrepareWindows(windows, "governance.freezes")...)
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &Policy{Rules: rules, Freezes: windows}, nil
}

// checkRules checks the rules and sets the default approval timeout.
func checkRules(rules []Rule) []error {
	var errs []error
	for i, r := range rules {
		if len(r.Applications) == 0 || len(r.Environments) == 0 {
			errs = append(errs, fmt.Errorf("governance.rules[%d]: applications and environments are required (\"*/*\" for all)", i))
		}
		for _, a := range r.Applications {
			if err := tenantpkg.CheckAppPattern(a); err != nil {
				errs = append(errs, fmt.Errorf("governance.rules[%d].applications: %w", i, err))
			}
		}
		if r.Approvals.Required < 0 {
			errs = append(errs, fmt.Errorf("governance.rules[%d].approvals.required must be >= 0", i))
		}
		if w := r.Approvals.When; w != "" && w != ApprovalsBefore && w != ApprovalsRollout {
			errs = append(errs, fmt.Errorf("invalid governance.rules[%d].approvals.when %q (before or rollout)", i, w))
		}
		if r.Tenants != "" {
			if _, err := labels.Parse(r.Tenants); err != nil {
				errs = append(errs, fmt.Errorf("governance.rules[%d].tenants: %w", i, err))
			}
		}
		if r.Selector != "" {
			if _, err := labels.Parse(r.Selector); err != nil {
				errs = append(errs, fmt.Errorf("governance.rules[%d].selector: %w", i, err))
			}
		}
		if r.Approvals.Required > 0 && r.Approvals.Timeout <= 0 {
			rules[i].Approvals.Timeout = DefaultApprovalTimeout
		}
	}
	return errs
}

// PrepareWindows checks the windows and prepares their cron expressions; field is the
// configuration path used in the errors.
func PrepareWindows(windows []Window, field string) []error {
	var errs []error
	for i := range windows {
		w := &windows[i]
		if w.Name == "" || len(w.Applications) == 0 || len(w.Environments) == 0 {
			errs = append(errs, fmt.Errorf("%s[%d]: name, applications and environments are required", field, i))
		}
		for _, a := range w.Applications {
			if err := tenantpkg.CheckAppPattern(a); err != nil {
				errs = append(errs, fmt.Errorf("%s[%d].applications: %w", field, i, err))
			}
		}
		if w.Tenants != "" {
			if _, err := labels.Parse(w.Tenants); err != nil {
				errs = append(errs, fmt.Errorf("%s[%d].tenants: %w", field, i, err))
			}
		}
		if w.Selector != "" {
			if _, err := labels.Parse(w.Selector); err != nil {
				errs = append(errs, fmt.Errorf("%s[%d].selector: %w", field, i, err))
			}
		}
		switch {
		case w.Cron != "" && (!w.From.IsZero() || !w.To.IsZero()):
			errs = append(errs, fmt.Errorf("%s[%d]: from/to and cron are alternatives", field, i))
		case w.Cron != "":
			if w.Duration <= 0 {
				errs = append(errs, fmt.Errorf("%s[%d]: duration is required with cron", field, i))
			}
			expr := w.Cron
			if w.Timezone != "" {
				expr = "CRON_TZ=" + w.Timezone + " " + expr
			}
			s, err := cron.ParseStandard(expr)
			if err != nil {
				errs = append(errs, fmt.Errorf("invalid %s[%d].cron: %w", field, i, err))
			}
			w.schedule = s
		case w.From.IsZero() || w.To.IsZero() || !w.To.After(w.From):
			errs = append(errs, fmt.Errorf("%s[%d]: provide from and to (to after from) or cron and duration", field, i))
		}
	}
	return errs
}

// Rule returns the first rule matching the application and environment, or an empty rule
// (no constraint).
func (p *Policy) Rule(application, environment string) Rule {
	return p.RuleFor(application, "", environment, nil, nil)
}

// RuleFor is Rule for an artifact of an application owned by a tenant (tenant: tags plus name,
// nil without tenant; app: the application selector labels): a rule with tenants applies only
// to the matching tenants, one with artifacts only to those artifacts, one with a selector only
// to the matching applications.
func (p *Policy) RuleFor(application, artifact, environment string, tenant, app map[string]string) Rule {
	if p == nil {
		return Rule{}
	}
	for _, r := range p.Rules {
		if tenantpkg.AnyApp(r.Applications, tenantName(tenant), application) && glob.Any(r.Environments, environment) && matchesTenant(r.Tenants, tenant) &&
			(len(r.Artifacts) == 0 || glob.Any(r.Artifacts, artifact)) && matchesTenant(r.Selector, app) {
			return r
		}
	}
	return Rule{}
}

// tenantName is the name in the tenant labels, Untenanted without.
func tenantName(labels map[string]string) string {
	if n := labels[tenantpkg.NameLabel]; n != "" {
		return n
	}
	return tenantpkg.Untenanted
}

// matchesTenant reports whether a tenant selector applies: an empty one to every deploy, a
// non-empty one only to the matching tenants.
func matchesTenant(selector string, tenant map[string]string) bool {
	if selector == "" {
		return true
	}
	if tenant == nil {
		return false
	}
	s, err := labels.Parse(selector)
	return err == nil && s.Matches(tenant)
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
	if active := ActiveWindows(p.Freezes, application, environment, region, t); len(active) > 0 {
		return &active[0]
	}
	return nil
}

// Windows returns the state of the windows concerning the application and environment (all
// if empty): active and future ones, with the next occurrence.
func (p *Policy) Windows(application, environment string, t time.Time) []WindowStatus {
	if p == nil {
		return nil
	}
	return ListWindows(p.Freezes, application, environment, t)
}

// ActiveWindows returns the windows of ws active at t for the application and environment in
// a region, in order (see FrozenIn for the region semantics).
func ActiveWindows(ws []Window, application, environment, region string, t time.Time) []WindowStatus {
	return ActiveWindowsFor(ws, application, "", environment, region, nil, nil, t)
}

// ActiveWindowsFor is ActiveWindows for an artifact of an application owned by a tenant
// (tenant: tags plus name, nil without tenant; app: the application selector labels): a window
// with tenants applies only to the matching tenants, one with artifacts only to those
// artifacts, one with a selector only to the matching applications. When the artifact or the
// application labels are unknown a partial freeze still freezes the environment.
func ActiveWindowsFor(ws []Window, application, artifact, environment, region string, tenant, app map[string]string, t time.Time) []WindowStatus {
	var out []WindowStatus
	for _, w := range ws {
		if !w.appliesTo(tenantName(tenant), application, environment) || !matchesTenant(w.Tenants, tenant) ||
			(artifact != "" && len(w.Artifacts) > 0 && !glob.Any(w.Artifacts, artifact)) ||
			(app != nil && !matchesTenant(w.Selector, app)) {
			continue
		}
		if len(w.Regions) > 0 && (region == "" || !glob.Any(w.Regions, region)) {
			continue
		}
		if st := w.status(t); st.Active {
			out = append(out, st)
		}
	}
	return out
}

// ListWindows returns the active and future windows of ws concerning the application and
// environment (all if empty), with the next occurrence.
func ListWindows(ws []Window, application, environment string, t time.Time) []WindowStatus {
	out := []WindowStatus{}
	for _, w := range ws {
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

// applies reports whether the window concerns the application, of any tenant, and the
// environment. An empty value matches any pattern, for listings filtered on only one of the two.
func (w Window) applies(application, environment string) bool {
	appOK := application == ""
	for _, p := range w.Applications {
		_, ap, ok := strings.Cut(p, "/")
		if !ok {
			ap = p
		}
		appOK = appOK || glob.Match(ap, application)
	}
	return appOK && (environment == "" || glob.Any(w.Environments, environment))
}

// appliesTo reports whether the window concerns the application of the tenant and the
// environment; patterns are qualified as in tenant.AppMatches.
func (w Window) appliesTo(tenantName, application, environment string) bool {
	return (application == "" || tenantpkg.AnyApp(w.Applications, tenantName, application)) &&
		(environment == "" || glob.Any(w.Environments, environment))
}

// status computes the occurrence active at instant t or, if none, the next one.
func (w Window) status(t time.Time) WindowStatus {
	st := WindowStatus{Name: w.Name, Reason: w.Reason, Applications: w.Applications, Environments: w.Environments, Regions: w.Regions, Tenants: w.Tenants, Artifacts: w.Artifacts, Selector: w.Selector,
		Hard: w.Hard, Dynamic: w.CreatedBy != "", CreatedBy: w.CreatedBy}
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
