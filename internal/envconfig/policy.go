package envconfig

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/glob"
)

// DescriptorPolicy limits what a descriptor can add to the environment configuration.
// Whoever can submit a descriptor is not necessarily who administers the environments:
// without limits they could, for example, run commands on the targets (post_command) or make
// the worker read secrets that are not theirs. What comes from the environment configuration
// is not subject to the policy.
type DescriptorPolicy struct {
	// Params lists, per plugin, the patterns of the parameters the descriptor can set; the "*"
	// key applies to all plugins.
	Params map[string][]string
	// SecretRefs are the patterns of the secret references the descriptor can declare.
	// They accept {application}, {artifact} and {environment}.
	SecretRefs []string
	// Targets allows targets given by the descriptor when the environment does not define them.
	Targets bool
	// StepParams lists, per custom step, the patterns of the parameters the descriptor can set;
	// the "*" key applies to all steps. Empty: none.
	StepParams map[string][]string
}

// Check verifies the descriptor against the policy, treating as already defined what comes
// from the environment configuration c.
func (p *DescriptorPolicy) Check(d descriptor.Descriptor, c EnvConfig) error {
	if p == nil {
		return nil
	}
	var errs []error
	plugin := cmp.Or(c.Plugin, d.Deploy.Plugin)
	allowed := slices.Concat(p.Params["*"], p.Params[plugin])
	for _, k := range slices.Sorted(maps.Keys(d.Deploy.Params)) {
		if _, ok := c.Params[k]; ok {
			continue
		}
		if !glob.Any(allowed, k) {
			errs = append(errs, fmt.Errorf("deploy.params.%s cannot be set by the descriptor: it must be defined in the environment configuration (descriptor_policy.params)", k))
		}
	}
	patterns := make([]string, len(p.SecretRefs))
	for i, s := range p.SecretRefs {
		patterns[i] = expandKey(s, d.Application, d.Artifact.Name, d.Environment)
	}
	for _, s := range d.Secrets {
		if slices.ContainsFunc(c.Secrets, func(e descriptor.SecretRef) bool { return e.Name == s.Name }) {
			continue
		}
		if !glob.Any(patterns, s.Ref) {
			errs = append(errs, fmt.Errorf("secrets.%s: reference not allowed in descriptors (descriptor_policy.secret_refs)", s.Name))
		}
	}
	for _, step := range slices.Sorted(maps.Keys(d.StepParams)) {
		allowed := slices.Concat(p.StepParams["*"], p.StepParams[step])
		for _, k := range slices.Sorted(maps.Keys(d.StepParams[step])) {
			if _, ok := c.StepParams[step][k]; ok {
				continue
			}
			if !glob.Any(allowed, k) {
				errs = append(errs, fmt.Errorf("stepParams.%s.%s cannot be set by the descriptor: it must be defined in the environment configuration (descriptor_policy.step_params)", step, k))
			}
		}
	}
	if !p.Targets && len(d.Deploy.Targets) > 0 && len(c.Targets) == 0 {
		errs = append(errs, errors.New("deploy.targets cannot be set by the descriptor: it must be defined in the environment configuration (descriptor_policy.targets)"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("descriptor not allowed by the policy: %w", err)
	}
	return nil
}
