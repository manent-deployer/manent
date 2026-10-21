package envconfig

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/tenant"
)

// Values of EnvConfig.Tenancy.
const (
	// TenancyUntenanted refuses deploys with a tenant (default).
	TenancyUntenanted = "untenanted"
	// TenancyTenanted requires a tenant.
	TenancyTenanted = "tenanted"
	// TenancyBoth accepts both.
	TenancyBoth = "both"
)

// TenantParam declares a parameter every tenant provides for the environment.
type TenantParam struct {
	// Pattern is the regular expression the whole value must match.
	Pattern  string `json:"pattern,omitempty" yaml:"pattern,omitempty"`
	Required bool   `json:"required,omitempty" yaml:"required,omitempty"`
	Default  string `json:"default,omitempty" yaml:"default,omitempty"`
}

// tenantPlaceholder is replaced with the tenant name in parameters and secret references.
const tenantPlaceholder = "{tenant}"

// checkTenancy checks the tenant of the deploy against the environment.
func checkTenancy(d descriptor.Descriptor, c EnvConfig) error {
	switch c.Tenancy {
	case "", TenancyUntenanted:
		if d.Tenant != "" {
			return fmt.Errorf("%s/%s does not accept tenants (tenancy: untenanted)", d.Application, d.Environment)
		}
	case TenancyTenanted:
		if d.Tenant == "" {
			return fmt.Errorf("%s/%s requires a tenant (tenancy: tenanted)", d.Application, d.Environment)
		}
	case TenancyBoth:
	default:
		return fmt.Errorf("invalid tenancy %q: untenanted, tenanted or both", c.Tenancy)
	}
	return nil
}

// applyTenant prepares the environment configuration for the deploy's tenant (t nil: none):
// it replaces {tenant}, keeps the targets of the tenant (dedicated and shared) and adds the
// tenant layer: the declared tenant_params, the secrets and the custom step parameters of the
// tenant for the application. A value defined twice is an error.
func applyTenant(d descriptor.Descriptor, c EnvConfig, t *tenant.Tenant) (EnvConfig, error) {
	var errs []error
	c.Params = maps.Clone(c.Params)
	c.Secrets = slices.Clone(c.Secrets)
	sp := map[string]map[string]string{}
	for step, ps := range c.StepParams {
		sp[step] = maps.Clone(ps)
	}
	c.StepParams = sp
	name := ""
	if t != nil {
		name = t.Name
	}
	expand := func(field, v string) string {
		if !strings.Contains(v, tenantPlaceholder) {
			return v
		}
		if name == "" {
			errs = append(errs, fmt.Errorf("%s uses {tenant} but the deploy has no tenant", field))
			return v
		}
		return strings.ReplaceAll(v, tenantPlaceholder, name)
	}
	for k, v := range c.Params {
		c.Params[k] = expand("params."+k, v)
	}
	for i, s := range c.Secrets {
		c.Secrets[i].Ref = expand("secrets."+s.Name, s.Ref)
	}
	for step, ps := range c.StepParams {
		for k, v := range ps {
			ps[k] = expand("step_params."+step+"."+k, v)
		}
	}
	c.Targets = tenantTargets(c.Targets, name)
	if t == nil {
		return c, errors.Join(errs...)
	}
	if len(c.Targets) == 0 && len(d.Deploy.Targets) == 0 {
		errs = append(errs, fmt.Errorf("no target for tenant %s: neither dedicated (tenant: %s) nor shared (tenant: \"*\")", name, name))
	}
	app := d.Application
	given := t.ParamsFor(app, d.Artifact.Name)
	for _, k := range slices.Sorted(maps.Keys(given)) {
		if _, ok := c.TenantParams[k]; !ok {
			errs = append(errs, fmt.Errorf("tenant %s: params.%s.%s is not declared in tenant_params of %s/%s", name, app, k, app, d.Environment))
		}
	}
	for _, k := range slices.Sorted(maps.Keys(c.TenantParams)) {
		p := c.TenantParams[k]
		v, ok := given[k]
		if !ok {
			if p.Required {
				errs = append(errs, fmt.Errorf("tenant %s: params.%s.%s is required by tenant_params", name, app, k))
				continue
			}
			v = p.Default
		}
		if p.Pattern != "" {
			re, err := regexp.Compile(`^(?:` + p.Pattern + `)$`)
			if err != nil {
				errs = append(errs, fmt.Errorf("tenant_params.%s: invalid pattern: %w", k, err))
				continue
			}
			if !re.MatchString(v) {
				errs = append(errs, fmt.Errorf("tenant %s: params.%s.%s does not match the pattern of tenant_params", name, app, k))
				continue
			}
		}
		if _, ok := c.Params[k]; ok {
			errs = append(errs, fmt.Errorf("tenant_params.%s is also a parameter of the environment configuration", k))
			continue
		}
		if _, ok := d.Deploy.Params[k]; ok {
			errs = append(errs, fmt.Errorf("deploy.params.%s is a tenant parameter: the descriptor cannot set it", k))
			continue
		}
		if c.Params == nil {
			c.Params = map[string]string{}
		}
		c.Params[k] = v
	}
	for _, s := range t.SecretsFor(app, d.Artifact.Name) {
		i := slices.IndexFunc(c.Secrets, func(e descriptor.SecretRef) bool { return e.Name == s.Name })
		switch {
		case i < 0:
			c.Secrets = append(c.Secrets, s)
		case c.Secrets[i].Ref != s.Ref:
			errs = append(errs, fmt.Errorf("tenant %s: secrets.%s differs from the environment configuration", name, s.Name))
		}
	}
	for step, ps := range t.StepParamsFor(app, d.Artifact.Name) {
		if c.StepParams[step] == nil {
			c.StepParams[step] = map[string]string{}
		}
		for k, v := range ps {
			if cur, ok := c.StepParams[step][k]; ok && cur != v {
				errs = append(errs, fmt.Errorf("tenant %s: step_params.%s.%s differs from the environment configuration", name, step, k))
				continue
			}
			c.StepParams[step][k] = v
		}
	}
	return c, errors.Join(errs...)
}

