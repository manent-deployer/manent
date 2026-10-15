package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/joeg-ita/manent/internal/callback"
	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/pipeline"
	"github.com/joeg-ita/manent/internal/rollout"
	"github.com/joeg-ita/manent/internal/service"
	"github.com/joeg-ita/manent/internal/steps"
)

// Nome stabile del workflow figlio che ridimensiona la versione sostituita da un rilascio per
// percentuale, dopo rollout.traffic.scaleDownDelay.
const retireWorkflowName = "manent.traffic-retire"

// TrafficOps sono le operazioni sui load balancer e sui router, eseguite dal motore dentro
// step DBOS (implementate da internal/steps).
type TrafficOps interface {
	// Check verifica la configurazione di traffico del descrittore effettivo, senza effetti.
	Check(d *descriptor.Descriptor) error
	// PoolStatus legge connessioni residue (phase "drain") o salute (phase "restore") dei
	// target dell'input.
	PoolStatus(ctx context.Context, in pipeline.Input, phase string) (pipeline.Output, error)
	Prepare(ctx context.Context, in pipeline.Input) (pipeline.Output, error)
	Testers(ctx context.Context, in pipeline.Input) (pipeline.Output, error)
	SetWeight(ctx context.Context, in pipeline.Input, percent int) (pipeline.Output, error)
	Weight(ctx context.Context, in pipeline.Input) (pipeline.Output, error)
	Scale(ctx context.Context, in pipeline.Input, percent int) (pipeline.Output, error)
	Promote(ctx context.Context, in pipeline.Input) (pipeline.Output, error)
	Revert(ctx context.Context, in pipeline.Input) (pipeline.Output, error)
	Retire(ctx context.Context, in pipeline.Input) (pipeline.Output, error)
}

// awaitsDecision indica uno stato del rollout che attende una decisione (approve/reject).
func awaitsDecision(status string) bool {
	return status == service.RolloutPausedApproval || status == service.RolloutPausedDrift
}

// passTrack collega un passo allo stato pubblicato del rollout (nil nei workflow figli).
type passTrack struct {
	rs   *RolloutState
	pass *RolloutPass
}

func (t *passTrack) drained(w *Worker, ctx dbos.Context, members []string, add bool) {
	if t == nil || t.pass == nil {
		return
	}
	for _, m := range members {
		i := slices.Index(t.pass.Drained, m)
		switch {
		case add && i < 0:
			t.pass.Drained = append(t.pass.Drained, m)
		case !add && i >= 0:
			t.pass.Drained = slices.Delete(t.pass.Drained, i, i+1)
		}
	}
	w.publishRollout(ctx, t.rs)
}

// trafficOp esegue un'operazione di traffico come step DBOS, con log, span ed eventi.
func (w *Worker) trafficOp(ctx dbos.Context, ev *events, d *descriptor.Descriptor, res *Result, name string, quiet bool,
	in pipeline.Input, fn func(context.Context, pipeline.Input) (pipeline.Output, error)) (pipeline.Output, error) {
	out, err := dbos.RunAsStep(ctx,
		func(c context.Context) (pipeline.Output, error) {
			return instrument(w, c, ev, name, func(c context.Context) (pipeline.Output, error) { return fn(c, in) })
		},
		dbos.WithStepName(name), dbos.WithStepMaxRetries(3), dbos.WithStepRetryPredicate(pipeline.Retryable),
	)
	if !quiet || err != nil {
		ev.send(d, stepEvent(name, out, err))
	}
	if err != nil {
		return nil, fmt.Errorf("step %q: %w", name, err)
	}
	if !quiet {
		res.Steps = append(res.Steps, StepResult{Name: name, Output: out})
	}
	return out, nil
}

