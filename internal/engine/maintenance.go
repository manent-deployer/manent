package engine

import (
	"context"
	"time"
)

// MaintenanceOptions configura le attività periodiche del worker.
type MaintenanceOptions struct {
	// HeartbeatInterval è ogni quanto il worker aggiorna il registro (default 15s).
	HeartbeatInterval time.Duration
	// StaleAfter è il ritardo di heartbeat oltre il quale un worker è considerato perso
	// (default 4 heartbeat).
	StaleAfter time.Duration
	// RecoverAfter, se > 0, abilita la recovery automatica: i workflow in sospeso di un worker
	// della stessa versione arrestato o senza heartbeat da RecoverAfter vengono ripresi da un
	// altro worker. Deve superare di molto le interruzioni di rete attese verso il database.
	RecoverAfter time.Duration
	// Retention, con MaxAge > 0, abilita la pulizia periodica dello storico.
	Retention RetentionOptions
	// RetentionInterval è ogni quanto viene eseguita la pulizia (default 1h).
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

// Lease delle attività che un solo worker alla volta deve svolgere.
const (
	leaseRecovery  = "recovery"
	leaseRetention = "retention"
)

// maintain invia gli heartbeat e, se configurate, esegue recovery automatica e retention.
func (w *Worker) maintain() {
	defer w.wg.Done()
	o := w.maintenance
	t := time.NewTicker(o.HeartbeatInterval)
	defer t.Stop()
	var lastPrune time.Time
	warned := false
	for {
		select {
		case <-w.stop:
			return
		case <-t.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), o.HeartbeatInterval)
		by, err := w.registry.Heartbeat(ctx, w.id, w.startedAt)
		switch {
		case err != nil:
			w.logger.Warn("heartbeat non registrato", "error", err)
		case by != "" && !warned:
			// Il worker è stato considerato perso (es. database irraggiungibile) e i suoi
			// workflow ripresi altrove: quelli ancora in esecuzione qui girano due volte.
			w.logger.Error("i workflow di questo worker sono stati recuperati da un altro: va riavviato", "recovered_by", by)
			warned = true
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

// autoRecover riprende i workflow in sospeso dei worker della stessa versione persi da
// almeno RecoverAfter. Lo esegue un worker alla volta (lease), e ogni worker perso viene
// rivendicato da uno solo.
func (w *Worker) autoRecover() {
	o := w.maintenance
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if ok, err := w.registry.Lease(ctx, leaseRecovery, w.id, 3*o.HeartbeatInterval); err != nil || !ok {
		return
	}
	es, err := w.registry.List(ctx)
	if err != nil {
		w.logger.Warn("recovery automatica: lettura del registro fallita", "error", err)
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
			w.logger.Debug("recovery automatica non eseguita", "executor", e.ID, "error", err)
			continue
		}
		w.logger.Warn("recovery automatica dei workflow di un worker perso", "executor", e.ID, "workflows", len(rep.Workflows))
	}
}

// prune esegue la retention se il worker ottiene il lease; restituisce true se l'ha eseguita.
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
		w.logger.Error("retention dello storico fallita", "error", err)
	}
	return true
}
