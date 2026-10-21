// Package tenant models the tenants: the internal teams that own the applications. Every
// application belongs to exactly one tenant, which lists it with its tags. Tags of tenants and
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
// it as a whole: every application belongs to exactly one tenant. It returns the tenants
// ordered by name; an empty document is an empty registry.
func ParseRegistry(data []byte) ([]Tenant, error) {
	var r Registry
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&r); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("invalid tenant registry: %w", err)
	}
	out := []Tenant{}
	var errs []error
	owner := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(r.Tenants)) {
		t := r.Tenants[name]
		if t == nil {
			t = &Tenant{}
		}
		t.Name = name
		if err := t.Validate(); err != nil {
			errs = append(errs, err)
		}
		for _, app := range slices.Sorted(maps.Keys(t.Applications)) {
			if prev, ok := owner[app]; ok {
				errs = append(errs, fmt.Errorf("application %s belongs to two tenants: %s and %s", app, prev, name))
			}
			owner[app] = name
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

// Owner returns the tenant that owns the application; ErrNotFound if none does.
func Owner(ctx context.Context, d Directory, application string) (*Tenant, error) {
	l, err := d.Tenants(ctx)
	if err != nil {
		return nil, err
	}
	for i := range l {
		if l[i].Owns(application) {
			t := l[i]
			return &t, nil
		}
	}
	return nil, fmt.Errorf("%w: application %s is not registered in any tenant", ErrNotFound, application)
}

// Cache keeps the listing of a Directory for TTL, so the selectors and the permission checks
// do not read the registry every time.
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
			// The registry is unreadable: the last listing is better than nothing.
			return c.list, nil
		}
		return nil, err
	}
	if l == nil {
		l = []Tenant{}
	}
	c.list, c.at = l, time.Now()
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