// drainBatch svuota i target di un lotto: lo step drain verifica la capacità del pool (se
// insufficiente il motore attende e riprova, fino a drain_timeout) e toglie i membri, poi
// un'attesa durevole aspetta la fine delle connessioni.
func (w *Worker) drainBatch(ctx dbos.Context, id string, ev *events, d *descriptor.Descriptor, batch []descriptor.Target,
	suffix string, outputs map[string]pipeline.Output, res *Result, track *passTrack) (*startedStep, error) {
	if w.traffic == nil {
		return nil, pipeline.Permanent(errors.New("traffic.pool: integrazione non disponibile su questo server"))
	}
	cfg := d.Traffic.Pool
	timeout, _, poll := cfg.Durations()
	ps := pipeline.PlannedStep{Name: steps.DrainStep, MaxRetries: 3}
	waited := time.Duration(0)
	var started *startedStep
	for k := 0; ; k++ {
		label := steps.DrainStep + suffix
		if k > 0 {
			label += "." + strconv.Itoa(k)
		}
		out, st, _, err := w.execStep(ctx, id, ev, d, ps, label, batch, outputs, res)
		if err != nil {
			return st, err
		}
		if out[steps.OutCapacity] != steps.CapacityInsufficient {
			started = st
			break
		}
		if waited >= timeout {
			return nil, fmt.Errorf("svuotamento non eseguito entro %s: %s", timeout, out[steps.OutDetail])
		}
		if _, err := dbos.Sleep(ctx, poll); err != nil {
			return nil, err
		}
		waited += poll
	}
	members := poolMembers(d, batch)
	track.drained(w, ctx, members, true)
	if err := w.poolWait(ctx, id, ev, d, batch, steps.DrainStep, suffix, outputs, res); err != nil {
		if cfg.OnTimeout == descriptor.OnTimeoutFail {
			// Il lotto non viene toccato: torna in servizio prima di fermare il passo.
			if _, rerr := w.restoreBatch(ctx, id, ev, d, batch, suffix, outputs, res, track); rerr != nil {
				return started, errors.Join(err, rerr)
			}
			return nil, err
		}
	}
	return started, nil
}

// restoreBatch rimette in servizio i target del lotto e attende che il load balancer li veda
// sani: un target non torna mai in servizio senza essere sano.
func (w *Worker) restoreBatch(ctx dbos.Context, id string, ev *events, d *descriptor.Descriptor, batch []descriptor.Target,
	suffix string, outputs map[string]pipeline.Output, res *Result, track *passTrack) (*startedStep, error) {
	ps := pipeline.PlannedStep{Name: steps.RestoreStep, MaxRetries: 3}
	_, st, _, err := w.execStep(ctx, id, ev, d, ps, steps.RestoreStep+suffix, batch, outputs, res)
	if err != nil {
		return st, err
	}
	track.drained(w, ctx, poolMembers(d, batch), false)
	return st, w.poolWait(ctx, id, ev, d, batch, steps.RestoreStep, suffix, outputs, res)
}

func poolMembers(d *descriptor.Descriptor, ts []descriptor.Target) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = d.Traffic.Pool.MemberName(t)
	}
	return out
}

// poolWait attende, con letture e pause durevoli, la fine delle connessioni (drain) o la
// salute dei target (restore). Allo scadere: per lo svuotamento con on_timeout proceed il
// deploy prosegue (le connessioni residue vengono chiuse dal riavvio), altrimenti è un errore.
func (w *Worker) poolWait(ctx dbos.Context, id string, ev *events, d *descriptor.Descriptor, batch []descriptor.Target,
	phase, suffix string, outputs map[string]pipeline.Output, res *Result) error {
	cfg := d.Traffic.Pool
	drainTimeout, restoreTimeout, poll := cfg.Durations()
	timeout := drainTimeout
	if phase == steps.RestoreStep {
		timeout = restoreTimeout
	}
	in := startedStep{Targets: batch, Outputs: outputs}.input(id, d)
	elapsed := time.Duration(0)
	for n := 1; ; n++ {
		name := phase + "-wait" + suffix + "." + strconv.Itoa(n)
		out, err := w.trafficOp(ctx, ev, d, res, name, true, in, func(c context.Context, in pipeline.Input) (pipeline.Output, error) {
			return w.traffic.PoolStatus(c, in, phase)
		})
		if err != nil {
			return err
		}
		minWait, _ := time.ParseDuration(out["min_wait"])
		if out["done"] == "true" && elapsed >= minWait {
			return nil
		}
		if elapsed >= max(timeout, minWait) {
			if phase == steps.DrainStep && cfg.OnTimeout != descriptor.OnTimeoutFail {
				w.logger.Warn("svuotamento oltre il timeout: il deploy prosegue", "deployment", id, "pending", out["pending"], "timeout", timeout)
				return nil
			}
			return fmt.Errorf("%s: target non pronti entro %s (%s)", phase, timeout, out["pending"])
		}
		wait := poll
		if out["done"] == "true" && minWait > elapsed {
			wait = minWait - elapsed
		}
		if _, err := dbos.Sleep(ctx, wait); err != nil {
			return err
		}
		elapsed += wait
	}
}

