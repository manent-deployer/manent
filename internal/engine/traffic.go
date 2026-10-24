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

	"github.com/manent-deployer/manent/internal/callback"
	"github.com/manent-deployer/manent/internal/descriptor"
	"github.com/manent-deployer/manent/internal/pipeline"
	"github.com/manent-deployer/manent/internal/rollout"
	"github.com/manent-deployer/manent/internal/service"
	"github.com/manent-deployer/manent/internal/steps"
)

// Stable name of the child workflow that scales down the version replaced by a percentage
// rollout, after rollout.traffic.scaleDownDelay.
const retireWorkflowName = "manent.traffic-retire"

// TrafficOps are the load balancer and router operations, run by the engine inside DBOS
// steps (implemented by internal/steps).
type TrafficOps interface {
	// Check checks the traffic configuration of the effective descriptor, without side effects.
	Check(d *descriptor.Descriptor) error
	// PoolStatus reads remaining connections (phase "drain") or health (phase "restore") of the
	// input targets.
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

// awaitsDecision reports a rollout state that waits for a decision (approve/reject).
func awaitsDecision(status string) bool {
	return status == service.RolloutPausedApproval || status == service.RolloutPausedDrift
}

// passTrack ties a pass to the published rollout state (nil in child workflows).
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

// trafficOp runs a traffic operation as a DBOS step, with logs, spans and events.
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

// drainBatch drains the targets of a batch: the drain step checks the pool capacity (if
// insufficient the engine waits and retries, up to drain_timeout) and removes the members,
// then a durable wait waits for the connections to end.
func (w *Worker) drainBatch(ctx dbos.Context, id string, ev *events, d *descriptor.Descriptor, batch []descriptor.Target,
	suffix string, outputs map[string]pipeline.Output, res *Result, track *passTrack) (*startedStep, error) {
	if w.traffic == nil {
		return nil, pipeline.Permanent(errors.New("traffic.pool: integration not available on this server"))
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
			return nil, fmt.Errorf("drain not completed within %s: %s", timeout, out[steps.OutDetail])
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
			// The batch is not touched: it goes back in service before stopping the pass.
			if _, rerr := w.restoreBatch(ctx, id, ev, d, batch, suffix, outputs, res, track); rerr != nil {
				return started, errors.Join(err, rerr)
			}
			return nil, err
		}
	}
	return started, nil
}

// restoreBatch puts the batch targets back in service and waits for the load balancer to see
// them healthy: a target never goes back in service without being healthy.
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

// poolWait waits, with durable reads and pauses, for the connections to end (drain) or for
// the targets to be healthy (restore). On timeout: for draining with on_timeout proceed the
// deploy goes on (the remaining connections are closed by the restart), otherwise it is an error.
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
				w.logger.Warn("draining beyond the timeout: the deploy goes on", "deployment", id, "pending", out["pending"], "timeout", timeout)
				return nil
			}
			return fmt.Errorf("%s: targets not ready within %s (%s)", phase, timeout, out["pending"])
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

// trafficPlan is the plan of a percentage rollout: with waves the passes of the wave plan (one
// cluster or region at a time), each with all steps; without, a single pass with all targets.
func trafficPlan(d descriptor.Descriptor) (rollout.Plan, error) {
	if len(d.Rollout.Waves) > 0 {
		return rollout.Build(d.Deploy.Targets, d.Rollout)
	}
	p := rollout.Pass{Name: "traffic", Wave: "traffic", Targets: d.Deploy.Targets, Batches: [][]descriptor.Target{d.Deploy.Targets}}
	return rollout.Plan{Stages: []rollout.Stage{{Name: "traffic", Wave: "traffic", Passes: []rollout.Pass{p}, LastOfWave: true}}}, nil
}

// multiPass reports a percentage rollout in waves: step and stage names carry the pass.
func multiPass(d *descriptor.Descriptor) bool { return len(d.Rollout.Waves) > 0 }

// stageName is the stage name of a step ("25%", or "<pass>/25%" with waves).
func stageName(d *descriptor.Descriptor, pass string, percent int) string {
	name := strconv.Itoa(percent) + "%"
	if multiPass(d) {
		name = pass + "/" + name
	}
	return name
}

// newTrafficState creates the state of a percentage rollout: one stage per step of every pass.
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

// trafficRun is the rollout state of a pass, for the return on error.
type trafficRun struct {
	pass rollout.Pass
	// base is the index of the pass's first stage in RolloutState.Stages.
	base int
	// sfx is the suffix of the pass's step names ("" without waves).
	sfx      string
	in       pipeline.Input
	prepared bool
	promoted bool
	started  []startedStep
}

