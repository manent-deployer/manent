// Package envconfig fetches from a configuration management system (e.g. Consul) the deploy
// configuration of an application in an environment and merges it with the descriptor.
package envconfig

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/pipeline"
	"github.com/joeg-ita/manent/internal/tenant"
)

// ErrNotFound means the application/environment has no configuration.
var ErrNotFound = errors.New("environment configuration not found")

// EnvConfig is the deploy configuration of an environment: plugin, targets and plugin
// parameters. It must not contain secret values, only references: DBOS persists it as step output.
type EnvConfig struct {
	Policy  string                 `json:"policy,omitempty" yaml:"policy,omitempty"`
	Plugin  string                 `json:"plugin,omitempty" yaml:"plugin,omitempty"`
	Targets []descriptor.Target    `json:"targets,omitempty" yaml:"targets,omitempty"`
	Params  map[string]string      `json:"params,omitempty" yaml:"params,omitempty"`
	Secrets []descriptor.SecretRef `json:"secrets,omitempty" yaml:"secrets,omitempty"`
	// Rollback imposes the environment's automatic rollback (e.g. always on in production),
	// within the artifact type policy.
	Rollback *descriptor.Rollback `json:"rollback,omitempty" yaml:"rollback,omitempty"`
	// Rollout is the environment's wave release strategy.
	Rollout *descriptor.Rollout `json:"rollout,omitempty" yaml:"rollout,omitempty"`
	// StepParams are the parameters of custom steps, per step.
	StepParams map[string]map[string]string `json:"step_params,omitempty" yaml:"step_params,omitempty"`
	// Traffic is the integration with load balancers and routers: target draining and traffic
	// shifting between slots.
	Traffic *descriptor.TrafficConfig `json:"traffic,omitempty" yaml:"traffic,omitempty"`
	// Tenancy says whether deploys have a tenant: untenanted (default), tenanted or both.
	Tenancy string `json:"tenancy,omitempty" yaml:"tenancy,omitempty"`
	// TenantParams declares the parameters every tenant provides (tenant registry).
	TenantParams map[string]TenantParam `json:"tenant_params,omitempty" yaml:"tenant_params,omitempty"`
	// Imports are the releases in service (only current) whose exports give targets
	// (TargetsFrom) and plugin parameters (ParamsFrom) to the environment's deploys: e.g. the
	// VMs created by the infrastructure deploy. They are resolved when every deploy starts.
	Imports     map[string]descriptor.Import    `json:"imports,omitempty" yaml:"imports,omitempty"`
	TargetsFrom []descriptor.ExportRef          `json:"targets_from,omitempty" yaml:"targets_from,omitempty"`
	ParamsFrom  map[string]descriptor.ExportRef `json:"params_from,omitempty" yaml:"params_from,omitempty"`
	// Exports are the outputs of the environment's deploys that other deploys can import.
	Exports map[string]descriptor.Export `json:"exports,omitempty" yaml:"exports,omitempty"`
}

// Document is a configuration read from the provider, with the metadata for the audit.
type Document struct {
	Config EnvConfig
	// Source identifies the configuration in the provider (e.g. Consul key).
	Source string
	// Revision is the configuration version (e.g. Consul ModifyIndex), if available.
	Revision string
}

// Provider reads the configuration of an artifact of an application in an environment.
// Templates without {artifact} ignore the artifact: one document per application/environment.
type Provider interface {
	Get(ctx context.Context, application, artifact, environment string) (*Document, error)
}

// Parse decodes a YAML or JSON configuration, rejecting unknown fields.
func Parse(data []byte) (EnvConfig, error) {
	var c EnvConfig
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("invalid environment configuration: %w", err)
	}
	return c, nil
}

// expandKey replaces {application}, {artifact} and {environment} in the key or path template.
func expandKey(tmpl, application, artifact, environment string) string {
	return strings.NewReplacer("{application}", application, "{artifact}", artifact, "{environment}", environment).Replace(tmpl)
}

// readKey expands the template and reads it with read. An artifact name that cannot be part
// of a key or path is a permanent error when the template uses it.
func readKey(tmpl, application, artifact, environment string, read func(key string) (*Document, error)) (*Document, error) {
	if strings.Contains(tmpl, "{artifact}") && !descriptor.ValidArtifactName(artifact) {
		return nil, pipeline.Permanent(fmt.Errorf("invalid artifact.name %q for the environment configuration key: %s", artifact, descriptor.ArtifactNameRule))
	}
	return read(expandKey(tmpl, application, artifact, environment))
}