// trafficPlan è il piano di un rilascio per percentuale: con le ondate i passi del piano a
// ondate (un cluster o una region alla volta), ciascuno con tutti i gradini; senza, un solo
// passo con tutti i target.
func trafficPlan(d descriptor.Descriptor) (rollout.Plan, error) {
	if len(d.Rollout.Waves) > 0 {
		return rollout.Build(d.Deploy.Targets, d.Rollout)
	}
	p := rollout.Pass{Name: "traffic", Wave: "traffic", Targets: d.Deploy.Targets, Batches: [][]descriptor.Target{d.Deploy.Targets}}
	return rollout.Plan{Stages: []rollout.Stage{{Name: "traffic", Wave: "traffic", Passes: []rollout.Pass{p}, LastOfWave: true}}}, nil
}

// multiPass indica un rilascio per percentuale a ondate: nomi degli step e degli stadi
// riportano il passo.
func multiPass(d *descriptor.Descriptor) bool { return len(d.Rollout.Waves) > 0 }

// stageName è il nome dello stadio di un gradino ("25%", o "<passo>/25%" con le ondate).
func stageName(d *descriptor.Descriptor, pass string, percent int) string {
	name := strconv.Itoa(percent) + "%"
	if multiPass(d) {
		name = pass + "/" + name
	}
	return name
}

// newTrafficState crea lo stato di un rilascio per percentuale: uno stadio per gradino di
// ogni passo.
func newTrafficState(d *descriptor.Descriptor, plan rollout.Plan) *RolloutState {
	r := d.Rollout
	st := &RolloutState{Strategy: descriptor.StrategyTraffic, Status: service.RolloutRunning, OnFailure: r.OnFailureOrDefault(),
		Traffic: &service.TrafficState{Steps: slices.Clone(r.Traffic.Steps)}}
	for _, stage := range plan.Stages {
		for _, p := range stage.Passes {
			for _, pct := range r.Traffic.Steps {
				st.Stages = append(st.Stages, RolloutStage{Name: stageName(d, p.Name, pct), Wave: p.Wave, State: service.StagePending,
					Passes: []RolloutPass{{Name: p.Name, Region: p.Region, AZ: p.AZ, Hosts: p.Hosts()}}})
			}
		}
	}
	return st
}

// trafficRun è lo stato del rilascio di un passo, per il ritorno in caso di errore.
type trafficRun struct {
	pass rollout.Pass
	// base è l'indice del primo stadio del passo in RolloutState.Stages.
	base int
	// sfx è il suffisso dei nomi degli step del passo ("" senza ondate).
	sfx      string
	in       pipeline.Input
	prepared bool
	promoted bool
	started  []startedStep
}

