package pipeline

import (
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/manent-deployer/manent/internal/config"
	"github.com/manent-deployer/manent/internal/customspec"
	"github.com/manent-deployer/manent/internal/descriptor"
)

// PlannedStep is a step selected for execution with its retry options.
type PlannedStep struct {
	Name       string `json:"name"`
	MaxRetries int    `json:"max_retries"`
	// Type, Spec, SpecMAC and SpecHash apply only to custom steps. Spec is the spec in canonical
	// form, captured at submission: a deploy resumed after a crash runs the definition it started
	// with. SpecMAC is its signature, verified by the worker before running it.
	Type     string `json:"type,omitempty"`
	Spec     string `json:"spec,omitempty"`
	SpecMAC  string `json:"spec_mac,omitempty"`
	SpecHash string `json:"spec_hash,omitempty"`
}

// IsCustom reports a custom step.
func (p PlannedStep) IsCustom() bool { return p.Spec != "" }

// Plan is the ordered sequence of steps to run.
type Plan []PlannedStep

// Public returns the plan to show to clients and the API: type and hash of the custom step
// specs, never commands, URLs and signature.
func (p Plan) Public() Plan {
	out := slices.Clone(p)
	for i := range out {
		out[i].Spec, out[i].SpecMAC = "", ""
	}
	return out
}

// Planner computes the plan by combining static configuration and descriptor.
type Planner interface {
	Plan(d *descriptor.Descriptor) (Plan, error)
}

// ConfigPlanner resolves the plan from the pipeline configuration.
type ConfigPlanner struct {
	Steps    []config.Step
	Registry *Registry
	// Keys sign the custom step specs; without them, specs stay unsigned.
	Keys customspec.Keys
}

// Plan applies the rules:
//   - mandatory steps always run and the descriptor cannot disable them;
//   - optional steps use the configuration default, which the descriptor can override;
//   - the descriptor cannot refer to unknown steps.
//
// The order always follows the configuration, so it is deterministic.
func (p ConfigPlanner) Plan(d *descriptor.Descriptor) (Plan, error) {
	known := map[string]config.Step{}
	for _, s := range p.Steps {
		known[s.Name] = s
	}
	var errs []error
	for name, enabled := range d.Steps {
		s, ok := known[name]
		if !ok {
			errs = append(errs, fmt.Errorf("unknown step %q", name))
			continue
		}
		if s.Mode == config.ModeMandatory && !enabled {
			errs = append(errs, fmt.Errorf("step %q is mandatory and cannot be disabled", name))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(d.StepParams)) {
		s, ok := known[name]
		if !ok || !s.IsCustom() {
			errs = append(errs, fmt.Errorf("stepParams.%s: %q is not a custom step of the pipeline", name, name))
			continue
		}
		if _, err := s.EffectiveParams(d.StepParams[name], false); err != nil {
			errs = append(errs, fmt.Errorf("stepParams.%s: %w", name, err))
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	var plan Plan
	for _, s := range p.Steps {
		enabled := s.Mode == config.ModeMandatory || s.Enabled
		if v, ok := d.Steps[s.Name]; ok && s.Mode == config.ModeOptional {
			enabled = v
		}
		if !enabled {
			continue
		}
		if s.IsCustom() {
			doc, err := customspec.Encode(s.Name, s.Spec)
			if err != nil {
				return nil, fmt.Errorf("step %q: %w", s.Name, err)
			}
			plan = append(plan, PlannedStep{
				Name: s.Name, MaxRetries: s.MaxRetries, Type: s.Type,
				Spec: doc, SpecMAC: p.Keys.Sign(doc), SpecHash: customspec.Hash(doc),
			})
			continue
		}
		if _, ok := p.Registry.Get(s.Name); !ok {
			return nil, fmt.Errorf("step %q configured but not implemented", s.Name)
		}
		plan = append(plan, PlannedStep{Name: s.Name, MaxRetries: s.MaxRetries})
	}
	return plan, nil
}