// Merge applies the environment configuration to the descriptor and returns the effective
// descriptor. The environment configuration is authoritative: the descriptor can only add
// what is not defined there. A value defined in both with different content is an error,
// never an override.
func Merge(d descriptor.Descriptor, c EnvConfig) (descriptor.Descriptor, error) {
	return MergeTenant(d, c, nil)
}

// MergeTenant is Merge with the tenant layer between environment and descriptor (t nil: the
// deploy has no tenant): see applyTenant.
func MergeTenant(d descriptor.Descriptor, c EnvConfig, t *tenant.Tenant) (descriptor.Descriptor, error) {
	var errs []error
	if err := checkTenancy(d, c); err != nil {
		errs = append(errs, err)
	}
	c, err := applyTenant(d, c, t)
	if err != nil {
		errs = append(errs, err)
	}
	pick := func(field, env, desc string) string {
		if env != "" && desc != "" && env != desc {
			errs = append(errs, fmt.Errorf("%s: %q in the environment configuration, %q in the descriptor", field, env, desc))
		}
		if env != "" {
			return env
		}
		return desc
	}
	d.Deploy.Policy = pick("deploy.policy", c.Policy, d.Deploy.Policy)
	d.Deploy.Plugin = pick("deploy.plugin", c.Plugin, d.Deploy.Plugin)

	if len(c.Targets) > 0 {
		if len(d.Deploy.Targets) > 0 && !slices.EqualFunc(c.Targets, d.Deploy.Targets, descriptor.Target.Equal) {
			errs = append(errs, errors.New("deploy.targets defined both in the environment configuration and in the descriptor"))
		}
		if len(d.Deploy.TargetsFrom) > 0 {
			errs = append(errs, errors.New("deploy.targetsFrom: the environment configuration already defines the targets"))
		}
		d.Deploy.Targets = slices.Clone(c.Targets)
	} else if len(c.TargetsFrom) > 0 && len(d.Deploy.Targets) > 0 {
		errs = append(errs, errors.New("deploy.targets: the environment configuration takes them from the exports of other deploys (targets_from)"))
	}
	errs = append(errs, mergeImports(&d, c)...)

	params := maps.Clone(d.Deploy.Params)
	if params == nil && len(c.Params) > 0 {
		params = map[string]string{}
	}
	for _, k := range slices.Sorted(maps.Keys(c.Params)) {
		if v, ok := params[k]; ok && v != c.Params[k] {
			errs = append(errs, fmt.Errorf("deploy.params.%s: value differs between environment configuration and descriptor", k))
		}
		params[k] = c.Params[k]
	}
	d.Deploy.Params = params

	if len(c.StepParams) > 0 {
		sp := map[string]map[string]string{}
		for step, params := range d.StepParams {
			sp[step] = maps.Clone(params)
		}
		for _, step := range slices.Sorted(maps.Keys(c.StepParams)) {
			if sp[step] == nil {
				sp[step] = map[string]string{}
			}
			for _, k := range slices.Sorted(maps.Keys(c.StepParams[step])) {
				if v, ok := sp[step][k]; ok && v != c.StepParams[step][k] {
					errs = append(errs, fmt.Errorf("stepParams.%s.%s: value differs between environment configuration and descriptor", step, k))
				}
				sp[step][k] = c.StepParams[step][k]
			}
		}
		d.StepParams = sp
	}

	secrets := slices.Clone(c.Secrets)
	for _, s := range d.Secrets {
		i := slices.IndexFunc(secrets, func(e descriptor.SecretRef) bool { return e.Name == s.Name })
		switch {
		case i < 0:
			secrets = append(secrets, s)
		case secrets[i].Ref != s.Ref:
			errs = append(errs, fmt.Errorf("secrets.%s: reference differs between environment configuration and descriptor", s.Name))
		}
	}
	d.Secrets = secrets

	if c.Rollback != nil {
		rb := descriptor.Rollback{}
		if d.Rollback != nil {
			rb = *d.Rollback
		}
		if e := c.Rollback.Enabled; e != nil {
			if rb.Enabled != nil && *rb.Enabled != *e {
				errs = append(errs, fmt.Errorf("rollback.enabled: %t in the environment configuration, %t in the descriptor", *e, *rb.Enabled))
			}
			rb.Enabled = e
		}
		if len(c.Rollback.On) > 0 {
			if len(rb.On) > 0 && !slices.Equal(rb.On, c.Rollback.On) {
				errs = append(errs, errors.New("rollback.on defined both in the environment configuration and in the descriptor"))
			}
			rb.On = slices.Clone(c.Rollback.On)
		}
		rb.Locator = pick("rollback.locator", c.Rollback.Locator, rb.Locator)
		d.Rollback = &rb
	}

	if c.Rollout != nil {
		// The environment strategy is authoritative: the descriptor can only repeat it unchanged.
		if d.Rollout != nil && !reflect.DeepEqual(*d.Rollout, *c.Rollout) {
			errs = append(errs, errors.New("rollout defined both in the environment configuration and in the descriptor"))
		}
		r := *c.Rollout
		d.Rollout = &r
	}
	// Load balancers and routers are infrastructure: only the environment configuration defines them.
	if d.Traffic != nil {
		errs = append(errs, errors.New("traffic is configured only in the environment configuration, not in the descriptor"))
	}
	d.Traffic = nil
	if c.Traffic != nil {
		if err := c.Traffic.Validate(); err != nil {
			errs = append(errs, err)
		}
		t := *c.Traffic
		d.Traffic = &t
	}
	// Targets taken from exports are added when the deploy starts (engine): only then is the
	// list checked.
	if len(d.Deploy.Targets) == 0 && len(d.Deploy.TargetsFrom) == 0 {
		errs = append(errs, errors.New("no deploy targets: neither in the environment configuration nor in the descriptor"))
	}
	if err := errors.Join(errs...); err != nil {
		return d, fmt.Errorf("environment configuration incompatible with the descriptor: %w", err)
	}
	return d, nil
}

