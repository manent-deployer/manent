package engine

import (
	"context"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/joeg-ita/manent/internal/auth"
	"github.com/joeg-ita/manent/internal/service"
)

// Me implements AccessService. The engine applies no permissions (authz.Guard does), so the
// identity is allowed everything.
func (e *Engine) Me(ctx context.Context) (*Identity, error) {
	p := auth.FromContext(ctx)
	id := &Identity{Grants: []Grant{{Role: "*", Actions: []string{"*"}, Applications: []string{"*"}, Environments: []string{"*"}}}}
	if p != nil {
		id.Subject, id.Name, id.Method, id.Groups = p.String(), p.Subject, p.Method, p.Groups
	}
	return id, nil
}

// Approvals implements GovernanceService: the deploys awaiting approval, both before the
// deploy (approval workflow) and between two waves of a rollout. It reads only unfinished
// workflows; for running deploys the rollout state is read from the event, so it is asked
// for at most maxPendingChecked deploys.
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
		// A queued deploy has not started yet: it cannot be paused between two waves.
		if w.Name == workflowName && w.Status != dbos.WorkflowStatusPending {
			continue
		}
		if checked++; checked > maxPendingChecked {
			break
		}
		// The full state also reports required approvals, decisions and expiry.
		st, err := e.Status(ctx, trimPendingSuffix(w.ID))
		if err == nil && st.State == StateAwaitingApproval {
			out = append(out, *st)
		}
	}
	return out, nil
}

// maxPendingChecked limits the waiting or running deploys whose state Approvals reads.
const maxPendingChecked = 200

var _ service.AccessService = (*Engine)(nil)
