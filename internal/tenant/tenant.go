// Package tenant models the tenants: the internal teams that own the applications. A tenant
// lists its applications with their tags; two tenants can list applications with the same name,
// which are different releases (identity tenant/application/artifact/environment). An
// application no tenant lists belongs to the reserved tenant Untenanted. Tags of tenants and
// applications are matched by selectors (permissions, governance rules, freezes, listings).
// It depends only on light packages: the client imports it too.
package tenant

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/glob"
	"github.com/joeg-ita/manent/internal/labels"
)

// NameLabel is the label with the tenant name, available to the tenant selectors besides its
// tags.
const NameLabel = "name"

// Labels of the application selectors, besides the application tags: the application name,
// the owning tenant and the tenant tags with the TenantTagPrefix.
const (
	ApplicationLabel = "application"
	TenantLabel      = "tenant"
	TenantTagPrefix  = "tenant."
)

// ErrNotFound means a tenant, or an application owner, that does not exist in the registry.
var ErrNotFound = errors.New("tenant not found")

// Errors of Resolve.
var (
	// ErrAmbiguous means an application listed by several tenants, without a tenant chosen.
	ErrAmbiguous = errors.New("ambiguous application")
	// ErrDisabled means a disabled owner: its applications cannot be deployed.
	ErrDisabled = errors.New("tenant disabled")
)

// Untenanted is the reserved tenant of the applications no tenant lists.
const Untenanted = "untenanted"

// ValidName checks a tenant name.
func ValidName(name string) error { return descriptor.ValidTenantName(name) }

// Application is an application owned by a tenant.
type Application struct {
	Tags map[string]string `json:"tags,omitempty" yaml:"tags,omitempty"`
}

// Members are the identities of a group of the tenant: subjects in the form
// "<method>:<name>" (patterns with *) and OIDC or static token groups.
type Members struct {
	Subjects []string `json:"subjects,omitempty" yaml:"subjects,omitempty"`
	Groups   []string `json:"groups,omitempty" yaml:"groups,omitempty"`
}

// Empty reports whether no identity is listed.
func (m Members) Empty() bool { return len(m.Subjects) == 0 && len(m.Groups) == 0 }

// Tenant is a tenant of the registry: a team and the applications it owns.
type Tenant struct {
	Name        string            `json:"name" yaml:"-"`
	Description string            `json:"description,omitempty" yaml:"description,omitempty"`
	Tags        map[string]string `json:"tags,omitempty" yaml:"tags,omitempty"`
	// Applications are the applications the tenant owns, with their tags.
	Applications map[string]Application `json:"applications,omitempty" yaml:"applications,omitempty"`
	// Members and Admins receive the roles of rbac.tenant_roles on the tenant's applications.
	Members Members `json:"members,omitzero" yaml:"members,omitempty"`
	Admins  Members `json:"admins,omitzero" yaml:"admins,omitempty"`
	// Queue, if set, is the tenant's deploy queue, served by the workers that listen to it
	// (for example a dedicated pool with the tenant's credentials); empty: the default queue.
	Queue string `json:"queue,omitempty" yaml:"queue,omitempty"`
	// Concurrency, if > 0, limits the deploys running at once on the tenant's queue (quota).
	Concurrency int `json:"concurrency,omitempty" yaml:"concurrency,omitempty"`
	// SecretRefs, if set, are the patterns every secret reference of the tenant's deploys must
	// match, whoever wrote it (environment configuration or descriptor).
	SecretRefs []string `json:"secret_refs,omitempty" yaml:"secret_refs,omitempty"`
	// ArtifactSources, if set, are the URL prefixes the tenant's artifacts must come from.
	ArtifactSources []string `json:"artifact_sources,omitempty" yaml:"artifact_sources,omitempty"`
	// CallbackHosts, if set, are the only hosts the callbacks of the tenant's deploys can reach.
	CallbackHosts []string `json:"callback_hosts,omitempty" yaml:"callback_hosts,omitempty"`
	// Disabled tenants cannot deploy their applications.
	Disabled bool `json:"disabled,omitempty" yaml:"disabled,omitempty"`
	// Source and Revision identify the registry the tenant was read from (not in the YAML).
	Source   string `json:"source,omitempty" yaml:"-"`
	Revision string `json:"revision,omitempty" yaml:"-"`
}