// runTraffic esegue un rilascio per percentuale: step iniziali; per ogni passo (uno solo
// senza ondate) preparazione (versione stabile, slot candidato), gli step da deploy ad
// activation esclusa sulla candidata senza traffico, tester, gradini (pesi, bake, gate,
// approvazioni), promozione e activation; infine gli step finali. Un workflow figlio per passo
// ridimensiona la versione sostituita dopo scaleDownDelay.
func (w *Worker) runTraffic(ctx dbos.Context, id string, req Request, d *descriptor.Descriptor, ev *events, res *Result) error {
	if w.traffic == nil {
		return pipeline.Permanent(errors.New("strategy: traffic non disponibile su questo server"))
	}
	plan, err := checkRollout(*d, w.health, w.traffic)
	if err != nil {
		return fmt.Errorf("rollout: %w", err)
	}
	prefix, perPass, suffix := splitPlan(req.Plan)
	outputs := map[string]pipeline.Output{}
	var started []startedStep
	for _, ps := range prefix {
		out, st, skipped, err := w.execStep(ctx, id, ev, d, ps, ps.Name, nil, outputs, res)
		if st != nil {
			started = append(started, *st)
		}
		if err != nil {
			return w.compensate(ctx, id, ev, d, res, started, ps.Name, err)
		}
		if !skipped {
			outputs[ps.Name] = out
		}
	}
	rs := newTrafficState(d, plan)
	res.Rollout = rs
	w.publishRollout(ctx, rs)
	var runs []*trafficRun
	base := 0
	for si, stage := range plan.Stages {
		if !d.DryRun {
			if err := w.waitFreeze(ctx, d, stage, rs); err != nil {
				return err
			}
		}
		for _, p := range stage.Passes {
			dd := *d
			dd.Deploy.Targets = p.Targets
			run := &trafficRun{pass: p, base: base}
			if multiPass(d) {
				run.sfx = "@" + p.Name
			}
			runs = append(runs, run)
			if err := w.trafficPass(ctx, id, req, &dd, ev, res, rs, perPass, outputs, run); err != nil {
				return w.trafficFailure(ctx, id, d, ev, res, rs, runs, err)
			}
			base += len(d.Rollout.Traffic.Steps)
		}
		if multiPass(d) && stage.LastOfWave && si < len(plan.Stages)-1 && !d.DryRun && slices.Contains(d.Rollout.ApprovalsBetween, stage.Wave) {
			if err := w.waitRolloutApproval(ctx, id, req, "l'ondata "+stage.Wave, service.RolloutPausedApproval, rs); err != nil {
				return w.trafficFailure(ctx, id, d, ev, res, rs, runs, err)
			}
		}
	}
	started = nil
	for _, ps := range suffix {
		out, st, skipped, err := w.execStep(ctx, id, ev, d, ps, ps.Name, nil, outputs, res)
		if st != nil {
			started = append(started, *st)
		}
		if err != nil {
			return w.compensate(ctx, id, ev, d, res, started, ps.Name, err)
		}
		if !skipped {
			outputs[ps.Name] = out
		}
	}
	rs.Status, rs.Stage = service.RolloutSucceeded, ""
	w.publishRollout(ctx, rs)
	for _, run := range runs {
		prep := run.in.Outputs[steps.TrafficOutput]
		if !d.DryRun && run.promoted && prep[steps.OutMode] == steps.ModePlugin && prep[steps.OutHasStable] == "true" {
			w.scheduleRetire(ctx, id, req, run)
		}
	}
	return nil
}