// tenantTargets keeps the targets of the tenant (dedicated with label tenant=<name> and shared
// with tenant="*"); without a tenant, those without the label and the shared ones.
func tenantTargets(ts []descriptor.Target, name string) []descriptor.Target {
	var out []descriptor.Target
	for _, t := range ts {
		v, ok := t.Labels[tenant.TargetLabel]
		if !ok || v == tenant.Shared || (name != "" && v == name) {
			out = append(out, t)
		}
	}
	return out
}

// TenantDirectory returns the tenant registry of the provider, nil if it has none.
func TenantDirectory(p Provider) tenant.Directory {
	if d, ok := p.(tenant.Directory); ok {
		return d
	}
	return nil
}

// Tenant implements tenant.Directory: the file of the tenant.
func (p *FileProvider) Tenant(_ context.Context, name string) (*tenant.Tenant, error) {
	if err := tenant.ValidName(name); err != nil {
		return nil, err
	}
	if p.TenantTemplate == "" {
		return nil, fmt.Errorf("%w: %s (no tenant registry configured)", tenant.ErrNotFound, name)
	}
	path := strings.ReplaceAll(p.TenantTemplate, tenantPlaceholder, name)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: file:%s", tenant.ErrNotFound, path)
	}
	if err != nil {
		return nil, err
	}
	t, err := tenant.Parse(data, name)
	if err != nil {
		return nil, fmt.Errorf("file:%s: %w", path, err)
	}
	t.Source = "file:" + path
	return t, nil
}

