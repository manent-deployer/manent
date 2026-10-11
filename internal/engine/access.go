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
// deploy (workflow di approvazione) sia tra due ondate di un rollout. Per questi ultimi lo
// stato si legge dall'evento del rollout, quindi viene chiesto per ogni deploy in attesa o in
// corso (pochi: al più maxPendingChecked).
func (e *Engine) Approvals(ctx context.Context) ([]Status, error) {
	out := []Status{}
	var candidates []string
	err := e.scan(nil, func(s Status) bool {
		if s.State == StateAwaitingApproval || s.State == string(dbos.WorkflowStatusPending) {
			candidates = append(candidates, s.ID)
		}
		return len(candidates) < maxPendingChecked
	})
	if err != nil {
		return nil, err
	}
	// Lo stato completo riporta anche approvazioni richieste, decisioni e scadenza.
	for _, id := range candidates {
		st, err := e.Status(ctx, id)
		if err == nil && st.State == StateAwaitingApproval {
			out = append(out, *st)
		}
	}
	return out, nil
}

// maxPendingChecked limita i deploy in attesa o in corso di cui Approvals legge lo stato.
const maxPendingChecked = 200

var _ service.AccessService = (*Engine)(nil)
