package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/joeg-ita/manent/internal/audit"
)

// RetentionOptions configura la pulizia dello storico.
type RetentionOptions struct {
	// MaxAge: i workflow conclusi da più di MaxAge vengono eliminati (0 = mai).
	MaxAge time.Duration `json:"max_age"`
	// KeepReleases sono i deploy riusciti conservati comunque per ogni applicazione e
	// ambiente (almeno 1): l'inventario, le promozioni e il rollback manuale li usano.
	KeepReleases int  `json:"keep_releases"`
	DryRun       bool `json:"dry_run,omitempty"`
	// By è chi esegue la pulizia, registrato nell'audit.
	By string `json:"-"`
}

// PruneReport è l'esito di una pulizia.
type PruneReport struct {
	Cutoff  time.Time `json:"cutoff"`
	DryRun  bool      `json:"dry_run,omitempty"`
	Deleted int       `json:"deleted"`
	// Kept sono i workflow oltre il limite di età conservati come release.
	Kept int `json:"kept"`
}

// pruneBatch è il numero di workflow esaminati ed eliminati per volta.
const pruneBatch = 200

// Prune elimina dal database di sistema i workflow di manent (deploy, approvazioni, tick delle
// schedule) conclusi da più di MaxAge, con step, eventi e log. Per ogni applicazione e
// ambiente conserva gli ultimi KeepReleases deploy riusciti e le loro approvazioni. Non tocca
// i workflow in corso, in coda o pianificati, né il file di audit.
func (a *Admin) Prune(ctx context.Context, o RetentionOptions) (*PruneReport, error) {
	if o.MaxAge <= 0 {
		return nil, fmt.Errorf("%w: max_age deve essere > 0", ErrInvalidRequest)
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
			// Senza WithDeleteChildren: i deploy figli dei tick delle schedule hanno la loro retention.
			if err := dbos.DeleteWorkflows(a.client, ids); err != nil {
				return rep, err
			}
		}
		rep.Deleted += len(ids)
		if len(wfs) < pruneBatch {
			break
		}
		// I workflow eliminati spariscono dall'elenco: la pagina successiva parte dopo quelli conservati.
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

// ActionAdminPrune è l'azione di audit della retention.
const ActionAdminPrune = "admin.prune"

// releasesToKeep restituisce gli ID dei deploy riusciti da conservare (gli ultimi keep per
// applicazione e ambiente, dry-run esclusi) e delle loro approvazioni. Scorre tutto lo storico
// dei deploy riusciti, senza il limite degli elenchi.
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
			k := s.Application + "/" + s.Environment
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