// trafficPass esegue il rilascio affiancato sui target del passo (d ha i soli target del passo).
func (w *Worker) trafficPass(ctx dbos.Context, id string, req Request, d *descriptor.Descriptor, ev *events, res *Result,
	rs *RolloutState, perPass pipeline.Plan, outputs map[string]pipeline.Output, run *trafficRun) error {
	tr := d.Rollout.Traffic
	bo := maps.Clone(outputs)
	in := pipeline.Input{DeploymentID: id, Descriptor: d, Outputs: bo}
	prep, err := w.trafficOp(ctx, ev, d, res, "traffic:prepare"+run.sfx, false, in, w.traffic.Prepare)
	if err != nil {
		return err
	}
	bo[steps.TrafficOutput] = prep
	run.in, run.prepared = in, true
	t := rs.Traffic
	t.Pass, t.Router, t.Stable, t.Candidate = "", prep[steps.OutRouter], prep[steps.OutStable], prep[steps.OutCandidate]
	t.Weight, t.Testers, t.Promoted = 0, false, false
	if multiPass(d) {
		t.Pass = run.pass.Name
	}
	targets := steps.CandidateTargets(d, prep)
	if targets != nil {
		hosts := make([]string, len(targets))
		for i, tg := range targets {
			hosts[i] = tg.Host
		}
		for i := range tr.Steps {
			rs.Stages[run.base+i].Passes[0].Hosts = hosts
		}
	}
	w.publishRollout(ctx, rs)
	tracks := &rollout.GateInput{Track: prep[steps.OutCandidate], StableTrack: prep[steps.OutStable], Version: d.Artifact.Version}
	hasStable := prep[steps.OutHasStable] == "true"

	act := slices.IndexFunc(perPass, func(ps pipeline.PlannedStep) bool { return ps.Name == "activation" })
	if act < 0 {
		act = len(perPass)
	}
	exec := func(list pipeline.Plan) error {
		for _, ps := range list {
			out, st, skipped, err := w.execStep(ctx, id, ev, d, ps, ps.Name+run.sfx, targets, bo, res)
			if st != nil {
				run.started = append(run.started, *st)
			}
			if err != nil {
				return err
			}
			if !skipped {
				bo[ps.Name] = out
			}
		}
		return nil
	}
	if err := exec(perPass[:act]); err != nil {
		return err
	}
	if d.DryRun {
		// In dry-run il traffico non si sposta: gli step restanti si simulano o si saltano.
		return exec(perPass[act:])
	}
	if tr.Testers != nil && hasStable {
		if _, err := w.trafficOp(ctx, ev, d, res, "traffic:testers"+run.sfx, false, in, w.traffic.Testers); err != nil {
			return err
		}
		t.Testers = true
		w.publishRollout(ctx, rs)
	}
	if slices.Contains(d.Rollout.ApprovalsBetween, descriptor.StageZero) {
		if err := w.waitRolloutApproval(ctx, id, req, "i test a peso 0", service.RolloutPausedApproval, rs); err != nil {
			return err
		}
	}
	last := 0
	for k, pct := range tr.Steps {
		i := run.base + k
		if !hasStable && pct < 100 {
			// Primo rilascio: non c'è traffico da dividere, la candidata riceve subito tutto.
			rs.Stages[i].State = service.StageSucceeded
			continue
		}
		rs.Stage, rs.Stages[i].State = rs.Stages[i].Name, service.StageRunning
		w.publishRollout(ctx, rs)
		if hasStable && tr.OnDrift != descriptor.OnDriftIgnore {
			if err := w.checkDrift(ctx, id, req, d, ev, res, rs, in, run.sfx, pct, last); err != nil {
				return err
			}
		}
		if tr.Replicas == descriptor.ReplicasProportional && hasStable {
			if _, err := w.trafficOp(ctx, ev, d, res, "traffic:scale@"+strconv.Itoa(pct)+run.sfx, false, in, func(c context.Context, in pipeline.Input) (pipeline.Output, error) {
				return w.traffic.Scale(c, in, pct)
			}); err != nil {
				return err
			}
		}
		if _, err := w.trafficOp(ctx, ev, d, res, "traffic@"+strconv.Itoa(pct)+run.sfx, false, in, func(c context.Context, in pipeline.Input) (pipeline.Output, error) {
			return w.traffic.SetWeight(c, in, pct)
		}); err != nil {
			return err
		}
		last, t.Weight = pct, pct
		bake := time.Duration(0)
		if pct < 100 {
			bake = tr.BakeDuration()
		}
		rs.Stages[i].State = service.StageBaking
		w.publishRollout(ctx, rs)
		p := run.pass
		p.Targets = cmpTargets(targets, d.Deploy.Targets)
		stage := rollout.Stage{Name: "traffic@" + strconv.Itoa(pct) + run.sfx, Wave: p.Wave, Bake: bake, Passes: []rollout.Pass{p}}
		if err := w.healthGate(ctx, id, d, stage, i, rs, bake, tracks); err != nil {
			return err
		}
		rs.Stages[i].State = service.StageSucceeded
		w.publishRollout(ctx, rs)
		ev.send(d, callback.Event{Type: descriptor.CallbackEventStep, Step: "stage:" + rs.Stages[i].Name, Status: callback.StatusSucceeded})
		if pct < 100 && slices.Contains(d.Rollout.ApprovalsBetween, strconv.Itoa(pct)) {
			if err := w.waitRolloutApproval(ctx, id, req, "il gradino del "+rs.Stages[i].Name, service.RolloutPausedApproval, rs); err != nil {
				return err
			}
		}
	}
	if _, err := w.trafficOp(ctx, ev, d, res, "traffic:promote"+run.sfx, false, in, w.traffic.Promote); err != nil {
		return err
	}
	t.Weight, t.Promoted, run.promoted = 100, true, true
	w.publishRollout(ctx, rs)
	return exec(perPass[act:])
}

