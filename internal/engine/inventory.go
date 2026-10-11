package engine

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/governance"
)

// Inventory ricostruisce dallo storico dei deploy la release in esercizio in ogni
// applicazione/ambiente. Non considera i dry-run. Esamina al più gli ultimi maxListScan deploy.
func (e *Engine) Inventory(_ context.Context, f InventoryFilter) ([]InventoryEntry, error) {
	byKey := map[string]*InventoryEntry{}
	var keys []string
	err := e.scan(nil, func(s Status) bool {
		if s.DryRun || s.Application == "" || (f.Application != "" && s.Application != f.Application) || (f.Environment != "" && s.Environment != f.Environment) {
			return true
		}
		k := s.Application + "/" + s.Environment
		ie := byKey[k]
		if ie == nil {
			ie = &InventoryEntry{Application: s.Application, Environment: s.Environment}
			byKey[k] = ie
			keys = append(keys, k)
		}
		if ie.Current != nil {
			return true
		}
		switch s.State {
		case string(dbos.WorkflowStatusSuccess):
			ie.Current = &s
		case string(dbos.WorkflowStatusError), string(dbos.WorkflowStatusCancelled), string(dbos.WorkflowStatusMaxRecoveryAttemptsExceeded):
			if ie.LastFailure == nil {
				ie.LastFailure = &s
			}
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	slices.Sort(keys)
	out := []InventoryEntry{}
	for _, k := range keys {
		ie := byKey[k]
		ie.Drift = ie.LastFailure != nil && progressStarted(ie.LastFailure.RolloutProgress)
		if ie.Current != nil || ie.LastFailure != nil {
			out = append(out, *ie)
		}
	}
	return out, nil
}

// findRelease restituisce il deploy più recente concluso con successo (non dry-run)
// nell'applicazione/ambiente che soddisfa match, o nil.
func (e *Engine) findRelease(_ context.Context, application, environment string, match func(Status) bool) (*Status, error) {
	var found *Status
	err := e.scan(nil, func(s Status) bool {
		if s.Application == application && s.Environment == environment && !s.DryRun &&
			s.State == string(dbos.WorkflowStatusSuccess) && match(s) {
			found = &s
			return false
		}
		return true
	})
	return found, err
}

// loadRequest legge l'input di un deploy registrato.
func (e *Engine) loadRequest(id string) (Request, error) {
	ws, err := dbos.ListWorkflows(e.client, dbos.WithFilterWorkflowIDs(id), dbos.WithFilterLoadInput(true), dbos.WithFilterLoadOutput(false))
	if err != nil {
		return Request{}, err
	}
	if len(ws) == 0 {
		return Request{}, fmt.Errorf("%w: deploy %s", ErrNotFound, id)
	}
	var req Request
	data, ok := ws[0].Input.(string)
	if !ok {
		b, err := json.Marshal(ws[0].Input)
		if err != nil {
			return req, fmt.Errorf("input del deploy %s non leggibile: %w", id, err)
		}
		data = string(b)
	}
	if err := json.Unmarshal([]byte(data), &req); err != nil {
		return req, fmt.Errorf("input del deploy %s non leggibile: %w", id, err)
	}
	return req, nil
}

// applyRedeploy prepara il descrittore del nuovo deploy a partire da quello registrato.
func applyRedeploy(r Redeploy, d *descriptor.Descriptor, src *Status) {
	d.ID, d.DryRun, d.FreezeOverride, d.Schedule = r.ID, r.DryRun, r.FreezeOverride, nil
	if src.SHA256 != "" {
		// Viene rilasciato esattamente l'artefatto già rilasciato: lo step qa lo verifica.
		d.Artifact.SHA256 = src.SHA256
	}
}

// Promote rilascia in To lo stesso artefatto (nome, versione e sha256) rilasciato in From, con
// il descrittore del deploy di origine. I target sono propri di ogni ambiente e non vengono
// copiati. La promozione è un deploy come gli altri: valgono policy, approvazioni e freeze.
func (e *Engine) Promote(ctx context.Context, r PromoteRequest) (*Submission, error) {
	if r.Application == "" || r.From == "" || r.To == "" || r.From == r.To {
		return nil, fmt.Errorf("%w: application, from e to obbligatori, from diverso da to", ErrInvalidRequest)
	}
	src, err := e.findRelease(ctx, r.Application, r.From, func(s Status) bool { return r.Version == "" || s.Version == r.Version })
	if err != nil {
		return nil, err
	}
	if src == nil {
		v := cmp.Or(r.Version, "alcuna versione")
		return nil, fmt.Errorf("%w: %s: %s non risulta rilasciata con successo in %s", ErrInvalidRequest, r.Application, v, r.From)
	}
	req, err := e.loadRequest(src.ID)
	if err != nil {
		return nil, err
	}
	d := req.Descriptor
	d.Environment = r.To
	d.Deploy.Targets = nil
	if r.Deploy != nil {
		d.Deploy = *r.Deploy
	}
	applyRedeploy(r.Redeploy, &d, src)
	if err := d.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	return e.submit(ctx, &d, submitMeta{kind: KindPromotion, source: src.ID})
}

// Rollback rilascia di nuovo, con il suo descrittore, una versione già rilasciata con successo
// nell'ambiente: di default la più recente diversa da quella in esercizio. È ammesso solo per
// i tipi di artefatto per cui la policy di rollback lo consente (non per le migration).
//
// Se dopo l'ultimo deploy riuscito un deploy è fallito lasciando i target modificati (senza
// rollback automatico completato, o con un rollout interrotto dopo il primo stadio), la
// versione dell'ultimo deploy riuscito non è più garantita sui target: il rollback di default
// la ripristina, e la si può indicare con --to.
func (e *Engine) Rollback(ctx context.Context, r RollbackRequest) (*Submission, error) {
	if r.Application == "" || r.Environment == "" {
		return nil, fmt.Errorf("%w: application ed environment obbligatori", ErrInvalidRequest)
	}
	var current, target, failed *Status
	err := e.scan(nil, func(s Status) bool {
		if s.Application != r.Application || s.Environment != r.Environment || s.DryRun {
			return true
		}
		if s.State != string(dbos.WorkflowStatusSuccess) {
			if current == nil && failed == nil && leftChanged(s) {
				failed = &s
			}
			return true
		}
		if current == nil {
			current = &s
			if failed != nil && (r.Version == "" || r.Version == s.Version) {
				target = &s
				return false
			}
			return true
		}
		if s.Version != current.Version && (r.Version == "" || s.Version == r.Version) {
			target = &s
			return false
		}
		return true
	})
	switch {
	case err != nil:
		return nil, err
	case current == nil:
		return nil, fmt.Errorf("%w: nessun deploy riuscito di %s in %s", ErrInvalidRequest, r.Application, r.Environment)
	case target == nil && r.Version == current.Version:
		return nil, fmt.Errorf("%w: la versione %s è già in esercizio in %s", ErrInvalidRequest, r.Version, r.Environment)
	case target == nil:
		v := cmp.Or(r.Version, "una versione precedente a "+current.Version)
		return nil, fmt.Errorf("%w: %s non risulta rilasciata con successo in %s", ErrInvalidRequest, v, r.Environment)
	}
	req, err := e.loadRequest(target.ID)
	if err != nil {
		return nil, err
	}
	d := req.Descriptor
	if e.rollback != nil {
		if err := e.rollback.Allowed(d.Artifact.Type); err != nil {
			return nil, fmt.Errorf("%w: rollback manuale non ammesso: %w", ErrInvalidRequest, err)
		}
	}
	applyRedeploy(r.Redeploy, &d, target)
	if err := d.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	return e.submit(ctx, &d, submitMeta{kind: KindRollback, source: target.ID})
}

// leftChanged dice se un deploy concluso senza successo può aver lasciato i target su una
// release diversa da quella dell'ultimo deploy riuscito: fallito senza rollback automatico o
// con un rollback fallito, oppure rollout interrotto dopo almeno uno stadio. Un rollback
// skipped significa che non c'era nulla da compensare; un deploy annullato prima di partire
// (es. pianificato) non conta.
func leftChanged(s Status) bool {
	switch s.State {
	case string(dbos.WorkflowStatusError), string(dbos.WorkflowStatusMaxRecoveryAttemptsExceeded):
		return (s.Rollback != RollbackCompleted && s.Rollback != RollbackSkipped) || progressStarted(s.RolloutProgress)
	case string(dbos.WorkflowStatusCancelled):
		return progressStarted(s.RolloutProgress)
	}
	return false
}

// Freezes elenca le finestre di freeze attive e future per l'applicazione e l'ambiente.
func (e *Engine) Freezes(_ context.Context, application, environment string) ([]governance.WindowStatus, error) {
	if e.governance == nil {
		return []governance.WindowStatus{}, nil
	}
	return e.governance.Windows(application, environment, e.now()), nil
}
