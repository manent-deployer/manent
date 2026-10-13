package engine

import (
	"context"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/joeg-ita/manent/internal/auth"
	"github.com/joeg-ita/manent/internal/service"
)

// Me implementa AccessService. L'engine non applica permessi (li applica authz.Guard), quindi
// l'identità risulta autorizzata su tutto.
func (e *Engine) Me(ctx context.Context) (*Identity, error) {
	p := auth.FromContext(ctx)
	id := &Identity{Grants: []Grant{{Role: "*", Actions: []string{"*"}, Applications: []string{"*"}, Environments: []string{"*"}}}}
	if p != nil {
		id.Subject, id.Name, id.Method, id.Groups = p.String(), p.Subject, p.Method, p.Groups
	}
	return id, nil
}

// Approvals implementa GovernanceService: i deploy in attesa di approvazione, sia prima del
// deploy (workflow di approvazione) sia tra due ondate di un rollout. Legge solo i workflow non
// conclusi; dei deploy in corso lo stato del rollout si legge dall'evento, quindi viene chiesto
// per al più maxPendingChecked deploy.
func (e *Engine) Approvals(ctx context.Context) ([]Status, error) {
	wfs, err := dbos.ListWorkflows(e.client,
		dbos.WithFilterName(workflowName, approvalWorkflowName),
		dbos.WithFilterStatus(dbos.WorkflowStatusPending, dbos.WorkflowStatusEnqueued),
		dbos.WithFilterSortDesc(), dbos.WithFilterLimit(maxActive),
		dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false))
	if err != nil {
		return nil, err
	}
	out := []Status{}
	checked := 0
	for _, w := range wfs {
		// Un deploy in coda non è ancora partito: non può essere fermo tra due ondate.
		if w.Name == workflowName && w.Status != dbos.WorkflowStatusPending {
			continue
		}
		if checked++; checked > maxPendingChecked {
			break
		}
		// Lo stato completo riporta anche approvazioni richieste, decisioni e scadenza.
		st, err := e.Status(ctx, trimPendingSuffix(w.ID))
		if err == nil && st.State == StateAwaitingApproval {
			out = append(out, *st)
		}
	}
	return out, nil
}

// maxPendingChecked limita i deploy in attesa o in corso di cui Approvals legge lo stato.
const maxPendingChecked = 200

var _ service.AccessService = (*Engine)(nil)
