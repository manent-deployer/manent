// Package tenant models the tenants: customers or instances to which the same application is
// released in several copies, one per tenant, in every environment. A tenant has tags, the
// environments it is connected to per application, and its parameters and secret
// references. It depends only on light packages: the client imports it too.
package tenant

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/labels"
)

// NameLabel is the label with the tenant name, available to the selectors besides its tags.
const NameLabel = "name"

// Shared is the value of the target label "tenant" for the targets shared by all tenants.
const Shared = "*"

// TargetLabel is the target label naming the tenant of a dedicated target.
const TargetLabel = "tenant"

// ErrNotFound means a tenant that does not exist in the registry.
var ErrNotFound = errors.New("tenant not found")

// ValidName checks a tenant name.
func ValidName(name string) error { return descriptor.ValidTenantName(name) }

// Tenant is a tenant of the registry.
type Tenant struct {
	Name        string            `json:"name" yaml:"name"`
	Description string            `json:"description,omitempty" yaml:"description,omitempty"`
	Tags        map[string]string `json:"tags,omitempty" yaml:"tags,omitempty"`
	// Applications maps every application to the environments the tenant is connected to.
	Applications map[string][]string `json:"applications,omitempty" yaml:"applications,omitempty"`
	// Params, Secrets and StepParams are per application: the values of the tenant_params of
	// the environment, secret references and custom step parameters. A key
	// "<application>/<artifact>" applies only to that artifact and takes priority over the
	// application's values.
	Params     map[string]map[string]string            `json:"params,omitempty" yaml:"params,omitempty"`
	Secrets    map[string][]descriptor.SecretRef       `json:"secrets,omitempty" yaml:"secrets,omitempty"`
	StepParams map[string]map[string]map[string]string `json:"step_params,omitempty" yaml:"step_params,omitempty"`
	// Disabled tenants are never selected nor deployable.
	Disabled bool `json:"disabled,omitempty" yaml:"disabled,omitempty"`
	// Source and Revision identify the document in the provider (not in the YAML).
	Source   string `json:"source,omitempty" yaml:"-"`
	Revision string `json:"revision,omitempty" yaml:"-"`
}

// Parse decodes a tenant document (YAML or JSON), rejecting unknown fields; name is the name
// expected from the key or the file.
func Parse(data []byte, name string) (*Tenant, error) {
	var t Tenant
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("invalid tenant %s: %w", name, err)
	}
	if t.Name == "" {
		t.Name = name
	}
	if t.Name != name {
		return nil, fmt.Errorf("tenant %s: the document names %q", name, t.Name)
	}
	if err := t.Validate(); err != nil {
		return nil, err
	}
	return &t, nil
}

// Validate checks name, tags and secret references.
func (t *Tenant) Validate() error {
	var errs []error
	if err := ValidName(t.Name); err != nil {
		errs = append(errs, err)
	}
	if err := labels.ValidLabels(t.Tags); err != nil {
		errs = append(errs, fmt.Errorf("tenant %s: tags: %w", t.Name, err))
	}
	if _, ok := t.Tags[NameLabel]; ok {
		errs = append(errs, fmt.Errorf("tenant %s: tag %q is reserved", t.Name, NameLabel))
	}
	for app, ss := range t.Secrets {
		for _, s := range ss {
			if s.Name == "" || s.Ref == "" {
				errs = append(errs, fmt.Errorf("tenant %s: secrets.%s: name and ref are required", t.Name, app))
			}
		}
	}
	return errors.Join(errs...)
}

// ArtifactKey is the key of the per-artifact values in Params, Secrets and StepParams.
func ArtifactKey(application, artifact string) string { return application + "/" + artifact }

// ParamsFor returns the parameters of the tenant for an artifact of the application: the
// application's, replaced by the artifact's where both set one.
func (t *Tenant) ParamsFor(application, artifact string) map[string]string {
	out := maps.Clone(t.Params[application])
	for k, v := range t.Params[ArtifactKey(application, artifact)] {
		if out == nil {
			out = map[string]string{}
		}
		out[k] = v
	}
	return out
}

// SecretsFor returns the secret references of the tenant for an artifact of the application:
// the artifact's replace the application's with the same name.
func (t *Tenant) SecretsFor(application, artifact string) []descriptor.SecretRef {
	own := t.Secrets[ArtifactKey(application, artifact)]
	out := slices.Clone(own)
	for _, s := range t.Secrets[application] {
		if !slices.ContainsFunc(own, func(o descriptor.SecretRef) bool { return o.Name == s.Name }) {
			out = append(out, s)
		}
	}
	return out
}

