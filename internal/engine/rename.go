package engine

import (
	"context"
	"fmt"
	"maps"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/joeg-ita/manent/internal/audit"
	"github.com/joeg-ita/manent/internal/descriptor"
)

// ActionAdminRenameArtifact is the audit action of the artifact rename.
const ActionAdminRenameArtifact = "admin.rename-artifact"

// RenameArtifactOptions renames an artifact in the history of an application.
type RenameArtifactOptions struct {
	Application, From, To string
	// DryRun counts the workflows without changing them.
	DryRun bool
	// By is the identity that runs the rename, for the audit.
	By string
}

// RenameReport is the outcome of RenameArtifact.
type RenameReport struct {
	Renamed int `json:"renamed"`
	// Pending are the workflows not ended, left unchanged: rename again when they end.
	Pending []string `json:"pending,omitempty"`
	DryRun  bool     `json:"dry_run,omitempty"`
}

// RenameArtifact changes the artifact attribute of the ended workflows (deploys, approvals,
// dependency waits) of an application from From to To: with the artifact identity, inventory,
// history, rollbacks and promotions follow the new name (applyRedeploy takes the name from the
// attribute). The input of the deploys is not changed. Workflows not ended are skipped: their
// end would record the old name again.
func (a *Admin) RenameArtifact(ctx context.Context, o RenameArtifactOptions) (*RenameReport, error) {
	if o.Application == "" || o.From == "" || o.To == "" || o.From == o.To {
		return nil, fmt.Errorf("%w: application, from and to are required, from different from to", ErrInvalidRequest)
	}
	if !descriptor.ValidArtifactName(o.To) {
		return nil, fmt.Errorf("%w: invalid artifact name %q: %s", ErrInvalidRequest, o.To, descriptor.ArtifactNameRule)
	}
	rep := &RenameReport{DryRun: o.DryRun}
	for offset := 0; ; offset += pruneBatch {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		wfs, err := dbos.ListWorkflows(a.client,
			dbos.WithFilterName(workflowName, approvalWorkflowName, afterWorkflowName),
			dbos.WithFilterLimit(pruneBatch), dbos.WithFilterOffset(offset),
			dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false),
		)
		if err != nil {
			return rep, err
		}
		for _, w := range wfs {
			if attr(w, attrApplication) != o.Application || attr(w, attrArtifact) != o.From {
				continue
			}
			if !Terminal(string(w.Status)) {
				rep.Pending = append(rep.Pending, w.ID)
				continue
			}
			rep.Renamed++
			if o.DryRun {
				continue
			}
			attrs := maps.Clone(w.Attributes)
			attrs[attrArtifact] = o.To
			if err := dbos.SetWorkflowAttributes(a.client, w.ID, attrs); err != nil {
				return rep, fmt.Errorf("workflow %s: %w", w.ID, err)
			}
		}
		if len(wfs) < pruneBatch {
			break
		}
	}
	a.logger.Info("artefatto rinominato nello storico", "application", o.Application, "from", o.From, "to", o.To,
		"renamed", rep.Renamed, "pending", len(rep.Pending), "dry_run", o.DryRun)
	if a.audit != nil && !o.DryRun && rep.Renamed > 0 {
		_ = a.audit.Record(ctx, audit.Entry{
			Time: a.now().UTC(), Actor: o.By, Action: ActionAdminRenameArtifact, Application: o.Application, Outcome: audit.OutcomeSucceeded,
			Comment: fmt.Sprintf("%s -> %s: rinominati=%d", o.From, o.To, rep.Renamed),
		})
	}
	return rep, nil
}
