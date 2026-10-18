package descriptor

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/joeg-ita/manent/internal/labels"
)

// tenantNameRe validates tenant names: they become part of workflow IDs, directories and keys.
var tenantNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// ValidTenantName checks a tenant name.
func ValidTenantName(name string) error {
	if !tenantNameRe.MatchString(name) {
		return fmt.Errorf("invalid tenant name %q: lowercase letters, digits and '-', at most 40", name)
	}
	return nil
}

// Behaviours of a multi-tenant release.
const (
	// TenantsOnFailureStop does not start the tenants not yet started (default).
	TenantsOnFailureStop = "stop"
	// TenantsOnFailureContinue goes on with the other tenants.
	TenantsOnFailureContinue = "continue"
	// TenantsOnDeniedSkip leaves out a tenant denied by the governance policy (default).
	TenantsOnDeniedSkip = "skip"
	// TenantsOnDeniedReject refuses the whole release if a tenant is denied.
	TenantsOnDeniedReject = "reject"
)

// Limits of a multi-tenant release.
const (
	MaxTenantsPerRun      = 1000
	DefaultTenantParallel = 5
	maxTenantGroups       = 10
)

// Tenants releases the same artifact to several tenants: one deploy per tenant, at most
// MaxParallel at a time, group by group.
type Tenants struct {
	// Names or Selector (tags, and "name" for the tenant name): exactly one of the two.
	Names    []string `json:"names,omitempty" yaml:"names,omitempty"`
	Selector string   `json:"selector,omitempty" yaml:"selector,omitempty"`
	// Groups split the tenants: a group starts when the previous one ended and its bake passed.
	// A group without select takes the remaining tenants.
	Groups []TenantGroup `json:"groups,omitempty" yaml:"groups,omitempty"`
	// MaxParallel is the number of tenant deploys running at the same time (default 5).
	MaxParallel int `json:"maxParallel,omitempty" yaml:"maxParallel,omitempty"`
	// OnFailure is stop (default) or continue.
	OnFailure string `json:"onFailure,omitempty" yaml:"onFailure,omitempty"`
	// OnDenied is skip (default: a tenant denied by the policy is left out) or reject.
	OnDenied string `json:"onDenied,omitempty" yaml:"onDenied,omitempty"`
	// ApprovalsBetween lists the groups after which the release waits for an approval.
	ApprovalsBetween []string `json:"approvalsBetween,omitempty" yaml:"approvalsBetween,omitempty"`
}

// TenantGroup is a group of tenants of a multi-tenant release.
type TenantGroup struct {
	Name   string `json:"name" yaml:"name"`
	Select string `json:"select,omitempty" yaml:"select,omitempty"`
	// Bake is the wait after the group, before the next one (e.g. "30m").
	Bake string `json:"bake,omitempty" yaml:"bake,omitempty"`
}

// BakeDuration returns the bake of the group (0 if absent or invalid, Validate rejects it).
func (g TenantGroup) BakeDuration() time.Duration {
	d, _ := time.ParseDuration(g.Bake)
	return d
}

// Parallel returns MaxParallel or the default.
func (t *Tenants) Parallel() int {
	if t.MaxParallel > 0 {
		return t.MaxParallel
	}
	return DefaultTenantParallel
}

// OnFailureOrDefault returns OnFailure or stop.
func (t *Tenants) OnFailureOrDefault() string {
	if t.OnFailure == "" {
		return TenantsOnFailureStop
	}
	return t.OnFailure
}

// OnDeniedOrDefault returns OnDenied or skip.
func (t *Tenants) OnDeniedOrDefault() string {
	if t.OnDenied == "" {
		return TenantsOnDeniedSkip
	}
	return t.OnDenied
}

// validate checks the multi-tenant release.
func (t *Tenants) validate() error {
	var errs []error
	if (len(t.Names) == 0) == (strings.TrimSpace(t.Selector) == "") {
		errs = append(errs, errors.New("tenants: give either names or selector"))
	}
	for _, n := range t.Names {
		if err := ValidTenantName(n); err != nil {
			errs = append(errs, fmt.Errorf("tenants.names: %w", err))
		}
	}
	if len(t.Names) > MaxTenantsPerRun {
		errs = append(errs, fmt.Errorf("tenants.names: at most %d", MaxTenantsPerRun))
	}
	if t.Selector != "" {
		if _, err := labels.Parse(t.Selector); err != nil {
			errs = append(errs, fmt.Errorf("tenants.selector: %w", err))
		}
	}
	if t.MaxParallel < 0 || t.MaxParallel > 100 {
		errs = append(errs, errors.New("tenants.maxParallel must be between 1 and 100"))
	}
	if !slices.Contains([]string{"", TenantsOnFailureStop, TenantsOnFailureContinue}, t.OnFailure) {
		errs = append(errs, fmt.Errorf("invalid tenants.onFailure %q: stop or continue", t.OnFailure))
	}
	if !slices.Contains([]string{"", TenantsOnDeniedSkip, TenantsOnDeniedReject}, t.OnDenied) {
		errs = append(errs, fmt.Errorf("invalid tenants.onDenied %q: skip or reject", t.OnDenied))
	}
	if len(t.Groups) > maxTenantGroups {
		errs = append(errs, fmt.Errorf("tenants.groups: at most %d", maxTenantGroups))
	}
	seen := map[string]bool{}
	for i, g := range t.Groups {
		switch {
		case !validName(g.Name):
			errs = append(errs, fmt.Errorf("tenants.groups[%d]: invalid name %q", i, g.Name))
		case seen[g.Name]:
			errs = append(errs, fmt.Errorf("tenants.groups[%d]: duplicate name %q", i, g.Name))
		}
		seen[g.Name] = true
		if g.Select != "" {
			if _, err := labels.Parse(g.Select); err != nil {
				errs = append(errs, fmt.Errorf("tenants.groups[%d].select: %w", i, err))
			}
		} else if i != len(t.Groups)-1 {
			errs = append(errs, fmt.Errorf("tenants.groups[%d]: only the last group can be without select", i))
		}
		if g.Bake != "" {
			if d, err := time.ParseDuration(g.Bake); err != nil || d < 0 {
				errs = append(errs, fmt.Errorf("tenants.groups[%d].bake: invalid duration %q", i, g.Bake))
			}
		}
	}
	for _, a := range t.ApprovalsBetween {
		if !seen[a] {
			errs = append(errs, fmt.Errorf("tenants.approvalsBetween: unknown group %q", a))
		}
	}
	return errors.Join(errs...)
}

// ValidID checks a deploy or schedule ID (the rule of the descriptor's id).
func ValidID(id string) error {
	if !validName(id) {
		return fmt.Errorf("invalid id %q: %s", id, nameRule)
	}
	return nil
}
