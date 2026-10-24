package policy

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/joeg-ita/manent/internal/auth"
	"github.com/joeg-ita/manent/internal/rbac"
	"github.com/joeg-ita/manent/internal/service"
)

// Defaults of the grants cache.
const (
	DefaultAuthzCache     = 30 * time.Second
	DefaultAuthzCacheSize = 10000
)

var _ rbac.Authorizer = (*Runtime)(nil)

// grantsCache keeps the grants per revision and identity for a short time: reads never
// evaluate Rego, and a removed permission stops applying within the TTL (or at a new revision).
type grantsCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	size    int
	entries map[string]grantsEntry
}

type grantsEntry struct {
	grants  []rbac.Grant
	expires time.Time
}

func (c *grantsCache) get(key string, now time.Time) ([]rbac.Grant, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || !now.Before(e.expires) {
		return nil, false
	}
	return e.grants, true
}

func (c *grantsCache) put(key string, gs []rbac.Grant, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= c.size {
		// Full: expired entries go first, then everything (rebuilt at the next requests).
		for k, e := range c.entries {
			if !now.Before(e.expires) {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= c.size {
			clear(c.entries)
		}
	}
	c.entries[key] = grantsEntry{grants: gs, expires: now.Add(c.ttl)}
}

func (c *grantsCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// identityKey identifies an identity in the cache: subject, groups and claims.
func identityKey(p *auth.Principal) string {
	var b strings.Builder
	b.WriteString(p.String())
	gs := slices.Clone(p.Groups)
	slices.Sort(gs)
	for _, g := range gs {
		b.WriteString("\x00g")
		b.WriteString(g)
	}
	keys := make([]string, 0, len(p.Claims))
	for k := range p.Claims {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		b.WriteString("\x00c")
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(p.Claims[k])
	}
	return b.String()
}

// authzRevision returns the compiled revision with the permission policy.
func (r *Runtime) authzRevision() (*Compiled, error) {
	st, err := r.current()
	if err != nil {
		return nil, err
	}
	if st.compiled == nil || st.compiled.authz == nil {
		return nil, errors.New("rbac.engine: opa requires governance.engine: opa with a revision compiled for the permissions")
	}
	return st.compiled, nil
}

// Grants implements rbac.Authorizer: the grants of the identity in the revision in force,
// from the cache when fresh.
func (r *Runtime) Grants(ctx context.Context, p *auth.Principal) ([]rbac.Grant, error) {
	if p == nil {
		return nil, nil
	}
	c, err := r.authzRevision()
	if err != nil {
		return nil, err
	}
	now := r.now()
	key := c.Revision + "\x00" + c.SHA256 + "\x00" + identityKey(p)
	if gs, ok := r.grants.get(key, now); ok {
		r.metrics.authzEval("grants", "cached", 0)
		return gs, nil
	}
	start := time.Now()
	res, err := c.Grants(ctx, GrantsInput{Schema: AuthzInputSchema, Now: now.UTC(), Identity: IdentityOf(p)})
	if err != nil {
		r.metrics.authzEval("grants", "error", time.Since(start))
		return nil, err
	}
	r.metrics.authzEval("grants", "ok", time.Since(start))
	for _, w := range res.Warnings {
		r.metrics.authz.invalidGrants.Inc()
		r.log.Warn("policy permission discarded", "subject", p.String(), "revision", c.Revision, "detail", w)
	}
	r.grants.put(key, res.Grants, now)
	return res.Grants, nil
}

// Deny implements rbac.Authorizer: the denials of the revision in force for the change.
func (r *Runtime) Deny(ctx context.Context, req rbac.DenyRequest) ([]string, error) {
	c, err := r.authzRevision()
	if err != nil {
		return nil, err
	}
	start := time.Now()
	reasons, err := c.Deny(ctx, DenyInputOf(req, r.now()))
	outcome := "allowed"
	switch {
	case err != nil:
		outcome = "error"
	case len(reasons) > 0:
		outcome = "denied"
	}
	r.metrics.authzDeny(string(req.Action), outcome, time.Since(start))
	return reasons, err
}

// Info implements rbac.Authorizer.
func (r *Runtime) Info() (string, string) {
	return rbac.EngineOPA, r.Revision()
}

// authzMetrics are the metrics of the permission policy.
type authzMetrics struct {
	grantEvals    *prometheus.CounterVec
	denyEvals     *prometheus.CounterVec
	durations     *prometheus.HistogramVec
	invalidGrants prometheus.Counter
}

var descAuthzCache = prometheus.NewDesc("manent_authz_cache_entries", "Identities with cached permissions.", nil, nil)

func newAuthzMetrics() authzMetrics {
	return authzMetrics{
		grantEvals: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "manent_authz_grants_evaluations_total", Help: "Computations of the permissions of an identity by outcome (ok, cached, error).",
		}, []string{"outcome"}),
		denyEvals: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "manent_authz_deny_evaluations_total", Help: "Evaluations of the denials by action and outcome (allowed, denied, error).",
		}, []string{"action", "outcome"}),
		durations: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "manent_authz_eval_duration_seconds", Help: "Duration of the evaluations of the permission policy.",
			Buckets: []float64{0.00005, 0.0001, 0.0005, 0.001, 0.005, 0.01, 0.05, 0.2},
		}, []string{"part"}),
		invalidGrants: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "manent_authz_invalid_grants_total", Help: "Permissions produced by the policy and discarded because invalid or with policy.* actions.",
		}),
	}
}

