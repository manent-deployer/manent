// Package pipeline definisce il contratto degli step e la risoluzione del piano di esecuzione.
package pipeline

import (
	"context"
	"errors"
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

// DryRunner è implementato dagli step che sanno simulare la propria esecuzione senza effetti
// sugli ambienti di esercizio. In un deploy dry-run gli step che non lo implementano vengono saltati.
type DryRunner interface {
	DryRun(ctx context.Context, in Input) (Output, error)
}

// Compensator è implementato dagli step con effetti sugli ambienti che sanno annullarli.
// Quando un deploy fallisce e la policy di rollback lo prevede, il worker chiama Compensate
// sugli step già avviati (compreso quello fallito), in ordine inverso. Compensate riceve lo
// stesso Input dello step e l'output del suo eventuale Checkpoint. Deve essere idempotente.
// L'output riporta in OutRollback l'esito (RollbackCompleted o RollbackSkipped).
type Compensator interface {
	Compensate(ctx context.Context, in Input, checkpoint Output) (Output, error)
}

// Checkpointer è implementato dai Compensator che devono registrare, prima di eseguire lo step,
// lo stato a cui tornare (es. la release in esercizio). Il checkpoint è uno step DBOS distinto:
// i retry dello step non lo ripetono, quindi non può leggere uno stato già modificato.
// Non deve avere effetti: viene eseguito anche in dry-run.
type Checkpointer interface {
	Checkpoint(ctx context.Context, in Input) (Output, error)
}

// Chiave ed esiti di una compensazione.
const (
	OutRollback       = "rollback"
	RollbackCompleted = "completed"
	// RollbackSkipped indica che non c'era nulla da ripristinare (es. primo deploy).
	RollbackSkipped = "skipped"
)

// ErrPermanent marca gli errori che un nuovo tentativo non può risolvere (es. una violazione
// di policy o una richiesta non valida): lo step fallisce subito, senza i retry di DBOS.
var ErrPermanent = errors.New("errore permanente")

// permanentError conserva il messaggio dell'errore originale.
type permanentError struct{ err error }

func (e permanentError) Error() string        { return e.err.Error() }
func (e permanentError) Unwrap() error        { return e.err }
func (e permanentError) Is(target error) bool { return target == ErrPermanent }

// Permanent marca err come permanente; nil resta nil.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err}
}

// Retryable è il predicato dei retry degli step: gli errori permanenti non vengono ritentati.
func Retryable(err error) bool { return !errors.Is(err, ErrPermanent) }

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
