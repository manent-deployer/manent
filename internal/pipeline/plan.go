package pipeline

import (
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/joeg-ita/manent/internal/config"
	"github.com/joeg-ita/manent/internal/customspec"
	"github.com/joeg-ita/manent/internal/descriptor"
)

// PlannedStep è uno step selezionato per l'esecuzione con le sue opzioni di retry.
type PlannedStep struct {
	Name       string `json:"name"`
	MaxRetries int    `json:"max_retries"`
	// Type, Spec, SpecMAC e SpecHash valgono solo per gli step personalizzati. Spec è la
	// specifica in forma canonica, fotografata all'accodamento: un deploy ripreso dopo un
	// crash esegue la definizione con cui era partito. SpecMAC è la sua firma, verificata
	// dal worker prima di eseguirla.
	Type     string `json:"type,omitempty"`
	Spec     string `json:"spec,omitempty"`
	SpecMAC  string `json:"spec_mac,omitempty"`
	SpecHash string `json:"spec_hash,omitempty"`
}

// IsCustom indica uno step personalizzato.
func (p PlannedStep) IsCustom() bool { return p.Spec != "" }

// Plan è la sequenza ordinata degli step da eseguire.
type Plan []PlannedStep

// Public restituisce il piano da mostrare a client e API: tipo e hash della specifica degli
// step personalizzati, mai comandi, URL e firma.
func (p Plan) Public() Plan {
	out := slices.Clone(p)
	for i := range out {
		out[i].Spec, out[i].SpecMAC = "", ""
	}
	return out
}

// Planner calcola il piano combinando configurazione statica e descrittore.
type Planner interface {
	Plan(d *descriptor.Descriptor) (Plan, error)
}

// ConfigPlanner risolve il piano a partire dalla configurazione della pipeline.
type ConfigPlanner struct {
	Steps    []config.Step
	Registry *Registry
	// Keys firmano le specifiche degli step personalizzati; senza, restano non firmate.
	Keys customspec.Keys
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
	for _, name := range slices.Sorted(maps.Keys(d.StepParams)) {
		s, ok := known[name]
		if !ok || !s.IsCustom() {
			errs = append(errs, fmt.Errorf("stepParams.%s: %q non è uno step personalizzato della pipeline", name, name))
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
			return nil, fmt.Errorf("step %q configurato ma non implementato", s.Name)
		}
		plan = append(plan, PlannedStep{Name: s.Name, MaxRetries: s.MaxRetries})
	}
	return plan, nil
}