func (c *collector) authzEval(part, outcome string, d time.Duration) {
	c.authz.grantEvals.WithLabelValues(outcome).Inc()
	if d > 0 {
		c.authz.durations.WithLabelValues(part).Observe(d.Seconds())
	}
}

func (c *collector) authzDeny(action, outcome string, d time.Duration) {
	c.authz.denyEvals.WithLabelValues(action, outcome).Inc()
	c.authz.durations.WithLabelValues("deny").Observe(d.Seconds())
}

// newGrantsCache creates the cache with the options of the runtime.
func newGrantsCache(o Options) *grantsCache {
	return &grantsCache{ttl: cmp.Or(o.AuthzCache, DefaultAuthzCache), size: cmp.Or(o.AuthzCacheSize, DefaultAuthzCacheSize), entries: map[string]grantsEntry{}}
}

// authzOf compiles a revision (0 = the one in force) and checks that it has the permissions.
func (r *Runtime) authzOf(ctx context.Context, revision int64) (*Compiled, error) {
	c, err := r.compiledRevision(ctx, revision)
	if err != nil {
		return nil, err
	}
	if c == nil || c.authz == nil {
		return nil, fmt.Errorf("%w: the permissions are not decided by the policy (rbac.engine is not opa)", service.ErrInvalidRequest)
	}
	return c, nil
}

// AccessRevision evaluates, without cache, the grants of an identity with a revision (0 = the
// one in force): it is the simulation of "policy access". It returns the revision label and
// the grants discarded as invalid.
func (r *Runtime) AccessRevision(ctx context.Context, revision int64, p *auth.Principal) (string, []rbac.Grant, []string, error) {
	c, err := r.authzOf(ctx, revision)
	if err != nil {
		return "", nil, nil, err
	}
	res, err := c.Grants(ctx, GrantsInput{Schema: AuthzInputSchema, Now: r.now().UTC(), Identity: IdentityOf(p)})
	if err != nil {
		return "", nil, nil, err
	}
	return c.Revision, res.Grants, res.Warnings, nil
}

// DenyRevision evaluates the denials of a revision (0 = the one in force) for a change.
func (r *Runtime) DenyRevision(ctx context.Context, revision int64, req rbac.DenyRequest) ([]string, error) {
	c, err := r.authzOf(ctx, revision)
	if err != nil {
		return nil, err
	}
	return c.Deny(ctx, DenyInputOf(req, r.now()))
}
