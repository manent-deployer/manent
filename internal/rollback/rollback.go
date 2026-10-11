// Package rollback applica le policy di rollback automatico al descrittore di deploy.
package rollback

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/joeg-ita/manent/internal/config"
	"github.com/joeg-ita/manent/internal/descriptor"
)

// Policy decide, per tipo di artefatto, se il rollback è ammesso e con quali default.
type Policy struct {
	// Locator è il locator di default.
	Locator string
	Types   map[string]config.RollbackType
	// Steps sono gli step della pipeline, per validare rollback.on.
	Steps []string
	// Locators sono i nomi dei locator registrati.
	Locators []string
}

// NewPolicy costruisce la policy dalla configurazione e dai locator registrati.
func NewPolicy(cfg *config.Config, locators []string) Policy {
	p := Policy{Locator: cfg.Rollback.Locator, Types: cfg.Rollback.ArtifactTypes, Locators: locators}
	for _, s := range cfg.Pipeline.Steps {
		p.Steps = append(p.Steps, s.Name)
	}
	return p
}

// Allowed indica se il tipo di artefatto ammette il rollback. Vale anche per il rollback
// manuale: tornare a una versione precedente di una migration non annulla la migration.
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

// Apply restituisce il descrittore con la decisione di rollback finale in d.Rollback:
// il default del tipo di artefatto, sovrascritto da quanto indicato nel descrittore (già fuso
// con la configurazione di ambiente). È un errore chiedere il rollback per un tipo che non
// lo ammette.
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
