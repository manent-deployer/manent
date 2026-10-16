package engine

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/governance"
)

// Inventory rebuilds from the deploy history the release in service in every
// application/environment. It ignores dry-runs. The state is kept in a process cache
// (inventoryCache): every call reads only new workflows and those not yet ended.
func (e *Engine) Inventory(_ context.Context, f InventoryFilter) ([]InventoryEntry, error) {
	e.inv.mu.Lock()
	defer e.inv.mu.Unlock()
	if err := e.refreshInventory(); err != nil {
		return nil, err
	}
	out := []InventoryEntry{}
	for _, k := range slices.Sorted(maps.Keys(e.inv.entries)) {
		ie := *e.inv.entries[k]
		if (f.Application != "" && ie.Application != f.Application) || (f.Environment != "" && ie.Environment != f.Environment) {
			continue
		}
		ie.Drift = ie.LastFailure != nil && progressStarted(ie.LastFailure.RolloutProgress)
		if ie.Current != nil || ie.LastFailure != nil {
			out = append(out, ie)
		}
	}
	return out, nil
}

// inventoryRebuild is the interval after which the inventory is rebuilt from scratch: it
// covers what the incremental read cannot see, like workflows deleted by retention.
const inventoryRebuild = 10 * time.Minute

// inventoryCache is the process inventory. It is rebuilt by scanning the history (at most
// maxListScan workflows) on first use and every inventoryRebuild; in between it reads the
// workflows created after the last one read (created_at index) and reads again by ID those
// not yet ended.
type inventoryCache struct {
	mu      sync.Mutex
	builtAt time.Time
	entries map[string]*InventoryEntry
	// mark is the most recent creation time read; recent are the workflows read that were
	// created in the last inventoryOverlap before mark, not to be read again.
	mark   time.Time
	recent map[string]time.Time
	// pending are the workflows read and not yet ended.
	pending map[string]bool
}

// inventoryOverlap is the margin for reading again the workflows created shortly before the
// last one read: created_at is set on insert, so a slower transaction can make visible a
// workflow with a time earlier than those already read.
const inventoryOverlap = 5 * time.Second

// refreshInventory updates the cache; call it with e.inv.mu locked.
func (e *Engine) refreshInventory() error {
	c := &e.inv
	if c.entries == nil || time.Since(c.builtAt) >= inventoryRebuild {
		return e.rebuildInventory()
	}
	names := dbos.WithFilterName(workflowName, approvalWorkflowName, afterWorkflowName)
	if len(c.pending) > 0 {
		ws, err := dbos.ListWorkflows(e.client, names, dbos.WithFilterWorkflowIDs(slices.Collect(maps.Keys(c.pending))...),
			dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false))
		if err != nil {
			return err
		}
		for _, w := range ws {
			c.apply(w)
		}
	}
	const page = 500
	from := c.mark.Add(-inventoryOverlap)
	for {
		ws, err := dbos.ListWorkflows(e.client, names, dbos.WithFilterCreatedAfter(from), dbos.WithFilterLimit(page),
			dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false))
		if err != nil {
			return err
		}
		fresh := 0
		for _, w := range ws {
			from = w.CreatedAt
			if _, ok := c.recent[w.ID]; ok {
				continue
			}
			fresh++
			c.see(w)
			c.apply(w)
		}
		if len(ws) < page {
			break
		}
		if fresh == 0 {
			// A whole page already read: continue from the next millisecond.
			from = from.Add(time.Millisecond)
		}
	}
	for id, at := range c.recent {
		if at.Before(c.mark.Add(-inventoryOverlap)) {
			delete(c.recent, id)
		}
	}
	return nil
}

// see records a read workflow for the next incremental read.
func (c *inventoryCache) see(w dbos.WorkflowStatus) {
	if w.CreatedAt.After(c.mark) {
		c.mark = w.CreatedAt
	}
	if !w.CreatedAt.Before(c.mark.Add(-inventoryOverlap)) {
		c.recent[w.ID] = w.CreatedAt
	}
}

// rebuildInventory rebuilds the cache by scanning the history from the most recent.
func (e *Engine) rebuildInventory() error {
	c := &e.inv
	c.entries, c.pending, c.recent, c.mark = map[string]*InventoryEntry{}, map[string]bool{}, map[string]time.Time{}, time.Time{}
	err := e.scanRaw(nil, func(w dbos.WorkflowStatus) bool {
		c.see(w)
		c.apply(w)
		return true
	})
	if err != nil {
		c.entries = nil
		return err
	}
	c.builtAt = time.Now()
	return nil
}

