package pipeline

import (
	"errors"
	"fmt"

	"github.com/joeg-ita/manent/internal/config"
	"github.com/joeg-ita/manent/internal/descriptor"
)

// PlannedStep è uno step selezionato per l'esecuzione con le sue opzioni di retry.
type PlannedStep struct {
	Name       string `json:"name"`
	MaxRetries int    `json:"max_retries"`
}

// Plan è la sequenza ordinata degli step da eseguire.
type Plan []PlannedStep

// Planner calcola il piano combinando configurazione statica e descrittore.
type Planner interface {
	Plan(d *descriptor.Descriptor) (Plan, error)
}

// ConfigPlanner risolve il piano a partire dalla configurazione della pipeline.
type ConfigPlanner struct {
	Steps    []config.Step
	Registry *Registry
}

// Plan applica le regole:
//   - gli step obbligatori sono sempre eseguiti e non possono essere disattivati dal descrittore;
//   - gli step opzionali usano il default di configurazione, sovrascrivibile dal descrittore;
//   - il descrittore non può riferire step sconosciuti.
//
// L'ordine segue sempre la configurazione, quindi è deterministico.
func (p ConfigPlanner) Plan(d *descriptor.Descriptor) (Plan, error) {
	known := map[string]config.Step{}
	for _, s := range p.Steps {
		known[s.Name] = s
	}
	var errs []error
	for name, enabled := range d.Steps {
		s, ok := known[name]
		if !ok {
			errs = append(errs, fmt.Errorf("step %q sconosciuto", name))
			continue
		}
		if s.Mode == config.ModeMandatory && !enabled {
			errs = append(errs, fmt.Errorf("step %q è obbligatorio e non può essere disattivato", name))
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
		if _, ok := p.Registry.Get(s.Name); !ok {
			return nil, fmt.Errorf("step %q configurato ma non implementato", s.Name)
		}
		plan = append(plan, PlannedStep{Name: s.Name, MaxRetries: s.MaxRetries})
	}
	return plan, nil
}
