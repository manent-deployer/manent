package engine

import (
	"errors"
	"fmt"

	"github.com/joeg-ita/manent/internal/customspec"
	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/pipeline"
)

// CustomSteps costruisce gli step personalizzati dalla specifica fotografata nel piano
// (internal/customstep). requestedBy è l'identità registrata nell'audit di ogni esecuzione.
type CustomSteps interface {
	Build(ps pipeline.PlannedStep, requestedBy string) pipeline.Step
}

// stepFor restituisce lo step del piano: dal registry per gli step standard, dalla
// specifica per quelli personalizzati.
func (w *Worker) stepFor(ps pipeline.PlannedStep, requestedBy string) (pipeline.Step, error) {
	if ps.IsCustom() {
		if w.custom == nil {
			return nil, pipeline.Permanent(fmt.Errorf("step %q personalizzato: worker senza supporto agli step personalizzati", ps.Name))
		}
		return w.custom.Build(ps, requestedBy), nil
	}
	step, ok := w.steps.Get(ps.Name)
	if !ok {
		return nil, fmt.Errorf("step %q non implementato", ps.Name)
	}
	return step, nil
}

// checkCustomSteps verifica all'accodamento parametri e segreti degli step personalizzati
// del piano. complete indica che la configurazione di ambiente è stata letta: solo allora
// parametri obbligatori e segreti mancanti sono errori.
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
				errs = append(errs, fmt.Errorf("step %q: segreto %q non dichiarato in secrets", ps.Name, n))
			}
		}
	}
	return errors.Join(errs...)
}