// runTraffic runs a percentage rollout: initial steps; for every pass (just one without
// waves) preparation (stable version, candidate slot), the steps from deploy to activation
// excluded on the candidate without traffic, testers, steps (weights, bake, gate,
// approvals), promotion and activation; finally the final steps. A child workflow per pass
// scales down the replaced version after scaleDownDelay.
func (w *Worker) runTraffic(ctx dbos.Context, id string, req Request, d *descriptor.Descriptor, ev *events, res *Result) error {
	if w.traffic == nil {
		return pipeline.Permanent(errors.New("strategy: traffic not available on this server"))
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
			if err := w.waitFreeze(ctx, req, d, stage, rs); err != nil {
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
			if err := w.waitRolloutApproval(ctx, id, req, "the wave "+stage.Wave, service.RolloutPausedApproval, rs); err != nil {
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

// trafficPass runs the side-by-side release on the pass targets (d has only the pass targets).
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
		// In dry-run traffic does not move: the remaining steps are simulated or skipped.
		return exec(perPass[act:])
	}
	if tr.Testers != nil && hasStable {
		if _, err := w.trafficOp(ctx, ev, d, res, "traffic:testers"+run.sfx, false, in, w.traffic.Testers); err != nil {
			return err
		}
		t.Testers = true
		w.publishRollout(ctx, rs)
	}
	// On the first release the intermediate steps, and their approvals, are skipped: with the
	// governance approvals moved into the rollout they are asked before any traffic.
	if slices.Contains(d.Rollout.ApprovalsBetween, descriptor.StageZero) || (!hasStable && req.Approval.InRollout() && !d.DryRun) {
		if err := w.waitRolloutApproval(ctx, id, req, "the tests at weight 0", service.RolloutPausedApproval, rs); err != nil {
			return err
		}
	}
	last := 0
	for k, pct := range tr.Steps {
		i := run.base + k
		if !hasStable && pct < 100 {
			// First release: there is no traffic to split, the candidate gets everything at once.
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
		gt := tracks
		if pct == 100 || !hasStable {
			// The stable version receives no traffic: a comparison would read an empty series.
			g := *tracks
			g.StableTrack, gt = "", &g
		}
		if err := w.healthGate(ctx, id, d, stage, i, rs, bake, gt); err != nil {
			return err
		}
		rs.Stages[i].State = service.StageSucceeded
		w.publishRollout(ctx, rs)
		ev.send(d, callback.Event{Type: descriptor.CallbackEventStep, Step: "stage:" + rs.Stages[i].Name, Status: callback.StatusSucceeded})
		if pct < 100 && slices.Contains(d.Rollout.ApprovalsBetween, strconv.Itoa(pct)) {
			if err := w.waitRolloutApproval(ctx, id, req, "the step at "+rs.Stages[i].Name, service.RolloutPausedApproval, rs); err != nil {
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

// checkDrift checks that the router weights are those set by manent: if someone changed them,
// the rollout stops waiting for a decision instead of overwriting them.
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
	msg := fmt.Sprintf("weights changed outside manent: the candidate receives %d%%, expected %d%%", cur, last)
	w.logger.Warn("router weights drift", "deployment", id, "weight", cur, "expected", last)
	return w.waitRolloutApproval(ctx, id, req, msg, service.RolloutPausedDrift, rs)
}

// trafficFailure brings traffic back to the stable version (unless onFailure: stop): with
// rollback-wave only in the failed pass, with rollback-all in all passes already run, in
// reverse order. Then it compensates the started steps of those passes.
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

// retireInput is the input of the workflow that scales down the replaced version.
type retireInput struct {
	DeploymentID string                     `json:"deployment_id"`
	Descriptor   descriptor.Descriptor      `json:"descriptor"`
	Outputs      map[string]pipeline.Output `json:"outputs"`
	RequestedBy  string                     `json:"requested_by,omitempty"`
}

// scheduleRetire starts, with a delay of scaleDownDelay, the child workflow that scales down
// the version replaced in the pass (ID <id>.retire, or <id>.retire.<pass> with waves). It
// runs on the approvals queue: it takes no environment slot, so the deploy ends at once.
func (w *Worker) scheduleRetire(ctx dbos.Context, id string, req Request, run *trafficRun) {
	q, err := dbos.RetrieveQueue(w.dbos, approvalQueue(w.queueName))
	if err != nil {
		w.logger.Warn("scale-down of the replaced version not scheduled", "deployment", id, "error", err)
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
		w.logger.Warn("scale-down of the replaced version not scheduled", "deployment", id, "error", err)
	}
}

// retireWorkflow scales down the replaced version. The plugin does not touch the version in
// service, even if another release changed it meanwhile.
func (w *Worker) retireWorkflow(ctx dbos.Context, in retireInput) (pipeline.Output, error) {
	if w.traffic == nil {
		return nil, errors.New("strategy: traffic not available on this worker")
	}
	ev := &events{w: w, ctx: ctx, id: in.DeploymentID, requestedBy: in.RequestedBy, silent: true}
	var res Result
	pin := pipeline.Input{DeploymentID: in.DeploymentID, Descriptor: &in.Descriptor, Outputs: in.Outputs}
	return w.trafficOp(ctx, ev, &in.Descriptor, &res, "traffic:retire", false, pin, w.traffic.Retire)
}

// trafficMeta describes the traffic configuration in the output of the config step.
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