// Registry is the document of the tenant registry (tenants.yml).
type Registry struct {
	Tenants map[string]*Tenant `yaml:"tenants"`
}

// ParseRegistry decodes the registry (YAML or JSON), rejecting unknown fields, and validates
// every tenant. It returns the tenants ordered by name; an empty document is an empty registry.
func ParseRegistry(data []byte) ([]Tenant, error) {
	var r Registry
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&r); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("invalid tenant registry: %w", err)
	}
	out := []Tenant{}
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(r.Tenants)) {
		t := r.Tenants[name]
		if t == nil {
			t = &Tenant{}
		}
		t.Name = name
		if err := t.Validate(); err != nil {
			errs = append(errs, err)
		}
		out = append(out, *t)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return out, nil
}

// Validate checks name, tags, applications and limits of the tenant.
func (t *Tenant) Validate() error {
	var errs []error
	if err := ValidName(t.Name); err != nil {
		errs = append(errs, err)
	}
	if t.Name == Untenanted {
		errs = append(errs, fmt.Errorf("tenant name %q is reserved for the applications no tenant lists", Untenanted))
	}
	if err := labels.ValidLabels(t.Tags); err != nil {
		errs = append(errs, fmt.Errorf("tenant %s: tags: %w", t.Name, err))
	}
	if _, ok := t.Tags[NameLabel]; ok {
		errs = append(errs, fmt.Errorf("tenant %s: tag %q is reserved", t.Name, NameLabel))
	}
	for _, app := range slices.Sorted(maps.Keys(t.Applications)) {
		if !descriptor.ValidArtifactName(app) {
			errs = append(errs, fmt.Errorf("tenant %s: invalid application name %q", t.Name, app))
		}
		tags := t.Applications[app].Tags
		if err := labels.ValidLabels(tags); err != nil {
			errs = append(errs, fmt.Errorf("tenant %s: applications.%s.tags: %w", t.Name, app, err))
		}
		for k := range tags {
			if k == ApplicationLabel || k == TenantLabel || strings.HasPrefix(k, TenantTagPrefix) {
				errs = append(errs, fmt.Errorf("tenant %s: applications.%s.tags: tag %q is reserved", t.Name, app, k))
			}
		}
	}
	if t.Concurrency < 0 {
		errs = append(errs, fmt.Errorf("tenant %s: concurrency must be >= 0", t.Name))
	}
	if t.Queue != "" && !descriptor.ValidArtifactName(t.Queue) {
		errs = append(errs, fmt.Errorf("tenant %s: invalid queue name %q", t.Name, t.Queue))
	}
	return errors.Join(errs...)
}