func cmpTargets(a, b []descriptor.Target) []descriptor.Target {
	if a != nil {
		return a
	}
	return b
}

// checkDrift verifica che i pesi del router siano quelli impostati da manent: se qualcuno li
// ha cambiati, il rollout si ferma in attesa di una decisione invece di sovrascriverli.
func (w *Worker) checkDrift(ctx dbos.Context, id string, req Request, d *descriptor.Descriptor, ev *events, res *Result,
	rs *RolloutState, in pipeline.Input, sfx string, pct, last int) error {
	out, err := w.trafficOp(ctx, ev, d, res, "traffic:weight@"+strconv.Itoa(pct)+sfx, true, in, w.traffic.Weight)
	if err != nil {
		return err
	}
	cur, _ := strconv.Atoi(out["weight"])
	if cur == last {
		return nil
	}
	msg := fmt.Sprintf("pesi cambiati fuori da manent: la candidata riceve il %d%%, atteso %d%%", cur, last)
	w.logger.Warn("deriva dei pesi del router", "deployment", id, "weight", cur, "expected", last)
	return w.waitRolloutApproval(ctx, id, req, msg, service.RolloutPausedDrift, rs)
}

// trafficFailure riporta il traffico alla versione stabile (salvo onFailure: stop): con
// rollback-wave solo nel passo fallito, con rollback-all in tutti i passi già eseguiti, in
// ordine inverso. Poi compensa gli step avviati di quei passi.
func (w *Worker) trafficFailure(ctx dbos.Context, id string, d *descriptor.Descriptor, ev *events, res *Result,
	rs *RolloutState, runs []*trafficRun, cause error) error {
	rs.Status = service.RolloutFailed
	if i := slices.IndexFunc(rs.Stages, func(s RolloutStage) bool { return s.Name == rs.Stage }); i >= 0 && rs.Stages[i].State != service.StageSucceeded {
		rs.Stages[i].State, rs.Stages[i].Error = service.StageFailed, cause.Error()
	}
	w.publishRollout(ctx, rs)
	onFailure := d.Rollout.OnFailureOrDefault()
	if d.DryRun || onFailure == descriptor.OnFailureStop || len(runs) == 0 || ctx.Err() != nil {
		return cause
	}
	from := 0
	if onFailure == descriptor.OnFailureRollbackWave {
		from = len(runs) - 1
	}
	rbErr := &RollbackError{Err: cause, Status: RollbackSkipped}
	for r := len(runs) - 1; r >= from; r-- {
		run := runs[r]
		if !run.prepared {
			continue
		}
		out, err := w.trafficOp(ctx, ev, run.in.Descriptor, res, "traffic:revert"+run.sfx, false, run.in, w.traffic.Revert)
		if err != nil {
			rbErr.Status, rbErr.Cause = RollbackFailed, err
			rs.Rollback = RollbackFailed
			w.publishRollout(ctx, rs)
			return rbErr
		}
		if out[pipeline.OutRollback] == pipeline.RollbackCompleted {
			rbErr.Status = RollbackCompleted
			rs.Traffic.Weight, rs.Traffic.Promoted, rs.Traffic.Testers = 0, false, false
		}
		if len(run.started) > 0 {
			if rb, ok := errors.AsType[*RollbackError](w.undo(ctx, id, ev, run.in.Descriptor, res, run.started, cause)); ok && rb.Status == RollbackFailed {
				rbErr.Status, rbErr.Cause = RollbackFailed, rb.Cause
				rs.Rollback = RollbackFailed
				w.publishRollout(ctx, rs)
				return rbErr
			}
		}
		for i := run.base; i < run.base+len(d.Rollout.Traffic.Steps) && i < len(rs.Stages); i++ {
			if rs.Stages[i].State == service.StageSucceeded {
				rs.Stages[i].State = service.StageRolledBack
			}
		}
	}
	rs.Rollback = rbErr.Status
	w.publishRollout(ctx, rs)
	return rbErr
}

