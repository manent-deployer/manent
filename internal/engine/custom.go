package engine

import (
	"errors"
	"fmt"

	"github.com/manent-deployer/manent/internal/customspec"
	"github.com/manent-deployer/manent/internal/descriptor"
	"github.com/manent-deployer/manent/internal/pipeline"
)

// CustomSteps builds the custom steps from the spec captured in the plan
// (internal/customstep). requestedBy is the identity recorded in the audit of every run.
type CustomSteps interface {
	Build(ps pipeline.PlannedStep, requestedBy string) pipeline.Step
}

// stepFor returns the step of the plan: from the registry for standard steps, from the spec
// for custom ones.
func (w *Worker) stepFor(ps pipeline.PlannedStep, requestedBy string) (pipeline.Step, error) {
	if ps.IsCustom() {
		if w.custom == nil {
			return nil, pipeline.Permanent(fmt.Errorf("custom step %q: worker without support for custom steps", ps.Name))
		}
		return w.custom.Build(ps, requestedBy), nil
	}
	step, ok := w.steps.Get(ps.Name)
	if !ok {
		return nil, fmt.Errorf("step %q not implemented", ps.Name)
	}
	return step, nil
}

// checkCustomSteps checks at submission the parameters and secrets of the plan's custom
// steps. complete means the environment configuration was read: only then are missing
// required parameters and secrets errors.
func checkCustomSteps(plan pipeline.Plan, d descriptor.Descriptor, complete bool) error {
	var errs []error
	for _, ps := range plan {
		if !ps.IsCustom() {
			continue
		}
		doc, err := customspec.Decode(ps.Spec)
		if err != nil {
			errs = append(errs, fmt.Errorf("step %q: %w", ps.Name, err))
			continue
		}
		if _, err := doc.EffectiveParams(d.StepParams[ps.Name], complete); err != nil {
			errs = append(errs, fmt.Errorf("step %q: %w", ps.Name, err))
		}
		if !complete {
			continue
		}
		for _, n := range doc.DeclaredSecrets() {
			if _, ok := d.SecretRefByName(n); !ok {
				errs = append(errs, fmt.Errorf("step %q: secret %q not declared in secrets", ps.Name, n))
			}
		}
	}
	return errors.Join(errs...)
}