// mergeImports adds to the descriptor imports, references and exports of the environment
// configuration. The environment can only import releases in service (current): it has no
// dependencies of its own. The same name with different content is an error.
func mergeImports(d *descriptor.Descriptor, c EnvConfig) []error {
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(c.Imports)) {
		if c.Imports[name].Deployment != "" {
			errs = append(errs, fmt.Errorf("imports.%s: the environment configuration can only import releases in service (current)", name))
		}
	}
	if len(c.Imports) > 0 || len(c.TargetsFrom) > 0 || len(c.ParamsFrom) > 0 {
		if err := descriptor.ValidateImports(c.Imports, c.TargetsFrom, c.ParamsFrom, c.Params, nil, d.Application, d.Artifact.Name, d.Environment); err != nil {
			errs = append(errs, err)
		}
	}
	if len(c.Imports) > 0 {
		imports := maps.Clone(d.Imports)
		if imports == nil {
			imports = map[string]descriptor.Import{}
		}
		for _, name := range slices.Sorted(maps.Keys(c.Imports)) {
			if v, ok := imports[name]; ok && !reflect.DeepEqual(v, c.Imports[name]) {
				errs = append(errs, fmt.Errorf("imports.%s: defined differently in the environment configuration and in the descriptor", name))
			}
			imports[name] = c.Imports[name]
		}
		d.Imports = imports
	}
	if len(c.TargetsFrom) > 0 {
		if len(d.Deploy.TargetsFrom) > 0 && !slices.Equal(d.Deploy.TargetsFrom, c.TargetsFrom) {
			errs = append(errs, errors.New("deploy.targetsFrom defined both in the environment configuration and in the descriptor"))
		}
		d.Deploy.TargetsFrom = slices.Clone(c.TargetsFrom)
	}
	if len(c.ParamsFrom) > 0 {
		pf := maps.Clone(d.Deploy.ParamsFrom)
		if pf == nil {
			pf = map[string]descriptor.ExportRef{}
		}
		for _, k := range slices.Sorted(maps.Keys(c.ParamsFrom)) {
			if v, ok := pf[k]; ok && v != c.ParamsFrom[k] {
				errs = append(errs, fmt.Errorf("deploy.paramsFrom.%s: defined differently in the environment configuration and in the descriptor", k))
			}
			if _, ok := d.Deploy.Params[k]; ok {
				errs = append(errs, fmt.Errorf("deploy.params.%s: the environment configuration takes it from an export (params_from)", k))
			}
			pf[k] = c.ParamsFrom[k]
		}
		d.Deploy.ParamsFrom = pf
	}
	if len(c.Exports) > 0 {
		if err := descriptor.ValidateExports(c.Exports); err != nil {
			errs = append(errs, err)
		}
		exports := maps.Clone(d.Exports)
		if exports == nil {
			exports = map[string]descriptor.Export{}
		}
		for _, name := range slices.Sorted(maps.Keys(c.Exports)) {
			if v, ok := exports[name]; ok && v != c.Exports[name] {
				errs = append(errs, fmt.Errorf("exports.%s: defined differently in the environment configuration and in the descriptor", name))
			}
			exports[name] = c.Exports[name]
		}
		d.Exports = exports
	}
	return errs
}