// StepParamsFor returns the custom step parameters of the tenant for an artifact of the
// application: the artifact's replace the application's.
func (t *Tenant) StepParamsFor(application, artifact string) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, key := range []string{application, ArtifactKey(application, artifact)} {
		for step, ps := range t.StepParams[key] {
			if out[step] == nil {
				out[step] = map[string]string{}
			}
			maps.Copy(out[step], ps)
		}
	}
	return out
}

// Labels are the tags plus the name, matched by the tenant selectors.
func (t *Tenant) Labels() map[string]string {
	l := maps.Clone(t.Tags)
	if l == nil {
		l = map[string]string{}
	}
	l[NameLabel] = t.Name
	return l
}

// Connected reports whether the tenant is connected to the application in the environment.
func (t *Tenant) Connected(application, environment string) bool {
	return slices.Contains(t.Applications[application], environment)
}

// Ref is a tenant as recorded in a request and passed to governance and permissions.
type Ref struct {
	Name string            `json:"name"`
	Tags map[string]string `json:"tags,omitempty"`
}

// Ref returns the reference of the tenant.
func (t *Tenant) Ref() *Ref { return &Ref{Name: t.Name, Tags: maps.Clone(t.Tags)} }

// Labels are the tags plus the name.
func (r *Ref) Labels() map[string]string {
	if r == nil {
		return nil
	}
	l := maps.Clone(r.Tags)
	if l == nil {
		l = map[string]string{}
	}
	l[NameLabel] = r.Name
	return l
}

// Matches reports whether a tenant selector applies to the tenant: an empty selector applies to
// everything (tenant or not), a non-empty one only to tenants it matches. An invalid selector
// matches nothing.
func Matches(selector string, r *Ref) bool {
	if strings.TrimSpace(selector) == "" {
		return true
	}
	if r == nil {
		return false
	}
	s, err := labels.Parse(selector)
	if err != nil {
		return false
	}
	return s.Matches(r.Labels())
}

// CheckSelector validates a tenant selector (empty is valid).
func CheckSelector(selector string) error {
	if strings.TrimSpace(selector) == "" {
		return nil
	}
	_, err := labels.Parse(selector)
	return err
}

// Directory reads the tenant registry.
type Directory interface {
	// Tenant reads a tenant; ErrNotFound if it does not exist.
	Tenant(ctx context.Context, name string) (*Tenant, error)
	// Tenants lists all tenants, ordered by name.
	Tenants(ctx context.Context) ([]Tenant, error)
}

// Cache keeps the listing of a Directory for TTL; single tenants are read from the listing
// while fresh, so the selectors and the permission checks do not read the provider every time.
type Cache struct {
	Next Directory
	TTL  time.Duration

	mu   sync.Mutex
	at   time.Time
	list []Tenant
}

// Tenants implements Directory.
func (c *Cache) Tenants(ctx context.Context) ([]Tenant, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.list != nil && time.Since(c.at) < c.TTL {
		return c.list, nil
	}
	l, err := c.Next.Tenants(ctx)
	if err != nil {
		if c.list != nil {
			// The provider is unreachable: the last listing is better than nothing.
			return c.list, nil
		}
		return nil, err
	}
	c.list, c.at = l, time.Now()
	return l, nil
}

// Tenant implements Directory from the cached listing.
func (c *Cache) Tenant(ctx context.Context, name string) (*Tenant, error) {
	l, err := c.Tenants(ctx)
	if err != nil {
		return nil, err
	}
	for i := range l {
		if l[i].Name == name {
			t := l[i]
			return &t, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
}

// Select returns the tenants connected to the application and environment, not disabled,
// chosen by names or by a selector (exactly one of them).
func Select(all []Tenant, application, environment string, names []string, selector string) ([]Tenant, error) {
	if (len(names) == 0) == (strings.TrimSpace(selector) == "") {
		return nil, errors.New("tenants: give either names or selector")
	}
	var sel labels.Selector
	if selector != "" {
		var err error
		if sel, err = labels.Parse(selector); err != nil {
			return nil, fmt.Errorf("tenants.selector: %w", err)
		}
	}
	byName := map[string]Tenant{}
	for _, t := range all {
		byName[t.Name] = t
	}
	var out []Tenant
	if len(names) > 0 {
		for _, n := range names {
			t, ok := byName[n]
			switch {
			case !ok:
				return nil, fmt.Errorf("%w: %s", ErrNotFound, n)
			case t.Disabled:
				return nil, fmt.Errorf("tenant %s is disabled", n)
			case !t.Connected(application, environment):
				return nil, fmt.Errorf("tenant %s is not connected to %s/%s", n, application, environment)
			}
			out = append(out, t)
		}
		return out, nil
	}
	for _, t := range all {
		if !t.Disabled && t.Connected(application, environment) && sel.Matches(t.Labels()) {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no tenant connected to %s/%s matches %q", application, environment, selector)
	}
	return out, nil
}
