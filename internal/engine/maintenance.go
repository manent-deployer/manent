package engine

import (
	"context"
	"errors"
	"time"

	"github.com/manent-deployer/manent/internal/cluster"
)

// MaintenanceOptions configures the worker's periodic tasks.
type MaintenanceOptions struct {
	// HeartbeatInterval is how often the worker updates the registry (default 15s).
	HeartbeatInterval time.Duration
	// StaleAfter is the heartbeat delay beyond which a worker is considered lost (default 4 heartbeats).
	StaleAfter time.Duration
	// RecoverAfter, if > 0, enables automatic recovery: the pending workflows of a worker of the
	// same version that stopped or sent no heartbeat for RecoverAfter are resumed by another
	// worker. It must be well above the expected network outages towards the database.
	RecoverAfter time.Duration
	// Retention, with MaxAge > 0, enables periodic history cleanup.
	Retention RetentionOptions
	// RetentionInterval is how often the cleanup runs (default 1h).
	RetentionInterval time.Duration
}

func (o MaintenanceOptions) withDefaults() MaintenanceOptions {
	if o.HeartbeatInterval <= 0 {
		o.HeartbeatInterval = 15 * time.Second
	}
	if o.StaleAfter <= 0 {
		o.StaleAfter = 4 * o.HeartbeatInterval
	}
	if o.RetentionInterval <= 0 {
		o.RetentionInterval = time.Hour
	}
	return o
}

// Leases of the tasks only one worker at a time must perform.
const (
	leaseRecovery  = "recovery"
	leaseRetention = "retention"
)

// maintain sends heartbeats and, if configured, runs automatic recovery and retention.
func (w *Worker) maintain() {
	defer w.wg.Done()
	o := w.maintenance
	t := time.NewTicker(o.HeartbeatInterval)
	defer t.Stop()
	var lastPrune time.Time
	warned, replaced := false, false
	for {
		select {
		case <-w.stop:
			return
		case <-t.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), o.HeartbeatInterval)
		by, err := w.registry.Heartbeat(ctx, w.id, w.startedAt)
		switch {
		case errors.Is(err, cluster.ErrReplaced):
			if !replaced {
				// Another process registered with the same ID after this one was declared lost: both can
				// resume the same deploys.
				w.logger.Error("another process registered with the same executor_id: this worker must be stopped", "executor", w.id)
				replaced = true
			}
		case err != nil:
			w.logger.Warn("heartbeat not recorded", "error", err)
		case by != "" && !warned:
			// The worker was declared lost (e.g. database unreachable) and its workflows resumed
			// elsewhere: those still running here run twice.
			w.logger.Error("the workflows of this worker were recovered by another one: it must be restarted", "recovered_by", by)
			warned = true
		}
		if err == nil {
			if rerr := w.registry.Report(ctx, w.id, w.startedAt, w.resources.sampleJSON()); rerr != nil {
				w.logger.Debug("worker resources not recorded", "error", rerr)
			}
		}
		cancel()
		if o.RecoverAfter > 0 {
			w.autoRecover()
		}
		if o.Retention.MaxAge > 0 && time.Since(lastPrune) >= o.RetentionInterval {
			if w.prune() {
				lastPrune = time.Now()
			}
		}
	}
}

// autoRecover resumes the pending workflows of the same-version workers lost for at least
// RecoverAfter. One worker at a time runs it (lease), and every lost worker is claimed by
// only one.
func (w *Worker) autoRecover() {
	o := w.maintenance
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if ok, err := w.registry.Lease(ctx, leaseRecovery, w.id, 3*o.HeartbeatInterval); err != nil || !ok {
		return
	}
	es, err := w.registry.List(ctx)
	if err != nil {
		w.logger.Warn("automatic recovery: reading the registry failed", "error", err)
		return
	}
	now := time.Now()
	for _, e := range es {
		if e.ID == w.id || e.Version != w.version || e.RecoveredBy != "" || !e.Stale(now, o.RecoverAfter) {
			continue
		}
		rep, err := w.admin.Recover(ctx, RecoverOptions{
			Executor: e.ID, StaleAfter: o.RecoverAfter, SameVersionOnly: true, By: "executor:" + w.id,
		})
		if err != nil {
			w.logger.Debug("automatic recovery not run", "executor", e.ID, "error", err)
			continue
		}
		w.logger.Warn("automatic recovery of the workflows of a lost worker", "executor", e.ID, "workflows", len(rep.Workflows))
	}
}

// prune runs the retention if the worker gets the lease; it returns true if it ran.
func (w *Worker) prune() bool {
	o := w.maintenance
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if ok, err := w.registry.Lease(ctx, leaseRetention, w.id, o.RetentionInterval); err != nil || !ok {
		return false
	}
	ro := o.Retention
	ro.By = "executor:" + w.id
	if _, err := w.admin.Prune(ctx, ro); err != nil {
		w.logger.Error("history retention failed", "error", err)
	}
	return true
}
