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

// DescriptorPolicy limita ciò che un descrittore può aggiungere alla configurazione di ambiente.
// Chi può inviare un descrittore non è necessariamente chi amministra gli ambienti: senza
// limiti potrebbe, ad esempio, far eseguire comandi sui target (post_command) o far leggere al
// worker segreti non suoi. Ciò che arriva dalla configurazione di ambiente non è soggetto
// alla policy.
type DescriptorPolicy struct {
	// Params elenca, per plugin, i pattern dei parametri che il descrittore può impostare;
	// la chiave "*" vale per tutti i plugin.
	Params map[string][]string
	// SecretRefs sono i pattern dei riferimenti ai segreti che il descrittore può dichiarare.
	// Accettano {application} e {environment}.
	SecretRefs []string
	// Targets ammette target indicati dal descrittore quando l'ambiente non li definisce.
	Targets bool
	// StepParams elenca, per step personalizzato, i pattern dei parametri che il descrittore
	// può impostare; la chiave "*" vale per tutti gli step. Vuoto: nessuno.
	StepParams map[string][]string
}

// Check verifica il descrittore rispetto alla policy, considerando già definito ciò che
// arriva dalla configurazione di ambiente c.
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
			errs = append(errs, fmt.Errorf("deploy.params.%s non può essere impostato dal descrittore: va definito nella configurazione di ambiente (descriptor_policy.params)", k))
		}
	}
	patterns := make([]string, len(p.SecretRefs))
	for i, s := range p.SecretRefs {
		patterns[i] = expandKey(s, d.Application, d.Environment)
	}
	for _, s := range d.Secrets {
		if slices.ContainsFunc(c.Secrets, func(e descriptor.SecretRef) bool { return e.Name == s.Name }) {
			continue
		}
		if !glob.Any(patterns, s.Ref) {
			errs = append(errs, fmt.Errorf("secrets.%s: riferimento non ammesso nei descrittori (descriptor_policy.secret_refs)", s.Name))
		}
	}
	for _, step := range slices.Sorted(maps.Keys(d.StepParams)) {
		allowed := slices.Concat(p.StepParams["*"], p.StepParams[step])
		for _, k := range slices.Sorted(maps.Keys(d.StepParams[step])) {
			if _, ok := c.StepParams[step][k]; ok {
				continue
			}
			if !glob.Any(allowed, k) {
				errs = append(errs, fmt.Errorf("stepParams.%s.%s non può essere impostato dal descrittore: va definito nella configurazione di ambiente (descriptor_policy.step_params)", step, k))
			}
		}
	}
	if !p.Targets && len(d.Deploy.Targets) > 0 && len(c.Targets) == 0 {
		errs = append(errs, errors.New("deploy.targets non può essere indicato dal descrittore: va definito nella configurazione di ambiente (descriptor_policy.targets)"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("descrittore non ammesso dalla policy: %w", err)
	}
	return nil
}