// Owns reports whether the tenant owns the application.
func (t *Tenant) Owns(application string) bool {
	_, ok := t.Applications[application]
	return ok
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

// Ref returns the reference of the tenant for one of its applications.
func (t *Tenant) Ref(application string) *Ref {
	return &Ref{Name: t.Name, Tags: maps.Clone(t.Tags), Application: application, AppTags: maps.Clone(t.Applications[application].Tags)}
}

// Ref is the owner of an application as recorded in a request and passed to governance and
// permissions: the tenant with its tags and the application with its tags.
type Ref struct {
	Name        string            `json:"name"`
	Tags        map[string]string `json:"tags,omitempty"`
	Application string            `json:"application,omitempty"`
	AppTags     map[string]string `json:"app_tags,omitempty"`
}

// Labels are the tags plus the name: what the tenant selectors match.
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

// AppLabels are what the application selectors match: the application tags, its name
// ("application"), the tenant ("tenant") and the tenant tags with the "tenant." prefix.
func (r *Ref) AppLabels() map[string]string {
	if r == nil {
		return nil
	}
	return AppLabels(r.Application, r.AppTags, r.Name, r.Tags)
}

// AppLabels builds the labels of an application selector.
func AppLabels(application string, appTags map[string]string, tenantName string, tenantTags map[string]string) map[string]string {
	l := maps.Clone(appTags)
	if l == nil {
		l = map[string]string{}
	}
	for k, v := range tenantTags {
		l[TenantTagPrefix+k] = v
	}
	l[ApplicationLabel] = application
	l[TenantLabel] = tenantName
	return l
}

// Matches reports whether a tenant selector applies to the tenant: an empty selector applies to
// everything (tenant or not), a non-empty one only to tenants it matches. An invalid selector
// matches nothing.
func Matches(selector string, r *Ref) bool {
	return matches(selector, r, (*Ref).Labels)
}

// MatchesApp reports whether an application selector applies to the application of r, with
// the same rules as Matches.
func MatchesApp(selector string, r *Ref) bool {
	return matches(selector, r, (*Ref).AppLabels)
}

func matches(selector string, r *Ref, l func(*Ref) map[string]string) bool {
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
	return s.Matches(l(r))
}

// AppMatches reports whether an application pattern applies to an application of a tenant. A
// qualified pattern "<tenant>/<application>" (globs on both parts: "payments/*", "*/shop",
// "*/*") applies to the applications of the matching tenants; a pattern without "/" only to the
// Untenanted applications, so a wide binding written for them never reaches a tenant.
func AppMatches(pattern, tenantName, application string) bool {
	if tenantName == "" {
		tenantName = Untenanted
	}
	if tp, ap, ok := strings.Cut(pattern, "/"); ok {
		return glob.Match(tp, tenantName) && glob.Match(ap, application)
	}
	return tenantName == Untenanted && glob.Match(pattern, application)
}

// AnyApp reports whether one of the patterns applies to the application of the tenant.
func AnyApp(patterns []string, tenantName, application string) bool {
	return slices.ContainsFunc(patterns, func(p string) bool { return AppMatches(p, tenantName, application) })
}

// NameOf is the name of the tenant of a reference, Untenanted for nil.
func NameOf(r *Ref) string {
	if r == nil {
		return Untenanted
	}
	return r.Name
}

// CheckAppPattern validates an application pattern: at most one "/" between non-empty parts.
func CheckAppPattern(pattern string) error {
	tp, ap, ok := strings.Cut(pattern, "/")
	if pattern == "" || (ok && (tp == "" || ap == "" || strings.Contains(ap, "/"))) {
		return fmt.Errorf("invalid application pattern %q: <application> or <tenant>/<application>", pattern)
	}
	return nil
}

// CheckSelector validates a selector (empty is valid).
func CheckSelector(selector string) error {
	if strings.TrimSpace(selector) == "" {
		return nil
	}
	_, err := labels.Parse(selector)
	return err
}

// Directory reads the tenant registry.
type Directory interface {
	// Tenants lists all tenants, ordered by name.
	Tenants(ctx context.Context) ([]Tenant, error)
}

// Find returns a tenant of the registry; ErrNotFound if it does not exist.
func Find(ctx context.Context, d Directory, name string) (*Tenant, error) {
	l, err := d.Tenants(ctx)
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

// Owners returns the tenants that list the application, ordered by name.
func Owners(ctx context.Context, d Directory, application string) ([]Tenant, error) {
	l, err := d.Tenants(ctx)
	if err != nil {
		return nil, err
	}
	var out []Tenant
	for _, t := range l {
		if t.Owns(application) {
			out = append(out, t)
		}
	}
	return out, nil
}

// UntenantedOf returns the reserved tenant Untenanted for an application no tenant lists:
// no tags, members, queue nor restrictions.
func UntenantedOf(application string) *Tenant {
	return &Tenant{Name: Untenanted, Applications: map[string]Application{application: {}}}
}

// IsUntenanted reports whether t is the reserved tenant of the applications no tenant lists.
func (t *Tenant) IsUntenanted() bool { return t == nil || t.Name == Untenanted }

// Resolve finds the tenant of an application. chosen (the tenant field of the descriptor, or
// --tenant) only chooses among the tenants that list the application, never assigns it:
//   - chosen set: that tenant if it lists the application (Untenanted if no tenant does);
//   - chosen empty: the only tenant that lists it, Untenanted if none, ErrAmbiguous if several.
//
// Untenanted applications are refused (ErrNotFound) unless allowUntenanted; a disabled owner is
// ErrDisabled, never Untenanted. Without a registry (d nil) every application is Untenanted.
func Resolve(ctx context.Context, d Directory, application, chosen string, allowUntenanted bool) (*Tenant, error) {
	var owners []Tenant
	if d != nil {
		var err error
		if owners, err = Owners(ctx, d, application); err != nil {
			return nil, err
		}
	}
	var t *Tenant
	switch {
	case chosen != "" && chosen != Untenanted:
		for i := range owners {
			if owners[i].Name == chosen {
				t = &owners[i]
			}
		}
		if t == nil {
			return nil, fmt.Errorf("%w: tenant %s does not list application %s", ErrNotFound, chosen, application)
		}
	case len(owners) == 1 && chosen == "":
		t = &owners[0]
	case len(owners) > 1 && chosen == "":
		names := make([]string, len(owners))
		for i, o := range owners {
			names[i] = o.Name
		}
		return nil, fmt.Errorf("%w: application %s belongs to the tenants %s: choose one with tenant", ErrAmbiguous, application, strings.Join(names, ", "))
	case len(owners) > 0:
		return nil, fmt.Errorf("%w: application %s belongs to a tenant, it is not %s", ErrNotFound, application, Untenanted)
	case !allowUntenanted:
		return nil, fmt.Errorf("%w: application %s is not registered in any tenant (tenants.untenanted: reject)", ErrNotFound, application)
	default:
		return UntenantedOf(application), nil
	}
	if t.Disabled {
		return nil, fmt.Errorf("%w: tenant %s, owner of %s, is disabled", ErrDisabled, t.Name, application)
	}
	return t, nil
}

// Cache keeps the listing of a Directory for TTL, so the selectors and the permission checks
// do not read the registry every time.
type Cache struct {
	Next Directory
	TTL  time.Duration
	// OnError, if set, is called when the registry becomes unreadable or invalid (once per
	// distinct error) while the last valid listing stays in force.
	OnError func(error)

	mu      sync.Mutex
	at      time.Time
	list    []Tenant
	lastErr error
}

// Err returns the error of the last read of the registry, nil once a read succeeds: a non-nil
// value means the listing in use is an older valid one.
func (c *Cache) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastErr
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
		if c.OnError != nil && (c.lastErr == nil || c.lastErr.Error() != err.Error()) {
			c.OnError(err)
		}
		c.lastErr = err
		if c.list != nil {
			// The registry is unreadable: the last listing is better than nothing.
			return c.list, nil
		}
		return nil, err
	}
	if l == nil {
		l = []Tenant{}
	}
	c.list, c.at, c.lastErr = l, time.Now(), nil
	return l, nil
}

// File is the registry read from a YAML file (tenants.yml) at every listing (Cache limits
// the reads); the revision is a prefix of the file's sha256.
type File struct {
	Path string
}

// Tenants implements Directory.
func (f File) Tenants(context.Context) ([]Tenant, error) {
	data, err := os.ReadFile(f.Path)
	if err != nil {
		return nil, fmt.Errorf("reading the tenant registry: %w", err)
	}
	ts, err := ParseRegistry(data)
	if err != nil {
		return nil, fmt.Errorf("file:%s: %w", f.Path, err)
	}
	sum := sha256.Sum256(data)
	for i := range ts {
		ts[i].Source, ts[i].Revision = "file:"+f.Path, hex.EncodeToString(sum[:6])
	}
	return ts, nil
}