// Resolver produces the effective descriptor using the configured provider.
type Resolver struct {
	// Provider is nil when no configuration system is configured.
	Provider Provider
	// Required fails the deploy if the environment has no configuration.
	Required bool
	// Policy, if set, limits what the descriptor can add.
	Policy *DescriptorPolicy
	// Tenants is the tenant registry, read at every resolution (nil: no tenants).
	Tenants tenant.Directory
}

// resolveTenant reads the tenant of the deploy and checks it can receive it. A missing or
// unusable tenant is permanent; an unreachable registry is temporary.
func (r Resolver) resolveTenant(ctx context.Context, d descriptor.Descriptor, meta map[string]string) (*tenant.Tenant, error) {
	if d.Tenant == "" {
		return nil, nil
	}
	if r.Tenants == nil {
		return nil, pipeline.Permanent(fmt.Errorf("tenant %s: no tenant registry configured (envconfig)", d.Tenant))
	}
	t, err := r.Tenants.Tenant(ctx, d.Tenant)
	switch {
	case errors.Is(err, tenant.ErrNotFound):
		return nil, pipeline.Permanent(err)
	case err != nil:
		return nil, err
	case t.Disabled:
		return nil, pipeline.Permanent(fmt.Errorf("tenant %s is disabled", t.Name))
	case !t.Connected(d.Application, d.Environment):
		return nil, pipeline.Permanent(fmt.Errorf("tenant %s is not connected to %s/%s", t.Name, d.Application, d.Environment))
	}
	if meta != nil {
		meta["tenant"], meta["tenant_source"] = t.Name, t.Source
		if t.Revision != "" {
			meta["tenant_revision"] = t.Revision
		}
	}
	return t, nil
}

// ResolveWith merges the descriptor with the given configuration (YAML or JSON) instead of the
// provider's, with the same policy: it checks a configuration before it is applied. Every
// error is permanent.
func (r Resolver) ResolveWith(_ context.Context, d descriptor.Descriptor, config []byte) (descriptor.Descriptor, map[string]string, error) {
	cfg, err := Parse(config)
	if err != nil {
		return d, nil, pipeline.Permanent(err)
	}
	if err := r.Policy.Check(d, cfg); err != nil {
		return d, nil, pipeline.Permanent(err)
	}
	t, err := r.resolveTenant(context.Background(), d, nil)
	if err != nil {
		return d, nil, err
	}
	eff, err := MergeTenant(d, cfg, t)
	if err != nil {
		return d, nil, pipeline.Permanent(err)
	}
	return eff, map[string]string{"source": "configurazione candidata", "targets": strconv.Itoa(len(eff.Deploy.Targets))}, nil
}

// Resolve reads the environment configuration and merges it with the descriptor.
// It also returns the metadata (source, revision) to record in the deploy result.
func (r Resolver) Resolve(ctx context.Context, d descriptor.Descriptor) (descriptor.Descriptor, map[string]string, error) {
	meta := map[string]string{"source": "descriptor"}
	cfg := EnvConfig{}
	if r.Provider != nil {
		doc, err := r.Provider.Get(ctx, d.Application, d.Artifact.Name, d.Environment)
		switch {
		case errors.Is(err, ErrNotFound) && !r.Required:
			meta["source"] = "descriptor (configurazione di ambiente assente)"
		case err != nil:
			return d, nil, err
		default:
			cfg = doc.Config
			meta["source"] = doc.Source
			if doc.Revision != "" {
				meta["revision"] = doc.Revision
			}
		}
	}
	t, err := r.resolveTenant(ctx, d, meta)
	if err != nil {
		return d, nil, err
	}
	// Policy and merge depend only on descriptor, configuration and tenant: a retry would give
	// the same error.
	if err := r.Policy.Check(d, cfg); err != nil {
		return d, nil, pipeline.Permanent(err)
	}
	eff, err := MergeTenant(d, cfg, t)
	if err != nil {
		return d, nil, pipeline.Permanent(err)
	}
	meta["targets"] = strconv.Itoa(len(eff.Deploy.Targets))
	return eff, meta, nil
}
