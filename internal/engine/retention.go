package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/joeg-ita/manent/internal/audit"
)

// RetentionOptions configures the history cleanup.
type RetentionOptions struct {
	// MaxAge: workflows ended more than MaxAge ago are deleted (0 = never).
	MaxAge time.Duration `json:"max_age"`
	// KeepReleases are the successful deploys always kept per application and environment (at
	// least 1): inventory, promotions and manual rollback use them.
	KeepReleases int  `json:"keep_releases"`
	DryRun       bool `json:"dry_run,omitempty"`
	// By is who runs the cleanup, recorded in the audit.
	By string `json:"-"`
}

// PruneReport is the outcome of a cleanup.
type PruneReport struct {
	Cutoff  time.Time `json:"cutoff"`
	DryRun  bool      `json:"dry_run,omitempty"`
	Deleted int       `json:"deleted"`
	// Kept are the workflows beyond the age limit kept as releases.
	Kept int `json:"kept"`
}

// pruneBatch is the number of workflows examined and deleted at a time.
const pruneBatch = 200

// Prune deletes from the system database the manent workflows (deploys, approvals, schedule
// ticks) ended more than MaxAge ago, with steps, events and logs. For every application and
// environment it keeps the last KeepReleases successful deploys and their approvals. It does
// not touch running, queued or scheduled workflows, nor the audit file.
func (a *Admin) Prune(ctx context.Context, o RetentionOptions) (*PruneReport, error) {
	if o.MaxAge <= 0 {
		return nil, fmt.Errorf("%w: max_age must be > 0", ErrInvalidRequest)
	}
	o.KeepReleases = max(o.KeepReleases, 1)
	rep := &PruneReport{Cutoff: a.now().Add(-o.MaxAge).UTC(), DryRun: o.DryRun}
	keep, err := a.releasesToKeep(o.KeepReleases)
	if err != nil {
		return nil, err
	}
	for offset := 0; ; {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		wfs, err := dbos.ListWorkflows(a.client,
			dbos.WithFilterName(workflowNames...),
			dbos.WithFilterStatus(dbos.WorkflowStatusSuccess, dbos.WorkflowStatusError, dbos.WorkflowStatusCancelled, dbos.WorkflowStatusMaxRecoveryAttemptsExceeded),
			dbos.WithFilterCompletedBefore(rep.Cutoff),
			dbos.WithFilterLimit(pruneBatch), dbos.WithFilterOffset(offset),
			dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false),
		)
		if err != nil {
			return rep, err
		}
		var ids []string
		kept := 0
		for _, w := range wfs {
			if keep[w.ID] {
				kept++
				continue
			}
			ids = append(ids, w.ID)
		}
		rep.Kept += kept
		if len(ids) > 0 && !o.DryRun {
			// Without WithDeleteChildren: the deploys that are children of schedule ticks have their own retention.
			if err := dbos.DeleteWorkflows(a.client, ids); err != nil {
				return rep, err
			}
		}
		rep.Deleted += len(ids)
		if len(wfs) < pruneBatch {
			break
		}
		// Deleted workflows disappear from the listing: the next page starts after the kept ones.
		if o.DryRun {
			offset += len(wfs)
		} else {
			offset += kept
		}
	}
	a.logger.Info("retention dello storico", "cutoff", rep.Cutoff, "deleted", rep.Deleted, "kept", rep.Kept, "dry_run", o.DryRun)
	if a.audit != nil && !o.DryRun && rep.Deleted > 0 {
		_ = a.audit.Record(ctx, audit.Entry{
			Time: a.now().UTC(), Actor: o.By, Action: ActionAdminPrune, Outcome: audit.OutcomeSucceeded,
			Comment: fmt.Sprintf("cutoff=%s eliminati=%d conservati=%d", rep.Cutoff.Format(time.RFC3339), rep.Deleted, rep.Kept),
		})
	}
	return rep, nil
}

// ActionAdminPrune is the audit action of the retention.
const ActionAdminPrune = "admin.prune"

// releasesToKeep returns the IDs of the successful deploys to keep (the last keep per
// application artifact and environment, dry-runs excluded) and of their approvals. It scans the whole
// history of successful deploys, without the listing limit.
func (a *Admin) releasesToKeep(keep int) (map[string]bool, error) {
	ids := map[string]bool{}
	perEnv := map[string]int{}
	for offset := 0; ; offset += pruneBatch {
		wfs, err := dbos.ListWorkflows(a.client,
			dbos.WithFilterName(workflowName),
			dbos.WithFilterStatus(dbos.WorkflowStatusSuccess),
			dbos.WithFilterSortDesc(),
			dbos.WithFilterLimit(pruneBatch), dbos.WithFilterOffset(offset),
			dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false),
		)
		if err != nil {
			return nil, err
		}
		for _, w := range wfs {
			s := toStatus(w)
			if s.DryRun || s.Application == "" {
				continue
			}
			k := s.Application + "/" + s.Artifact + "/" + s.Environment
			if perEnv[k] < keep {
				perEnv[k]++
				ids[w.ID], ids[w.ID+approvalSuffix], ids[w.ID+afterSuffix] = true, true, true
			}
		}
		if len(wfs) < pruneBatch {
			return ids, nil
		}
	}
}