// Tenants implements tenant.Directory: the files matching the template.
func (p *FileProvider) Tenants(ctx context.Context) ([]tenant.Tenant, error) {
	if p.TenantTemplate == "" {
		return nil, nil
	}
	prefix, suffix, ok := strings.Cut(p.TenantTemplate, tenantPlaceholder)
	if !ok {
		return nil, fmt.Errorf("the tenant path template %q has no {tenant}", p.TenantTemplate)
	}
	matches, err := filepath.Glob(prefix + "*" + suffix)
	if err != nil {
		return nil, err
	}
	// Glob returns cleaned paths ("./tenants/a.yml" becomes "tenants/a.yml"): the prefix is
	// cleaned the same way before cutting the name out of a match.
	cleanPrefix := strings.TrimSuffix(filepath.Clean(prefix+"x"), "x")
	var out []tenant.Tenant
	for _, m := range slices.Sorted(slices.Values(matches)) {
		name := strings.TrimSuffix(strings.TrimPrefix(m, cleanPrefix), suffix)
		if tenant.ValidName(name) != nil {
			continue
		}
		t, err := p.Tenant(ctx, name)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, nil
}

// consulKey returns the key of the tenant.
func (p *ConsulProvider) tenantKey(name string) string {
	return strings.TrimLeft(strings.ReplaceAll(p.TenantKeyTemplate, tenantPlaceholder, name), "/")
}

// consulGet reads keys from the KV store (recursively with recurse).
func (p *ConsulProvider) consulGet(ctx context.Context, key string, recurse bool) ([]consulKV, int, error) {
	u, err := url.Parse(strings.TrimRight(p.Address, "/") + "/v1/kv/" + key)
	if err != nil {
		return nil, 0, fmt.Errorf("invalid Consul address: %w", err)
	}
	q := url.Values{}
	if p.Datacenter != "" {
		q.Set("dc", p.Datacenter)
	}
	if recurse {
		q.Set("recurse", "true")
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, 0, err
	}
	if p.Token != "" {
		req.Header.Set("X-Consul-Token", p.Token)
	}
	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("reading from Consul: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, resp.StatusCode, fmt.Errorf("reading from Consul %s: status %d: %s", key, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var kvs []consulKV
	if err := json.NewDecoder(resp.Body).Decode(&kvs); err != nil {
		return nil, 0, fmt.Errorf("invalid Consul response: %w", err)
	}
	return kvs, resp.StatusCode, nil
}

// decodeTenant decodes the value of a tenant key.
func decodeTenant(kv consulKV, name string) (*tenant.Tenant, error) {
	raw, err := base64.StdEncoding.DecodeString(kv.Value)
	if err != nil {
		return nil, fmt.Errorf("invalid Consul value: %w", err)
	}
	t, err := tenant.Parse(raw, name)
	if err != nil {
		return nil, fmt.Errorf("consul:%s: %w", kv.Key, err)
	}
	t.Source, t.Revision = "consul:"+kv.Key, strconv.FormatUint(kv.ModifyIndex, 10)
	return t, nil
}

// Tenant implements tenant.Directory.
func (p *ConsulProvider) Tenant(ctx context.Context, name string) (*tenant.Tenant, error) {
	if err := tenant.ValidName(name); err != nil {
		return nil, err
	}
	if p.TenantKeyTemplate == "" {
		return nil, fmt.Errorf("%w: %s (no tenant registry configured)", tenant.ErrNotFound, name)
	}
	kvs, code, err := p.consulGet(ctx, p.tenantKey(name), false)
	if code == http.StatusNotFound || (err == nil && (len(kvs) == 0 || kvs[0].Value == "")) {
		return nil, fmt.Errorf("%w: consul:%s", tenant.ErrNotFound, p.tenantKey(name))
	}
	if err != nil {
		return nil, err
	}
	return decodeTenant(kvs[0], name)
}

// Tenants implements tenant.Directory: the keys under the prefix of the template.
func (p *ConsulProvider) Tenants(ctx context.Context) ([]tenant.Tenant, error) {
	if p.TenantKeyTemplate == "" {
		return nil, nil
	}
	prefix, suffix, ok := strings.Cut(strings.TrimLeft(p.TenantKeyTemplate, "/"), tenantPlaceholder)
	if !ok {
		return nil, fmt.Errorf("the tenant key template %q has no {tenant}", p.TenantKeyTemplate)
	}
	kvs, code, err := p.consulGet(ctx, prefix, true)
	if code == http.StatusNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []tenant.Tenant
	for _, kv := range kvs {
		name, ok := strings.CutPrefix(kv.Key, prefix)
		if !ok || !strings.HasSuffix(name, suffix) || kv.Value == "" {
			continue
		}
		name = strings.TrimSuffix(name, suffix)
		if tenant.ValidName(name) != nil {
			continue
		}
		t, err := decodeTenant(kv, name)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	slices.SortFunc(out, func(a, b tenant.Tenant) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}