// retireInput è l'input del workflow che ridimensiona la versione sostituita.
type retireInput struct {
	DeploymentID string                     `json:"deployment_id"`
	Descriptor   descriptor.Descriptor      `json:"descriptor"`
	Outputs      map[string]pipeline.Output `json:"outputs"`
	RequestedBy  string                     `json:"requested_by,omitempty"`
}

// scheduleRetire avvia, con un ritardo pari a scaleDownDelay, il workflow figlio che
// ridimensiona la versione sostituita nel passo (ID <id>.retire, o <id>.retire.<passo> con le
// ondate). Gira sulla coda delle approvazioni: non occupa uno slot dell'ambiente, così il
// deploy si conclude subito.
func (w *Worker) scheduleRetire(ctx dbos.Context, id string, req Request, run *trafficRun) {
	q, err := dbos.RetrieveQueue(w.dbos, approvalQueue(w.queueName))
	if err != nil {
		w.logger.Warn("ridimensionamento della versione sostituita non pianificato", "deployment", id, "error", err)
		return
	}
	wid := id + ".retire"
	if run.sfx != "" {
		wid += "." + run.pass.Name
	}
	d := run.in.Descriptor
	opts := []dbos.WorkflowOption{dbos.WithWorkflowID(wid), dbos.WithQueue(q)}
	if delay := d.Rollout.Traffic.ScaleDownDelayDuration(); delay > 0 {
		opts = append(opts, dbos.WithDelay(delay))
	}
	if req.RequestedBy != "" {
		opts = append(opts, dbos.WithAuthenticatedUser(req.RequestedBy))
	}
	keep := map[string]pipeline.Output{}
	for _, k := range []string{"artifacts", steps.TrafficOutput} {
		if o, ok := run.in.Outputs[k]; ok {
			keep[k] = o
		}
	}
	if _, err := dbos.RunWorkflow(ctx, w.retireWorkflow, retireInput{DeploymentID: id, Descriptor: *d, Outputs: keep, RequestedBy: req.RequestedBy}, opts...); err != nil {
		w.logger.Warn("ridimensionamento della versione sostituita non pianificato", "deployment", id, "error", err)
	}
}

// retireWorkflow ridimensiona la versione sostituita. Il plugin non tocca la versione in
// esercizio, anche se nel frattempo un altro rilascio l'ha cambiata.
func (w *Worker) retireWorkflow(ctx dbos.Context, in retireInput) (pipeline.Output, error) {
	if w.traffic == nil {
		return nil, errors.New("strategy: traffic non disponibile su questo worker")
	}
	ev := &events{w: w, ctx: ctx, id: in.DeploymentID, requestedBy: in.RequestedBy, silent: true}
	var res Result
	pin := pipeline.Input{DeploymentID: in.DeploymentID, Descriptor: &in.Descriptor, Outputs: in.Outputs}
	return w.trafficOp(ctx, ev, &in.Descriptor, &res, "traffic:retire", false, pin, w.traffic.Retire)
}

// trafficMeta descrive la configurazione di traffico nell'output dello step config.
func trafficMeta(d descriptor.Descriptor) string {
	var parts []string
	if c := d.Traffic; c != nil {
		if c.Pool != nil {
			parts = append(parts, "pool "+c.Pool.Provider)
		}
		if c.Router != nil {
			parts = append(parts, "router "+c.Router.Provider)
		}
	}
	return strings.Join(parts, ", ")
}
