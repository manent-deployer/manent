package engine

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/joeg-ita/manent/internal/cluster"
	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/labels"
	"github.com/joeg-ita/manent/internal/service"
)

// ReconcileOptions configures the periodic reading of the release in service on the targets.
type ReconcileOptions struct {
	// Interval between two readings of every environment; 0 disables the reconciliation.
	Interval time.Duration
	// Timeout of the reading of one target (default 30s).
	Timeout time.Duration
	// Concurrency is the number of targets of an environment read together (default 8).
	Concurrency int
}

func (o ReconcileOptions) withDefaults() ReconcileOptions {
	o.Timeout = cmp.Or(o.Timeout, 30*time.Second)
	o.Concurrency = cmp.Or(o.Concurrency, 8)
	return o
}

// TargetInspector reads from the targets the version in service (implemented by steps.Deploy).
type TargetInspector interface {
	// InspectTargets returns, for each target, the version in service ("" if none) or the error
	// reading it; the error means the environment cannot be read at all.
	InspectTargets(ctx context.Context, d *descriptor.Descriptor, targets []descriptor.Target, timeout time.Duration,
		concurrency int) (versions []string, errs []error, err error)
}

// TargetStateReader provides the states recorded by the reconciliation (Admin).
type TargetStateReader interface {
	TargetStates(ctx context.Context) ([]cluster.TargetsRecord, error)
}

// leaseReconcile is the lease of the reconciliation; every scope has its own
// (reconcile/<scope>), because only its workers reach its targets.
const leaseReconcile = "reconcile"

// scopeNoRegion is the scope of the workers with region routing but without a region (the
// orchestrators): they read the targets without a region. It is not a valid region name.
const scopeNoRegion = "~"

// reconcileScope returns the scope of this worker's reconciliation: "" without region routing
// (all targets), otherwise its region, or scopeNoRegion.
func (w *Worker) reconcileScope() string {
	switch {
	case !w.regionRouting:
		return ""
	case w.region == "":
		return scopeNoRegion
	}
	return w.region
}

// inScope reports whether the worker reads target t.
func (w *Worker) inScope(t descriptor.Target) bool {
	return !w.regionRouting || t.Labels[labels.Region] == w.region
}

// reconcileLoop reads the targets every Interval, if it gets the lease of its scope. It runs
// apart from the heartbeats: reading many targets can take long. The lease is renewed before
// every environment, so a reading longer than the interval is not interrupted and not run by
// two workers together.
func (w *Worker) reconcileLoop() {
	defer w.wg.Done()
	o := w.reconcile
	lease := leaseReconcile
	if s := w.reconcileScope(); s != "" {
		lease += "/" + s
	}
	ttl := o.Interval + 2*o.Timeout
	t := time.NewTicker(o.Interval)
	defer t.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-t.C:
		}
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			select {
			case <-w.stop:
				cancel()
			case <-ctx.Done():
			}
		}()
		renew := func() bool {
			ok, err := w.registry.Lease(ctx, lease, w.id, ttl)
			return err == nil && ok
		}
		if renew() {
			w.reconcileAll(ctx, renew)
		}
		cancel()
	}
}

// reconcileAll reads the targets of every environment with a release in service, except those
// with a deploy running: their targets are changing, and the result would be outdated anyway.
// renew renews the lease; if it is lost, the reading stops.
func (w *Worker) reconcileAll(ctx context.Context, renew func() bool) {
	store, ok := w.registry.(cluster.TargetStore)
	if !ok || w.inspector == nil || w.reader == nil {
		return
	}
	inv, err := w.reader.Inventory(ctx, InventoryFilter{})
	if err != nil {
		w.logger.Warn("riconciliazione: lettura dell'inventario fallita", "error", err)
		return
	}
	busy := map[string]bool{}
	if act, err := w.reader.Activity(ctx, ActivityFilter{}); err == nil {
		for _, s := range slices.Concat(act.Running, act.AwaitingApproval) {
			busy[releaseKey(s.Application, s.Artifact, s.Environment)] = true
		}
	} else {
		w.logger.Warn("riconciliazione: lettura dei deploy in corso fallita", "error", err)
		return
	}
	prev := map[string]service.TargetsState{}
	if recs, err := store.ListTargets(ctx); err == nil {
		for _, r := range recs {
			var st service.TargetsState
			if r.Scope == w.reconcileScope() && json.Unmarshal([]byte(r.Data), &st) == nil {
				prev[releaseKey(r.Application, r.Artifact, r.Environment)] = st
			}
		}
	}
	for _, ie := range inv {
		key := releaseKey(ie.Application, ie.Artifact, ie.Environment)
		if ie.Current == nil || busy[key] {
			continue
		}
		if ctx.Err() != nil || !renew() {
			return
		}
		st, ok := w.reconcileEnv(ctx, ie)
		if !ok {
			continue
		}
		raw, _ := json.Marshal(st)
		rec := cluster.TargetsRecord{Application: ie.Application, Artifact: ie.Artifact, Environment: ie.Environment, Scope: w.reconcileScope(), CheckedAt: st.CheckedAt, Data: string(raw)}
		if err := store.SaveTargets(ctx, rec); err != nil {
			w.logger.Warn("riconciliazione: salvataggio fallito", "application", ie.Application, "environment", ie.Environment, "error", err)
			continue
		}
		w.logTargetChanges(ie, prev[key], st)
	}
}

