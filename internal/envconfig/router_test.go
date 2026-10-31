package envconfig

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/manent-deployer/manent/internal/descriptor"
	"github.com/manent-deployer/manent/internal/pipeline"
	"github.com/manent-deployer/manent/internal/tenant"
)

// memProvider serves the documents of its map; err, if set, fails every read.
type memProvider struct {
	name string
	docs map[string]EnvConfig
	err  error
	// revs, if set, makes it versioned: it records the revisions asked for.
	revs *[]Revision
}

func (m *memProvider) Get(ctx context.Context, k Key) (*Document, error) {
	if m.err != nil {
		return nil, m.err
	}
	key := k.Tenant + "/" + k.Application + "/" + k.Environment
	c, ok := m.docs[key]
	if !ok {
		return nil, fmt.Errorf("%w: %s:%s", ErrNotFound, m.name, key)
	}
	return &Document{Config: c, Source: m.name + ":" + key}, nil
}

type versionedProvider struct{ *memProvider }

func (v versionedProvider) GetAt(ctx context.Context, k Key, rev Revision) (*Document, error) {
	*v.revs = append(*v.revs, rev)
	doc, err := v.Get(ctx, k)
	if err == nil {
		doc.Revision = "sha-" + v.name
	}
	return doc, err
}

type tenantDir []tenant.Tenant

func (d tenantDir) Tenants(context.Context) ([]tenant.Tenant, error) { return d, nil }

func target(h string) EnvConfig { return EnvConfig{Targets: []descriptor.Target{{Host: h}}} }

