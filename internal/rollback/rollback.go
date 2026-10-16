// Package rollback applies the automatic rollback policies to the deploy descriptor.
package rollback

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/joeg-ita/manent/internal/config"
	"github.com/joeg-ita/manent/internal/descriptor"
)

// Policy decides, per artifact type, whether rollback is allowed and with which defaults.
type Policy struct {
	// Locator is the default locator.
	Locator string
	Types   map[string]config.RollbackType
	// Steps are the pipeline steps, to validate rollback.on.
	Steps []string
	// Locators are the names of the registered locators.
	Locators []string
}

// NewPolicy builds the policy from the configuration and the registered locators.
func NewPolicy(cfg *config.Config, locators []string) Policy {
	p := Policy{Locator: cfg.Rollback.Locator, Types: cfg.Rollback.ArtifactTypes, Locators: locators}
	for _, s := range cfg.Pipeline.Steps {
		p.Steps = append(p.Steps, s.Name)
	}
	return p
}

// Allowed reports whether the artifact type allows rollback. It also applies to manual
// rollback: going back to an earlier version of a migration does not undo the migration.
func (p Policy) Allowed(artifactType string) error {
	tp, known := p.Types[artifactType]
	switch {
	case !known:
		return fmt.Errorf("rollback non ammesso per artefatti di tipo %q: tipo assente in rollback.artifact_types", artifactType)
	case !tp.Allowed:
		return fmt.Errorf("rollback non ammesso per artefatti di tipo %q", artifactType)
	}
	return nil
}

// Apply returns the descriptor with the final rollback decision in d.Rollback: the artifact
// type default, overridden by what the descriptor says (already merged with the environment
// configuration). Requesting rollback for a type that does not allow it is an error.
func (p Policy) Apply(d descriptor.Descriptor) (descriptor.Descriptor, error) {
	tp := p.Types[d.Artifact.Type]
	req := descriptor.Rollback{}
	if d.Rollback != nil {
		req = *d.Rollback
	}
	enabled := tp.Enabled
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	if !enabled {
		d.Rollback = &descriptor.Rollback{Enabled: &enabled}
		return d, nil
	}
	if err := p.Allowed(d.Artifact.Type); err != nil {
		return d, err
	}
	on := req.On
	if len(on) == 0 {
		on = tp.On
	}
	if len(on) == 0 {
		on = config.DefaultRollbackOn
	}
	for _, s := range on {
		if len(p.Steps) > 0 && !slices.Contains(p.Steps, s) {
			return d, fmt.Errorf("rollback.on: step %q non presente nella pipeline", s)
		}
	}
	locator := cmp.Or(req.Locator, tp.Locator, p.Locator, "plugin")
	if len(p.Locators) > 0 && !slices.Contains(p.Locators, locator) {
		return d, fmt.Errorf("rollback.locator %q sconosciuto (disponibili: %v)", locator, p.Locators)
	}
	d.Rollback = &descriptor.Rollback{Enabled: &enabled, On: slices.Clone(on), Locator: locator}
	return d, nil
}