// reconcileEnv reads the targets of an environment and compares them with the version in
// service. It returns false when the environment has no target in this worker's scope.
func (w *Worker) reconcileEnv(ctx context.Context, ie InventoryEntry) (service.TargetsState, bool) {
	cur := ie.Current
	st := service.TargetsState{Deployment: cur.ID, Version: cur.Version}
	req, err := w.reader.loadRequest(cur.ID)
	if err != nil {
		st.CheckedAt, st.Error = time.Now().UTC(), err.Error()
		return st, true
	}
	// The environment configuration valid now: targets may have been added or removed since the
	// deploy.
	d := req.Descriptor
	if w.resolver != nil || d.HasImports() {
		// Imports too are read again: the targets of a release in service may have changed.
		eff, _, err := w.reader.importResolve(w.resolver)(ctx, d)
		if err != nil {
			st.CheckedAt, st.Error = time.Now().UTC(), "environment configuration: "+err.Error()
			return st, true
		}
		d = eff
	}
	var targets []descriptor.Target
	for _, t := range d.Deploy.Targets {
		if w.inScope(t) {
			targets = append(targets, t)
		}
	}
	if len(targets) == 0 {
		return st, false
	}
	versions, errs, err := w.inspector.InspectTargets(ctx, &d, targets, w.reconcile.Timeout, w.reconcile.Concurrency)
	st.CheckedAt = time.Now().UTC()
	if err != nil {
		st.Error = err.Error()
		return st, true
	}
	for i, t := range targets {
		ts := service.TargetState{Host: t.Host, Region: t.Labels[labels.Region], AZ: t.Labels[labels.AZ], Slot: t.Labels["slot"], Version: versions[i]}
		switch {
		case errs[i] != nil:
			ts.State, ts.Detail = service.TargetUnknown, errs[i].Error()
		case versions[i] == "":
			ts.State = service.TargetMissing
		case versions[i] == cur.Version:
			ts.State = service.TargetInSync
		default:
			ts.State = service.TargetDrifted
		}
		st.Targets = append(st.Targets, ts)
	}
	markStandby(st.Targets)
	return st, true
}

// markStandby applies the slots of percentage rollouts with the environment's router: the slot
// whose targets run the version in service is the active one, the other keeps the replaced
// version without traffic. Its targets are standby, not drifted. If no slot runs the version in
// service, nothing changes: the environment really is out of sync.
func markStandby(ts []service.TargetState) {
	active := map[string]bool{}
	for _, t := range ts {
		if t.Slot != "" && t.State == service.TargetInSync {
			active[t.Slot] = true
		}
	}
	if len(active) == 0 {
		return
	}
	for i, t := range ts {
		if t.Slot != "" && !active[t.Slot] && t.State == service.TargetDrifted {
			ts[i].State, ts[i].Detail = service.TargetStandby, "slot senza traffico"
		}
	}
}

// logTargetChanges logs (and sends to the SIEM) the targets whose state changed since the
// previous reading of the same deploy: a warning for the targets changed outside manent, an
// information when they come back in sync.
func (w *Worker) logTargetChanges(ie InventoryEntry, prev, cur service.TargetsState) {
	if prev.Deployment != cur.Deployment {
		prev = service.TargetsState{}
	}
	if cur.Error != "" {
		if cur.Error != prev.Error {
			w.logger.Warn("riconciliazione: target non leggibili", "application", ie.Application, "environment", ie.Environment, "error", cur.Error)
		}
		return
	}
	was := map[string]string{}
	for _, t := range prev.Targets {
		was[t.Host] = t.State
	}
	for _, t := range cur.Targets {
		before := cmp.Or(was[t.Host], service.TargetInSync)
		switch {
		case t.Changed() && before != t.State:
			w.logger.Warn("target cambiato fuori da manent", "application", ie.Application, "environment", ie.Environment,
				"host", t.Host, "state", t.State, "expected", cur.Version, "found", t.Version, "deployment", cur.Deployment)
		case t.State == service.TargetInSync && before != service.TargetInSync && before != service.TargetUnknown:
			w.logger.Info("target di nuovo allineato", "application", ie.Application, "environment", ie.Environment, "host", t.Host, "version", t.Version)
		}
	}
}

// mergeTargets combines the records of the scopes of an environment that refer to the deploy
// in service; records of an earlier deploy are outdated and ignored. A target read by more
// scopes (workers with and without region routing on the same database) keeps the most recent
// reading.
func mergeTargets(recs []cluster.TargetsRecord, deployment string) *service.TargetsState {
	var out *service.TargetsState
	var errs []string
	type read struct {
		t  service.TargetState
		at time.Time
	}
	byHost := map[string]read{}
	for _, r := range recs {
		var st service.TargetsState
		if json.Unmarshal([]byte(r.Data), &st) != nil || st.Deployment != deployment {
			continue
		}
		if out == nil {
			out = &service.TargetsState{CheckedAt: st.CheckedAt, Deployment: st.Deployment, Version: st.Version}
		}
		if st.CheckedAt.Before(out.CheckedAt) {
			// The oldest reading: the state is at least that recent.
			out.CheckedAt = st.CheckedAt
		}
		for _, t := range st.Targets {
			if prev, ok := byHost[t.Host]; !ok || st.CheckedAt.After(prev.at) {
				byHost[t.Host] = read{t, st.CheckedAt}
			}
		}
		if st.Error != "" && !slices.Contains(errs, st.Error) {
			errs = append(errs, st.Error)
		}
	}
	if out != nil {
		for _, h := range slices.Sorted(maps.Keys(byHost)) {
			out.Targets = append(out.Targets, byHost[h].t)
		}
		out.Error = strings.Join(errs, "; ")
	}
	return out
}

// errNoTargetStore means a registry that does not keep the target states.
var errNoTargetStore = errors.New("registry without target state")