func TestRouterSceltaDelProvider(t *testing.T) {
	git := &memProvider{name: "git", docs: map[string]EnvConfig{"payments/shop/prod": target("git-host")}}
	legacy := &memProvider{name: "legacy", docs: map[string]EnvConfig{"untenanted/old/test": target("legacy-host")}}
	def := &memProvider{name: "default", docs: map[string]EnvConfig{
		"payments/shop/test": target("default-host"), "untenanted/old/test": target("default-old"), "retail/web/prod": target("default-web"),
	}}
	r := &Router{
		Routes: []Route{
			{Name: "git", Provider: git, Match: Match{Applications: []string{"payments/*"}, Environments: []string{"prod"}}},
			{Name: "legacy", Provider: legacy, Match: Match{Environments: []string{"test"}}, OnMissing: OnMissingNext},
			{Name: "default", Provider: def},
		},
		Tenants: tenantDir{{Name: "retail", EnvConfig: "legacy"}},
	}
	read := func(k Key) (*Document, error) { return r.Get(context.Background(), k) }

	// The first matching route wins.
	if doc, err := read(Key{Tenant: "payments", Application: "shop", Environment: "prod"}); err != nil || doc.Provider != "git" || doc.Config.Targets[0].Host != "git-host" {
		t.Errorf("payments/shop/prod: %+v %v", doc, err)
	}
	if doc, err := read(Key{Tenant: tenant.Untenanted, Application: "old", Environment: "test"}); err != nil || doc.Provider != "legacy" {
		t.Errorf("old/test: %+v %v", doc, err)
	}
	// on_missing: next moves to the next matching route and records the skipped ones.
	doc, err := read(Key{Tenant: "payments", Application: "shop", Environment: "test"})
	if err != nil || doc.Provider != "default" || strings.Join(doc.Skipped, ",") != "legacy" {
		t.Errorf("fallback: %+v %v", doc, err)
	}
	// The default on_missing (fail) stops at the route: no configuration, not the next one.
	if _, err := read(Key{Tenant: "payments", Application: "shop", Environment: "prod-eu"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing in default: %v", err)
	}
	gitOnly := &Router{Routes: []Route{{Name: "git", Provider: git}, {Name: "default", Provider: def}}}
	if _, err := gitOnly.Get(context.Background(), Key{Tenant: "retail", Application: "web", Environment: "prod"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("on_missing fail moved on: %v", err)
	}
	// The tenant's route comes first, also outside its match, then the matching ones.
	doc, err = read(Key{Tenant: "retail", Application: "web", Environment: "prod"})
	if err != nil || doc.Provider != "default" || strings.Join(doc.Skipped, ",") != "legacy" {
		t.Errorf("tenant route: %+v %v", doc, err)
	}
	// A tenant-only route is never chosen by the match rules.
	teamOnly := &Router{Routes: []Route{{Name: "team", Provider: git, TenantOnly: true}, {Name: "default", Provider: def}},
		Tenants: tenantDir{{Name: "payments", EnvConfig: "team"}}}
	if doc, err := teamOnly.Get(context.Background(), Key{Tenant: "payments", Application: "shop", Environment: "prod"}); err != nil || doc.Provider != "team" {
		t.Errorf("tenant-only route for its tenant: %+v %v", doc, err)
	}
	if doc, err := teamOnly.Get(context.Background(), Key{Tenant: "retail", Application: "web", Environment: "prod"}); err != nil || doc.Provider != "default" {
		t.Errorf("tenant-only route for another tenant: %+v %v", doc, err)
	}
	// A tenant naming a provider that does not exist: permanent error.
	r.Tenants = tenantDir{{Name: "retail", EnvConfig: "nope"}}
	if _, err := read(Key{Tenant: "retail", Application: "web", Environment: "prod"}); err == nil || pipeline.Retryable(err) || !strings.Contains(err.Error(), `"nope"`) {
		t.Errorf("unknown tenant provider: %v", err)
	}
	// No route matches: no configuration.
	none := &Router{Routes: []Route{{Name: "git", Provider: git, Match: Match{Environments: []string{"prod"}}}}}
	if _, err := none.Get(context.Background(), Key{Tenant: tenant.Untenanted, Application: "a", Environment: "dev"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("no route: %v", err)
	}
}

func TestRouterErroriNonPassanoAlSuccessivo(t *testing.T) {
	down := &memProvider{name: "consul", err: errors.New("dial tcp: connection refused")}
	def := &memProvider{name: "default", docs: map[string]EnvConfig{"untenanted/a/test": target("h")}}
	r := &Router{Routes: []Route{{Name: "consul", Provider: down, OnMissing: OnMissingNext}, {Name: "default", Provider: def}}}
	if _, err := r.Get(context.Background(), Key{Tenant: tenant.Untenanted, Application: "a", Environment: "test"}); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("an unreachable provider must not fall back to the next one: %v", err)
	}
}

func TestRouterRevisioni(t *testing.T) {
	var revsA, revsB []Revision
	a := versionedProvider{&memProvider{name: "a", docs: map[string]EnvConfig{"untenanted/app/prod": target("a")}, revs: &revsA}}
	b := versionedProvider{&memProvider{name: "b", docs: map[string]EnvConfig{"untenanted/app/prod": target("b")}, revs: &revsB}}
	plain := &memProvider{name: "plain", docs: map[string]EnvConfig{"untenanted/app/prod": target("p")}}
	k := Key{Tenant: tenant.Untenanted, Application: "app", Environment: "prod"}

	// A pinned revision is read from the provider that recorded it, not from the first route.
	r := &Router{Routes: []Route{{Name: "a", Provider: a}, {Name: "b", Provider: b}}}
	ctx := WithRevision(context.Background(), "b", "sha-b")
	doc, err := r.GetAt(ctx, k, Revision{Ref: "sha-b", Pinned: true})
	if err != nil || doc.Provider != "b" || len(revsA) != 0 || len(revsB) != 1 {
		t.Errorf("pinned on b: %+v %v (a %v, b %v)", doc, err, revsA, revsB)
	}
	// Recorded by a provider no longer configured: permanent error.
	ctx = WithRevision(context.Background(), "gone", "sha-x")
	if _, err := r.GetAt(ctx, k, Revision{Ref: "sha-x", Pinned: true}); err == nil || pipeline.Retryable(err) {
		t.Errorf("pinned on a removed provider: %v", err)
	}
	// A revision is always recorded with its provider: without it, permanent error.
	ctx = WithRevision(context.Background(), "", "sha-a")
	if _, err := r.GetAt(ctx, k, Revision{Ref: "sha-a", Pinned: true}); err == nil || pipeline.Retryable(err) {
		t.Errorf("pinned without provider: %v", err)
	}
	// A requested revision (config.ref) needs a versioned provider.
	rp := &Router{Routes: []Route{{Name: "plain", Provider: plain}}}
	if _, err := rp.GetAt(context.Background(), k, Revision{Ref: "refs/tags/x"}); err == nil || pipeline.Retryable(err) || !strings.Contains(err.Error(), "plain is not versioned") {
		t.Errorf("config.ref on a non-versioned provider: %v", err)
	}
	// A pinned revision is ignored by non-versioned providers.
	if doc, err := rp.GetAt(WithRevision(context.Background(), "plain", "x"), k, Revision{Ref: "x", Pinned: true}); err != nil || doc.Provider != "plain" {
		t.Errorf("pinned on a non-versioned provider: %+v %v", doc, err)
	}
}

func TestResolverRegistraIlProvider(t *testing.T) {
	legacy := &memProvider{name: "legacy", docs: map[string]EnvConfig{}}
	def := &memProvider{name: "default", docs: map[string]EnvConfig{"untenanted/myapp/prod": target("h1")}}
	res := Resolver{Provider: &Router{Routes: []Route{{Name: "legacy", Provider: legacy, OnMissing: OnMissingNext}, {Name: "default", Provider: def}}}}
	d := baseDescriptor()
	_, meta, err := res.Resolve(context.Background(), d)
	if err != nil || meta["provider"] != "default" || meta["skipped"] != "legacy" || meta["source"] != "default:untenanted/myapp/prod" {
		t.Errorf("meta: %v %v", meta, err)
	}
}