// apply updates the inventory with a read workflow: a successful deploy becomes the release in
// service if it is the most recent; a failed or cancelled deploy is the last failure if more
// recent than the release in service. Unfinished workflows remain to be read again.
func (c *inventoryCache) apply(w dbos.WorkflowStatus) {
	delete(c.pending, w.ID)
	if superseded(w) {
		return
	}
	s := toStatus(w)
	if s.DryRun || s.Application == "" {
		return
	}
	if !Terminal(s.State) {
		c.pending[w.ID] = true
		return
	}
	k := s.Application + "/" + s.Environment
	ie := c.entries[k]
	if ie == nil {
		ie = &InventoryEntry{Application: s.Application, Environment: s.Environment}
		c.entries[k] = ie
	}
	switch s.State {
	case string(dbos.WorkflowStatusSuccess):
		if ie.Current == nil || s.CreatedAt.After(ie.Current.CreatedAt) {
			ie.Current = &s
			if ie.LastFailure != nil && !ie.LastFailure.CreatedAt.After(s.CreatedAt) {
				ie.LastFailure = nil
			}
		}
	case string(dbos.WorkflowStatusError), string(dbos.WorkflowStatusCancelled), string(dbos.WorkflowStatusMaxRecoveryAttemptsExceeded):
		if (ie.Current == nil || s.CreatedAt.After(ie.Current.CreatedAt)) &&
			(ie.LastFailure == nil || s.CreatedAt.After(ie.LastFailure.CreatedAt)) {
			ie.LastFailure = &s
		}
	}
}

// findRelease returns the most recent successfully ended deploy (not dry-run) in the
// application/environment that satisfies match, or nil.
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

// loadRequest reads the input of a recorded deploy.
func (e *Engine) loadRequest(id string) (Request, error) {
	ws, err := dbos.ListWorkflows(e.client, dbos.WithFilterWorkflowIDs(id), dbos.WithFilterLoadInput(true), dbos.WithFilterLoadOutput(false))
	if err != nil {
		return Request{}, err
	}
	if len(ws) == 0 {
		return Request{}, fmt.Errorf("%w: deploy %s", ErrNotFound, id)
	}
	var req Request
	if err := decodeInput(ws[0].Input, &req); err != nil {
		return req, fmt.Errorf("input del deploy %s non leggibile: %w", id, err)
	}
	return req, nil
}

// applyRedeploy prepares the descriptor of the new deploy from the recorded one.
func applyRedeploy(r Redeploy, d *descriptor.Descriptor, src *Status) {
	d.ID, d.DryRun, d.FreezeOverride, d.Schedule = r.ID, r.DryRun, r.FreezeOverride, nil
	// The dependencies applied to the source deploy, already ended.
	d.After, d.AfterTimeout = nil, ""
	if src.SHA256 != "" {
		// Exactly the artifact already released is released: the qa step verifies it.
		d.Artifact.SHA256 = src.SHA256
	}
}

// Promote releases to To the same artifact (name, version and sha256) released to From, with
// the descriptor of the source deploy. Targets belong to each environment and are not copied.
// A promotion is a deploy like the others: policies, approvals and freezes apply.
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

// Rollback releases again, with its descriptor, a version already successfully released to
// the environment: by default the most recent one other than the one in service. It is
// allowed only for artifact types whose rollback policy allows it (not for migrations).
//
// If after the last successful deploy a deploy failed leaving the targets changed (without a
// completed automatic rollback, or with a rollout interrupted after the first stage), the
// version of the last successful deploy is no longer guaranteed on the targets: the default
// rollback restores it, and it can be given with --to.
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

// leftChanged says whether a deploy ended without success may have left the targets on a
// release other than the last successful deploy's: failed without automatic rollback or with
// a failed rollback, or rollout interrupted after at least one stage. A skipped rollback means
// there was nothing to compensate; a deploy cancelled before starting (e.g. scheduled) does
// not count.
func leftChanged(s Status) bool {
	switch s.State {
	case string(dbos.WorkflowStatusError), string(dbos.WorkflowStatusMaxRecoveryAttemptsExceeded):
		return (s.Rollback != RollbackCompleted && s.Rollback != RollbackSkipped) || progressStarted(s.RolloutProgress)
	case string(dbos.WorkflowStatusCancelled):
		return progressStarted(s.RolloutProgress)
	}
	return false
}

// Freezes lists the active and future freeze windows for the application and environment.
func (e *Engine) Freezes(_ context.Context, application, environment string) ([]governance.WindowStatus, error) {
	if e.governance == nil {
		return []governance.WindowStatus{}, nil
	}
	return e.governance.Windows(application, environment, e.now()), nil
}
