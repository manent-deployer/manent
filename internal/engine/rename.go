package engine

import (
	"cmp"
	"context"
	"fmt"
	"maps"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/joeg-ita/manent/internal/audit"
	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/tenant"
)

// Audit actions of the history rewrites.
const (
	ActionAdminRenameArtifact  = "admin.rename-artifact"
	ActionAdminMoveApplication = "admin.move-application"
)

// RenameArtifactOptions renames an artifact in the history of an application of a tenant.
type RenameArtifactOptions struct {
	// Tenant is the tenant of the application (default untenanted).
	Tenant                string
	Application, From, To string
	// DryRun counts the workflows without changing them.
	DryRun bool
	// By is the identity that runs the rename, for the audit.
	By string
}

// MoveApplicationOptions moves the history of an application from one tenant to another.
type MoveApplicationOptions struct {
	Application string
	// From and To are tenants; "untenanted" for the applications no tenant lists.
	From, To string
	DryRun   bool
	By       string
}

// RenameReport is the outcome of RenameArtifact and MoveApplication.
type RenameReport struct {
	Renamed int `json:"renamed"`
	// Pending are the workflows not ended, left unchanged: run again when they end.
	Pending []string `json:"pending,omitempty"`
	DryRun  bool     `json:"dry_run,omitempty"`
}

// RenameArtifact changes the artifact attribute of the ended workflows (deploys, approvals,
// dependency waits) of an application of a tenant from From to To: inventory, history,
// rollbacks and promotions follow the new name (applyRedeploy takes the name from the
// attribute). The input of the deploys is not changed. Workflows not ended are skipped: their
// end would record the old name again.
func (a *Admin) RenameArtifact(ctx context.Context, o RenameArtifactOptions) (*RenameReport, error) {
	if o.Application == "" || o.From == "" || o.To == "" || o.From == o.To {
		return nil, fmt.Errorf("%w: application, from and to are required, from different from to", ErrInvalidRequest)
	}
	if !descriptor.ValidArtifactName(o.To) {
		return nil, fmt.Errorf("%w: invalid artifact name %q: %s", ErrInvalidRequest, o.To, descriptor.ArtifactNameRule)
	}
	tn := cmp.Or(o.Tenant, tenant.Untenanted)
	rep, err := a.rewrite(ctx, o.DryRun, func(w dbos.WorkflowStatus) bool {
		return workflowTenant(w) == tn && attr(w, attrApplication) == o.Application && attr(w, attrArtifact) == o.From
	}, func(attrs map[string]any) { attrs[attrArtifact] = o.To })
	if err != nil {
		return rep, err
	}
	a.logger.Info("artefatto rinominato nello storico", "tenant", tn, "application", o.Application, "from", o.From, "to", o.To,
		"renamed", rep.Renamed, "pending", len(rep.Pending), "dry_run", o.DryRun)
	a.recordRewrite(ctx, ActionAdminRenameArtifact, o.By, o.Application, fmt.Sprintf("%s/%s: %s -> %s: rinominati=%d", tn, o.Application, o.From, o.To, rep.Renamed), rep)
	return rep, nil
}

// MoveApplication changes the tenant attribute of the ended workflows of an application from
// From to To: the history, the inventory, rollbacks and promotions follow the application to its
// new tenant (applyRedeploy takes the tenant from the attribute). It is how an untenanted
// application joins a tenant, or passes to another, without losing its releases. The input of
// the deploys is not changed; workflows not ended are skipped.
func (a *Admin) MoveApplication(ctx context.Context, o MoveApplicationOptions) (*RenameReport, error) {
	if o.Application == "" || o.From == "" || o.To == "" || o.From == o.To {
		return nil, fmt.Errorf("%w: application, from and to are required, from different from to", ErrInvalidRequest)
	}
	for _, n := range []string{o.From, o.To} {
		if err := descriptor.ValidTenantName(n); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
		}
	}
	rep, err := a.rewrite(ctx, o.DryRun, func(w dbos.WorkflowStatus) bool {
		return workflowTenant(w) == o.From && attr(w, attrApplication) == o.Application
	}, func(attrs map[string]any) { attrs[attrTenant] = o.To })
	if err != nil {
		return rep, err
	}
	a.logger.Info("applicazione spostata tra tenant nello storico", "application", o.Application, "from", o.From, "to", o.To,
		"moved", rep.Renamed, "pending", len(rep.Pending), "dry_run", o.DryRun)
	a.recordRewrite(ctx, ActionAdminMoveApplication, o.By, o.Application, fmt.Sprintf("%s: %s -> %s: spostati=%d", o.Application, o.From, o.To, rep.Renamed), rep)
	return rep, nil
}

// workflowTenant is the tenant attribute of a workflow, untenanted without.
func workflowTenant(w dbos.WorkflowStatus) string {
	return cmp.Or(attr(w, attrTenant), tenant.Untenanted)
}

// rewrite changes, with update, the attributes of the ended workflows (deploys, approvals,
// dependency waits) that match; those not ended are reported as pending.
func (a *Admin) rewrite(ctx context.Context, dryRun bool, match func(dbos.WorkflowStatus) bool, update func(map[string]any)) (*RenameReport, error) {
	rep := &RenameReport{DryRun: dryRun}
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
			if !match(w) {
				continue
			}
			if !Terminal(string(w.Status)) {
				rep.Pending = append(rep.Pending, w.ID)
				continue
			}
			rep.Renamed++
			if dryRun {
				continue
			}
			attrs := maps.Clone(w.Attributes)
			update(attrs)
			if err := dbos.SetWorkflowAttributes(a.client, w.ID, attrs); err != nil {
				return rep, fmt.Errorf("workflow %s: %w", w.ID, err)
			}
		}
		if len(wfs) < pruneBatch {
			return rep, nil
		}
	}
}

// recordRewrite records a history rewrite in the audit.
func (a *Admin) recordRewrite(ctx context.Context, action, by, application, comment string, rep *RenameReport) {
	if a.audit == nil || rep.DryRun || rep.Renamed == 0 {
		return
	}
	_ = a.audit.Record(ctx, audit.Entry{
		Time: a.now().UTC(), Actor: by, Action: action, Application: application, Outcome: audit.OutcomeSucceeded, Comment: comment,
	})
}
