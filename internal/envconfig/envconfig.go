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
}

// Document is a configuration read from the provider, with the metadata for the audit.
type Document struct {
	Config EnvConfig
	// Source identifies the configuration in the provider (e.g. Consul key).
	Source string
	// Revision is the configuration version (e.g. Consul ModifyIndex), if available.
	Revision string
}

// Provider reads the configuration of an application in an environment.
type Provider interface {
	Get(ctx context.Context, application, environment string) (*Document, error)
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

// expandKey replaces {application} and {environment} in the key or path template.
func expandKey(tmpl, application, environment string) string {
	return strings.NewReplacer("{application}", application, "{environment}", environment).Replace(tmpl)
}

// Merge applies the environment configuration to the descriptor and returns the effective
// descriptor. The environment configuration is authoritative: the descriptor can only add
// what is not defined there. A value defined in both with different content is an error,
// never an override.
func Merge(d descriptor.Descriptor, c EnvConfig) (descriptor.Descriptor, error) {
	var errs []error
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
		d.Deploy.Targets = slices.Clone(c.Targets)
	}

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
	if len(d.Deploy.Targets) == 0 {
		errs = append(errs, errors.New("no deploy targets: neither in the environment configuration nor in the descriptor"))
	}
	if err := errors.Join(errs...); err != nil {
		return d, fmt.Errorf("environment configuration incompatible with the descriptor: %w", err)
	}
	return d, nil
}

// Resolver produces the effective descriptor using the configured provider.
type Resolver struct {
	// Provider is nil when no configuration system is configured.
	Provider Provider
	// Required fails the deploy if the environment has no configuration.
	Required bool
	// Policy, if set, limits what the descriptor can add.
	Policy *DescriptorPolicy
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
	eff, err := Merge(d, cfg)
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
		doc, err := r.Provider.Get(ctx, d.Application, d.Environment)
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
	// Policy and merge depend only on descriptor and configuration: a retry would give the same error.
	if err := r.Policy.Check(d, cfg); err != nil {
		return d, nil, pipeline.Permanent(err)
	}
	eff, err := Merge(d, cfg)
	if err != nil {
		return d, nil, pipeline.Permanent(err)
	}
	meta["targets"] = strconv.Itoa(len(eff.Deploy.Targets))
	return eff, meta, nil
}
