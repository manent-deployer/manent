// Package pipeline definisce il contratto degli step e la risoluzione del piano di esecuzione.
package pipeline

import (
	"context"
	"fmt"

	"github.com/joeg-ita/manent/internal/descriptor"
)

// Output è il risultato di uno step. Viene persistito da DBOS: non deve mai contenere segreti.
type Output map[string]string

// Input è ciò che uno step riceve: il descrittore e gli output degli step già eseguiti.
type Input struct {
	DeploymentID string
	Descriptor   *descriptor.Descriptor
	Outputs      map[string]Output
}

// Step è una fase della pipeline di deploy.
// Run viene eseguito come step DBOS: deve essere idempotente perché può essere ripetuto
// in caso di retry o recovery.
type Step interface {
	Name() string
	Run(ctx context.Context, in Input) (Output, error)
}

// Cleaner libera le risorse locali di un deploy al termine del workflow (es. la directory di lavoro).
type Cleaner interface {
	Cleanup(ctx context.Context, deploymentID string) error
}

// Registry contiene gli step disponibili per nome.
type Registry struct {
	steps map[string]Step
}

// NewRegistry crea un registry con gli step indicati.
func NewRegistry(steps ...Step) (*Registry, error) {
	r := &Registry{steps: map[string]Step{}}
	for _, s := range steps {
		if _, ok := r.steps[s.Name()]; ok {
			return nil, fmt.Errorf("step %q registrato due volte", s.Name())
		}
		r.steps[s.Name()] = s
	}
	return r, nil
}

// Get restituisce lo step con il nome indicato.
func (r *Registry) Get(name string) (Step, bool) {
	s, ok := r.steps[name]
	return s, ok
}
