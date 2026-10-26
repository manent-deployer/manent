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
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/manent-deployer/manent/internal/callback"
	"github.com/manent-deployer/manent/internal/deploylog"
	"github.com/manent-deployer/manent/internal/descriptor"
	"github.com/manent-deployer/manent/internal/governance"
	"github.com/manent-deployer/manent/internal/pipeline"
	"github.com/manent-deployer/manent/internal/rollout"
	"github.com/manent-deployer/manent/internal/service"
	"github.com/manent-deployer/manent/internal/telemetry"
)

// Stable name of the child workflow that runs a rollout pass on its region's queue.
const passWorkflowName = "manent.deploy-pass"

// gateWorkflowName is the child workflow that runs the health gate probes on the targets of a
// region, on a worker of that region: with separate networks per region the orchestrator
// cannot reach them.
const gateWorkflowName = "manent.gate-probe"

const (
	// rolloutEvent is the key of the event with the rollout state.
	rolloutEvent = "rollout"
	// rolloutApprovalTopic is the topic of the decisions on approvals between waves.
	rolloutApprovalTopic = "rollout-approval"
)

// Contract aliases for the rollout state.
type (
	RolloutState = service.RolloutState
	RolloutStage = service.RolloutStage
	RolloutPass  = service.RolloutPass
)

// passStepNames are the steps that work on the targets: from the first to the last of these,
// the plan steps run for every rollout pass and batch. The ones before (secrets, artifact,
// QA) run once, the ones after at the end.
var passStepNames = []string{"deploy", "post-test", "activation"}

// splitPlan splits the plan into initial steps, per-pass steps and final steps.
func splitPlan(plan pipeline.Plan) (prefix, perPass, suffix pipeline.Plan) {
	first, last := -1, -1
	for i, ps := range plan {
		if slices.Contains(passStepNames, ps.Name) {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 {
		return plan, nil, nil
	}
	return plan[:first], plan[first : last+1], plan[last+1:]
}

// regionQueue is the queue of a region's passes: only that region's workers listen to it.
func regionQueue(deployQueue, region string) string { return deployQueue + "." + region }

// newRolloutState creates the initial rollout state from the plan.
func newRolloutState(p rollout.Plan, r *descriptor.Rollout) *RolloutState {
	st := &RolloutState{Status: service.RolloutRunning, OnFailure: r.OnFailureOrDefault()}
	for _, s := range p.Stages {
		rs := RolloutStage{Name: s.Name, Wave: s.Wave, State: service.StagePending}
		for _, ps := range s.Passes {
			rs.Passes = append(rs.Passes, RolloutPass{Name: ps.Name, Region: ps.Region, AZ: ps.AZ, Hosts: ps.Hosts()})
		}
		st.Stages = append(st.Stages, rs)
	}
	return st
}

// checkRollout checks the rollout of the effective descriptor: plan consistent with the
// targets, rollback enabled if required by onFailure, runnable health checks.
func checkRollout(d descriptor.Descriptor, health rollout.Checker, ops TrafficOps) (rollout.Plan, error) {
	if d.Traffic != nil {
		if ops == nil {
			return rollout.Plan{}, errors.New("traffic: load balancer and router integration not available on this server")
		}
		if err := ops.Check(&d); err != nil {
			return rollout.Plan{}, err
		}
	}
	if d.Rollout.IsTraffic() {
		if ops == nil {
			return rollout.Plan{}, errors.New("strategy: traffic not available on this server")
		}
		if h := d.Rollout.Health; h != nil {
			if health == nil {
				return rollout.Plan{}, errors.New("rollout.health: health gate not configured on this server")
			}
			if err := health.Supports(h); err != nil {
				return rollout.Plan{}, err
			}
		}
		return trafficPlan(d)
	}
	p, err := rollout.Build(d.Deploy.Targets, d.Rollout)
	if err != nil {
		return p, err
	}
	if d.Rollout.OnFailureOrDefault() != descriptor.OnFailureStop && !d.Rollback.IsEnabled() {
		return p, fmt.Errorf("rollout.onFailure %s requires rollback enabled (rollback.enabled) for artifact type %q", d.Rollout.OnFailure, d.Artifact.Type)
	}
	if h := d.Rollout.Health; h != nil {
		if health == nil {
			return p, errors.New("rollout.health: health gate not configured on this server")
		}
		if err := health.Supports(h); err != nil {
			return p, err
		}
	}
	return p, nil
}

// passRun is a run pass, with the steps to compensate on rollback.
type passRun struct {
	pass    rollout.Pass
	routed  bool
	started []startedStep
}

// runRollout runs the wave plan: initial steps, then for every stage the passes (inline or on
// the region workers), the observation time with the health gate and, if configured, an
// approval; finally the final steps. The state is published with SetEvent.
func (w *Worker) runRollout(ctx dbos.Context, id string, req Request, d *descriptor.Descriptor, ev *events, res *Result) error {
	if d.Rollout.IsTraffic() {
		return w.runTraffic(ctx, id, req, d, ev, res)
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
	rs := newRolloutState(plan, d.Rollout)
	res.Rollout = rs
	w.publishRollout(ctx, rs)
	var done [][]passRun
	for i, stage := range plan.Stages {
		if !d.DryRun {
			if err := w.waitFreeze(ctx, req, d, stage, rs); err != nil {
				return err
			}
		}
		rs.Stage, rs.Stages[i].State = stage.Name, service.StageRunning
		w.publishRollout(ctx, rs)
		run, err := w.runStage(ctx, id, req, d, ev, perPass, stage, outputs, res, rs, i)
		done = append(done, run)
		if err == nil && !d.DryRun {
			rs.Stages[i].State = service.StageBaking
			w.publishRollout(ctx, rs)
			err = w.healthGate(ctx, id, d, stage, i, rs, stage.Bake, nil)
		}
		if err != nil {
			return w.rolloutFailure(ctx, id, d, ev, res, done, rs, i, err)
		}
		rs.Stages[i].State = service.StageSucceeded
		w.publishRollout(ctx, rs)
		ev.send(d, callback.Event{Type: descriptor.CallbackEventStep, Step: "stage:" + stage.Name, Status: callback.StatusSucceeded})
		if stage.LastOfWave && i < len(plan.Stages)-1 && !d.DryRun && slices.Contains(d.Rollout.ApprovalsBetween, stage.Wave) {
			if err := w.waitRolloutApproval(ctx, id, req, "the wave "+stage.Wave, service.RolloutPausedApproval, rs); err != nil {
				return w.rolloutFailure(ctx, id, d, ev, res, done, rs, i, err)
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
	return nil
}

// publishRollout publishes the rollout state, read by Status and the client.
func (w *Worker) publishRollout(ctx dbos.Context, rs *RolloutState) {
	if err := dbos.SetEvent(ctx, rolloutEvent, *rs); err != nil {
		w.logger.Warn("publishing the rollout state failed", "error", err)
	}
}

// runStage runs the passes of a stage: those routed to their region as child workflows, in
// parallel; the others inline, one after the other. It waits for all of them to end.
func (w *Worker) runStage(ctx dbos.Context, id string, req Request, d *descriptor.Descriptor, ev *events, steps pipeline.Plan,
	stage rollout.Stage, outputs map[string]pipeline.Output, res *Result, rs *RolloutState, si int) ([]passRun, error) {
	run := make([]passRun, len(stage.Passes))
	type child struct {
		i  int
		id string
		h  dbos.WorkflowHandle[passOutput]
	}
	var children []child
	var firstErr error
	for i, p := range stage.Passes {
		run[i].pass = p
		if w.regionRouting && p.Region != "" {
			childID, h, err := w.startPass(ctx, id, req, d, ev, steps, p, outputs, nil, "")
			if err != nil {
				firstErr = cmpErr(firstErr, fmt.Errorf("pass %s: %w", p.Name, err))
				continue
			}
			run[i].routed = true
			children = append(children, child{i: i, id: childID, h: h})
			continue
		}
		st, err := w.runPass(ctx, id, ev, d, steps, p, outputs, res, &passTrack{rs: rs, pass: &rs.Stages[si].Passes[i]})
		run[i].started = st
		w.passFinished(d, p, err)
		if err != nil {
			return run, fmt.Errorf("pass %s: %w", p.Name, err)
		}
	}
	for _, c := range children {
		out, err := c.h.GetResult()
		w.copyPassLogs(ctx, c.id)
		run[c.i].started = out.Started
		res.Steps = append(res.Steps, out.Steps...)
		if err == nil && out.Error != "" {
			err = errors.New(out.Error)
		}
		w.passFinished(d, run[c.i].pass, err)
		if err != nil {
			firstErr = cmpErr(firstErr, fmt.Errorf("pass %s: %w", run[c.i].pass.Name, err))
		}
	}
	return run, firstErr
}

func cmpErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

// passFinished records the outcome of a pass in the metrics.
func (w *Worker) passFinished(d *descriptor.Descriptor, p rollout.Pass, err error) {
	outcome := "succeeded"
	if err != nil {
		outcome = "failed"
	}
	w.metrics.PassFinished(d.Application, d.Environment, p.Region, p.AZ, outcome)
}

// runPass runs the per-pass steps on the batches of a pass, one batch after the other. Every
// batch sees the outputs of the initial steps and of its own previous steps.
func (w *Worker) runPass(ctx dbos.Context, id string, ev *events, d *descriptor.Descriptor, steps pipeline.Plan,
	p rollout.Pass, outputs map[string]pipeline.Output, res *Result, track *passTrack) ([]startedStep, error) {
	var started []startedStep
	// With a pool every batch is drained from the load balancer before the deploy and put back
	// in service before activation (or at the end).
	pool := d.Traffic != nil && d.Traffic.Pool != nil && !d.DryRun
	for b, batch := range p.Batches {
		bo := maps.Clone(outputs)
		if bo == nil {
			// In the child workflow an empty map arrives as nil (omitted in JSON).
			bo = map[string]pipeline.Output{}
		}
		suffix := "@" + p.Name + "#" + strconv.Itoa(b)
		restored := !pool
		if pool {
			st, err := w.drainBatch(ctx, id, ev, d, batch, suffix, bo, res, track)
			if st != nil {
				started = append(started, *st)
			}
			if err != nil {
				return started, err
			}
		}
		for _, ps := range steps {
			if !restored && ps.Name == "activation" {
				st, err := w.restoreBatch(ctx, id, ev, d, batch, suffix, bo, res, track)
				if st != nil {
					started = append(started, *st)
				}
				if err != nil {
					return started, err
				}
				restored = true
			}
			label := ps.Name + suffix
			out, st, skipped, err := w.execStep(ctx, id, ev, d, ps, label, batch, bo, res)
			if st != nil {
				started = append(started, *st)
			}
			if err != nil {
				return started, err
			}
			if !skipped {
				bo[ps.Name] = out
			}
		}
		if !restored {
			st, err := w.restoreBatch(ctx, id, ev, d, batch, suffix, bo, res, track)
			if st != nil {
				started = append(started, *st)
			}
			if err != nil {
				return started, err
			}
		}
		// The batch is back in service: check it before touching the next one. The last batch is
		// left to the stage gate.
		if p.BatchGate && !d.DryRun && b < len(p.Batches)-1 {
			if err := w.batchGate(ctx, d, p, b); err != nil {
				return started, err
			}
		}
	}
	return started, nil
}

// passInput is the input of a pass's child workflow.
type passInput struct {
	DeploymentID string                     `json:"deployment_id"`
	Descriptor   descriptor.Descriptor      `json:"descriptor"`
	Steps        pipeline.Plan              `json:"steps,omitempty"`
	Pass         rollout.Pass               `json:"pass"`
	Outputs      map[string]pipeline.Output `json:"outputs,omitempty"`
	// Undo, with Started, asks to compensate the started steps instead of running the pass.
	Undo        bool          `json:"undo,omitempty"`
	Started     []startedStep `json:"started,omitempty"`
	Error       string        `json:"error,omitempty"`
	TraceParent string        `json:"trace_parent,omitempty"`
	// RequestedBy is the requester's identity, recorded in the audit of custom steps.
	RequestedBy string `json:"requested_by,omitempty"`
}

// passOutput is the outcome of a pass. A failed pass does not fail the child workflow: the
// orchestrator needs the started steps for a possible rollback.
type passOutput struct {
	Started  []startedStep `json:"started,omitempty"`
	Steps    []StepResult  `json:"steps,omitempty"`
	Error    string        `json:"error,omitempty"`
	Rollback string        `json:"rollback,omitempty"`
}

// startPass starts the child workflow of a pass on its region's queue, with a deterministic
// ID: after an orchestrator recovery the already started child is not duplicated.
func (w *Worker) startPass(ctx dbos.Context, id string, req Request, d *descriptor.Descriptor, ev *events, steps pipeline.Plan,
	p rollout.Pass, outputs map[string]pipeline.Output, undo []startedStep, cause string) (string, dbos.WorkflowHandle[passOutput], error) {
	qname := regionQueue(w.queueOf(id), p.Region)
	q, err := dbos.RetrieveQueue(w.dbos, qname)
	if err != nil {
		return "", nil, fmt.Errorf("no worker registered for region %s (queue %s): %w", p.Region, qname, err)
	}
	kind := "pass"
	if undo != nil {
		kind = "undo"
	}
	childID := id + "." + kind + "." + p.Name
	in := passInput{
		DeploymentID: id, Descriptor: *d, Steps: steps, Pass: p, Outputs: outputs,
		Undo: undo != nil, Started: undo, Error: cause,
		TraceParent: telemetry.TraceParent(trace.ContextWithSpanContext(context.Background(), ev.span)),
		RequestedBy: ev.requestedBy,
	}
	opts := []dbos.WorkflowOption{
		dbos.WithWorkflowID(childID), dbos.WithQueue(q),
		dbos.WithQueuePartitionKey(partitionKey(d) + "/" + p.Region),
	}
	if req.RequestedBy != "" {
		opts = append(opts, dbos.WithAuthenticatedUser(req.RequestedBy))
	}
	h, err := dbos.RunWorkflow(ctx, w.passWorkflow, in, opts...)
	return childID, h, err
}

// passWorkflow runs (or compensates) a pass on a region worker. The orchestrator sends the
// callback events; logs go to the child's stream and are copied to the deploy's at the end of
// the pass.
func (w *Worker) passWorkflow(ctx dbos.Context, in passInput) (passOutput, error) {
	_, span := telemetry.Tracer().Start(telemetry.WithTraceParent(context.Background(), in.TraceParent), "pass "+in.Pass.Name,
		trace.WithAttributes(attribute.String("manent.deployment.id", in.DeploymentID), attribute.String("manent.region", in.Pass.Region), attribute.String("manent.az", in.Pass.AZ)))
	defer span.End()
	ev := &events{w: w, ctx: ctx, id: in.DeploymentID, requestedBy: in.RequestedBy, silent: true, span: span.SpanContext()}
	var res Result
	var out passOutput
	d := &in.Descriptor
	if in.Undo {
		err := w.undo(ctx, in.DeploymentID, ev, d, &res, in.Started, errors.New(in.Error))
		if rb, ok := errors.AsType[*RollbackError](err); ok {
			out.Rollback = rb.Status
			if rb.Cause != nil {
				out.Error = rb.Cause.Error()
			}
		}
	} else {
		st, err := w.runPass(ctx, in.DeploymentID, ev, d, in.Steps, in.Pass, in.Outputs, &res, nil)
		out.Started = st
		if err != nil {
			out.Error = err.Error()
		}
	}
	out.Steps = res.Steps
	if w.cleaner != nil {
		// The artifact downloaded on this worker is no longer needed; if the worker also runs the
		// orchestrator, a later step downloads it again and verifies its sha256.
		if _, err := dbos.RunAsStep(ctx, func(c context.Context) (string, error) {
			return "", w.cleaner.Cleanup(c, in.DeploymentID)
		}, dbos.WithStepName("cleanup")); err != nil {
			w.logger.Warn("cleanup of the pass failed", "deployment", in.DeploymentID, "pass", in.Pass.Name, "error", err)
		}
	}
	return out, nil
}

// copyPassLogs copies into the deploy log the lines written by a pass's child workflow.
func (w *Worker) copyPassLogs(ctx dbos.Context, childID string) {
	if _, err := dbos.RunAsStep(ctx, func(c context.Context) (int, error) {
		return deploylog.Copy(c, w.dbos, childID)
	}, dbos.WithStepName("logs@"+childID)); err != nil {
		w.logger.Warn("copying the log of the pass failed", "workflow", childID, "error", err)
	}
}

// healthGate waits for the stage observation time, reading the health gate at every interval:
// it stops as soon as consecutive red checks reach failuresToStop and requires the last read
// to be green. Waits and reads are durable: after a recovery the bake already elapsed is not
// repeated.
func (w *Worker) healthGate(ctx dbos.Context, id string, d *descriptor.Descriptor, stage rollout.Stage, i int, rs *RolloutState,
	bake time.Duration, tracks *rollout.GateInput) error {
	h := d.Rollout.Health
	if h == nil {
		if bake > 0 {
			if _, err := dbos.Sleep(ctx, bake); err != nil {
				return err
			}
		}
		return nil
	}
	in := rollout.GateInput{Application: d.Application, Environment: d.Environment, Stage: stage.Name, Passes: stage.Passes, Health: *h}
	if tracks != nil {
		in.Track, in.StableTrack, in.Version = tracks.Track, tracks.StableTrack, tracks.Version
	}
	last, stopped, err := observe(ctx, bake, h, func(n int, elapsed time.Duration, final bool) (rollout.GateResult, error) {
		// Elapsed and Final come from the workflow, not from a clock: the read stays deterministic.
		in.Elapsed, in.Final = elapsed, final
		r, err := w.readGate(ctx, id, d, in, stage.Name+"#"+strconv.Itoa(n))
		if err != nil {
			return r, err
		}
		rs.Stages[i].Score = r.Score
		if !r.OK {
			rs.Stages[i].Gate = r.Failed()
			w.publishRollout(ctx, rs)
		}
		return r, nil
	})
	switch {
	case err != nil:
		return err
	case stopped:
		return fmt.Errorf("health gate red after stage %s: %s", stage.Name, last.Failed())
	case !last.OK:
		return fmt.Errorf("health gate red at the end of the bake of stage %s: %s", stage.Name, last.Failed())
	}
	rs.Stages[i].Gate = ""
	return nil
}

// observe runs the bake loop shared by the stage and batch gates: a durable sleep of at most
// one interval, then a read, until the bake is over. It stops early (stopped) as soon as
// consecutive red reads reach failuresToStop; otherwise the caller requires the last read to
// be green.
func observe(ctx dbos.Context, bake time.Duration, h *descriptor.Health,
	read func(n int, elapsed time.Duration, final bool) (rollout.GateResult, error)) (last rollout.GateResult, stopped bool, err error) {
	interval, limit := h.IntervalDuration(), max(1, h.FailuresToStop)
	fails := 0
	for n, elapsed := 1, time.Duration(0); ; n++ {
		if wait := min(interval, bake-elapsed); wait > 0 {
			if _, err := dbos.Sleep(ctx, wait); err != nil {
				return last, false, err
			}
			elapsed += wait
		}
		if last, err = read(n, elapsed, elapsed >= bake); err != nil {
			return last, false, err
		}
		if last.OK {
			fails = 0
		} else if fails++; fails >= limit {
			return last, true, nil
		}
		if elapsed >= bake {
			return last, false, nil
		}
	}
}

// batchGate observes a batch before the next one of the same pass: the probes read the batch
// targets for the pass's BatchBake. Metrics and alerts stay with the stage gate: they describe
// the whole pass, and are read by the orchestrator even when the pass runs in its region.
// Without probes the gate is only a durable wait. Read steps: gate@<pass>#<batch>.<n>.
func (w *Worker) batchGate(ctx dbos.Context, d *descriptor.Descriptor, p rollout.Pass, b int) error {
	var h descriptor.Health
	if d.Rollout.Health != nil {
		h = descriptor.Health{Probes: d.Rollout.Health.Probes, Interval: d.Rollout.Health.Interval, FailuresToStop: d.Rollout.Health.FailuresToStop}
	}
	if len(h.Probes) == 0 {
		if p.BatchBake > 0 {
			_, err := dbos.Sleep(ctx, p.BatchBake)
			return err
		}
		return nil
	}
	label := p.Name + "#" + strconv.Itoa(b)
	bp := p
	bp.Targets, bp.Batches = p.Batches[b], nil
	in := rollout.GateInput{Application: d.Application, Environment: d.Environment, Stage: label, Passes: []rollout.Pass{bp}, Health: h}
	last, stopped, err := observe(ctx, p.BatchBake, &h, func(n int, elapsed time.Duration, final bool) (rollout.GateResult, error) {
		in.Elapsed, in.Final = elapsed, final
		// Already on the pass's worker (orchestrator or region): the probes are read here.
		return w.checkGate(ctx, "gate@"+label+"."+strconv.Itoa(n), in)
	})
	switch {
	case err != nil:
		return err
	case stopped:
		return fmt.Errorf("health gate red after batch %s: %s", label, last.Failed())
	case !last.OK:
		return fmt.Errorf("health gate red at the end of the bake of batch %s: %s", label, last.Failed())
	}
	return nil
}

// readGate runs one health gate read. With region routing the probes of routed passes run in
// a child workflow on each region's queue (deterministic ID <id>.gate.<stage>#<n>.<region>),
// in parallel; metrics, alerts and probes of passes without a region stay on the orchestrator.
// As for passes, a region without live workers makes the read wait.
func (w *Worker) readGate(ctx dbos.Context, id string, d *descriptor.Descriptor, in rollout.GateInput, label string) (rollout.GateResult, error) {
	local := in
	type child struct {
		region string
		h      dbos.WorkflowHandle[rollout.GateResult]
	}
	var children []child
	if w.regionRouting && len(in.Health.Probes) > 0 {
		byRegion := map[string][]rollout.Pass{}
		var localPasses []rollout.Pass
		for _, p := range in.Passes {
			if p.Region != "" {
				byRegion[p.Region] = append(byRegion[p.Region], p)
			} else {
				localPasses = append(localPasses, p)
			}
		}
		if len(byRegion) > 0 {
			for _, region := range slices.Sorted(maps.Keys(byRegion)) {
				qname := regionQueue(w.queueOf(id), region)
				q, err := dbos.RetrieveQueue(w.dbos, qname)
				if err != nil {
					return rollout.GateResult{}, fmt.Errorf("no worker registered for region %s (queue %s): %w", region, qname, err)
				}
				gi := in
				gi.Passes, gi.Health = byRegion[region], descriptor.Health{Probes: in.Health.Probes}
				h, err := dbos.RunWorkflow(ctx, w.gateWorkflow, gi, dbos.WithWorkflowID(id+".gate."+label+"."+region),
					dbos.WithQueue(q), dbos.WithQueuePartitionKey(partitionKey(d)+"/"+region+"/gate"))
				if err != nil {
					return rollout.GateResult{}, fmt.Errorf("health gate in region %s: %w", region, err)
				}
				children = append(children, child{region: region, h: h})
			}
			// The orchestrator keeps the probes of passes without a region and the centralized checks
			// (metrics and alerts) of all passes.
			probes := local
			probes.Passes, probes.Health = localPasses, descriptor.Health{Probes: in.Health.Probes}
			local.Health.Probes = nil
			r, err := w.checkGate(ctx, "gate@"+label, local, probes)
			if err != nil {
				return r, err
			}
			parts := []rollout.GateResult{r}
			for _, c := range children {
				cr, err := c.h.GetResult()
				if err != nil {
					return rollout.GateResult{}, fmt.Errorf("health gate in region %s: %w", c.region, err)
				}
				parts = append(parts, cr)
			}
			return rollout.Merge(parts...), nil
		}
	}
	return w.checkGate(ctx, "gate@"+label, local)
}

// checkGate runs the given reads in a DBOS step and merges their outcome.
func (w *Worker) checkGate(ctx dbos.Context, name string, ins ...rollout.GateInput) (rollout.GateResult, error) {
	return dbos.RunAsStep(ctx, func(c context.Context) (rollout.GateResult, error) {
		// Only the red reads go to the deploy log: a green one every interval would bury the rest.
		c = deploylog.WithStep(c, name)
		cc, cancel := context.WithTimeout(c, w.healthTimeout)
		defer cancel()
		var parts []rollout.GateResult
		for _, in := range ins {
			if len(in.Passes) > 0 || in.Health.Alerts != nil {
				parts = append(parts, w.health.Check(cc, in))
			}
		}
		r := rollout.Merge(parts...)
		if !r.OK {
			w.logger.WarnContext(c, "health gate red", "reason", r.Failed())
		}
		return r, nil
	}, dbos.WithStepName(name))
}

// gateWorkflow runs the health gate probes on the targets of this worker's region.
func (w *Worker) gateWorkflow(ctx dbos.Context, in rollout.GateInput) (rollout.GateResult, error) {
	return w.checkGate(ctx, "gate", in)
}

// now reads the time in a step: the workflow stays deterministic after a recovery.
func now(ctx dbos.Context) (time.Time, error) {
	return dbos.RunAsStep(ctx, func(context.Context) (time.Time, error) { return time.Now().UTC(), nil }, dbos.WithStepName("now"))
}

// waitRolloutApproval waits for the approvals required after a wave. The deploy's governance
// rule applies (number of approvals, self-approval, expiry) or, without one, a single
// approval by another identity.
func (w *Worker) waitRolloutApproval(ctx dbos.Context, id string, req Request, after, status string, rs *RolloutState) error {
	rule := governance.Approvals{Required: 1, Timeout: governance.DefaultApprovalTimeout}
	if a := req.Approval; a != nil {
		rule.Required, rule.AllowSelf = max(1, a.Required), a.AllowSelf
		if a.Timeout > 0 {
			rule.Timeout = a.Timeout
		}
	}
	t, err := now(ctx)
	if err != nil {
		return err
	}
	st := ApprovalState{Status: ApprovalPending, Required: rule.Required, AllowSelf: rule.AllowSelf, ExpiresAt: t.Add(rule.Timeout)}
	if a := req.Approval; a != nil {
		st.Approvers = a.Approvers
	}
	rs.Status, rs.Paused, rs.Approval = status, "approval after "+after, &st
	if status == service.RolloutPausedDrift {
		rs.Paused = after
	}
	log := w.logger.With("deployment", id, "after", after)
	for st.Status == ApprovalPending {
		w.publishRollout(ctx, rs)
		t, err := now(ctx)
		if err != nil {
			return err
		}
		dec, err := dbos.Recv[Decision](ctx, rolloutApprovalTopic, max(st.ExpiresAt.Sub(t), time.Millisecond))
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			st.Status = ApprovalExpired
			continue
		case err != nil:
			return err
		}
		switch {
		case dec.Approver == "":
			continue
		case !rule.AllowSelf && dec.Approver == req.RequestedBy:
			log.Warn("decision discarded: self-approval not allowed", "approver", dec.Approver)
			continue
		case slices.ContainsFunc(st.Decisions, func(x Decision) bool { return x.Approver == dec.Approver }):
			continue
		}
		st.Decisions = append(st.Decisions, dec)
		switch {
		case !dec.Approve:
			st.Status = ApprovalRejected
		case len(st.Decisions) >= rule.Required:
			st.Status = ApprovalApproved
		}
	}
	log.Info("rollout approval ended", "status", st.Status)
	if st.Status != ApprovalApproved {
		w.publishRollout(ctx, rs)
		return fmt.Errorf("rollout stopped (%s): approval %s", rs.Paused, st.Status)
	}
	rs.Status, rs.Paused = service.RolloutRunning, ""
	w.publishRollout(ctx, rs)
	return nil
}

// rolloutFailure applies onFailure after stage i failed: stop leaves everything as is,
// rollback-wave compensates the failed stage, rollback-all all stages in reverse order.
func (w *Worker) rolloutFailure(ctx dbos.Context, id string, d *descriptor.Descriptor, ev *events, res *Result,
	done [][]passRun, rs *RolloutState, i int, cause error) error {
	rs.Status, rs.Stage = service.RolloutFailed, rs.Stages[i].Name
	if rs.Stages[i].State != service.StageSucceeded {
		rs.Stages[i].State = service.StageFailed
	}
	rs.Stages[i].Error = cause.Error()
	w.publishRollout(ctx, rs)
	onFailure := d.Rollout.OnFailureOrDefault()
	if d.DryRun || onFailure == descriptor.OnFailureStop {
		return cause
	}
	from := 0
	if onFailure == descriptor.OnFailureRollbackWave {
		from = len(done) - 1
	}
	rbErr := &RollbackError{Err: cause, Status: RollbackSkipped}
	for s := len(done) - 1; s >= from; s-- {
		for j := len(done[s]) - 1; j >= 0; j-- {
			pr := done[s][j]
			status, failure := w.undoPass(ctx, id, d, ev, res, pr, cause)
			switch status {
			case RollbackFailed:
				rbErr.Status, rbErr.Cause = RollbackFailed, failure
				rs.Rollback = RollbackFailed
				w.publishRollout(ctx, rs)
				return rbErr
			case RollbackCompleted:
				rbErr.Status = RollbackCompleted
			}
		}
		rs.Stages[s].State = service.StageRolledBack
		w.publishRollout(ctx, rs)
	}
	rs.Rollback = rbErr.Status
	w.publishRollout(ctx, rs)
	return rbErr
}

// undoPass compensates a pass, inline or on its region's worker.
func (w *Worker) undoPass(ctx dbos.Context, id string, d *descriptor.Descriptor, ev *events, res *Result, pr passRun, cause error) (string, error) {
	if len(pr.started) == 0 {
		return RollbackSkipped, nil
	}
	if !pr.routed {
		err := w.undo(ctx, id, ev, d, res, pr.started, cause)
		if rb, ok := errors.AsType[*RollbackError](err); ok {
			return rb.Status, rb.Cause
		}
		return RollbackFailed, err
	}
	childID, h, err := w.startPass(ctx, id, Request{}, d, ev, nil, pr.pass, nil, pr.started, cause.Error())
	if err != nil {
		return RollbackFailed, err
	}
	out, err := h.GetResult()
	w.copyPassLogs(ctx, childID)
	res.Steps = append(res.Steps, out.Steps...)
	if err != nil {
		return RollbackFailed, err
	}
	if out.Rollback == RollbackFailed {
		return RollbackFailed, errors.New(cmpString(out.Error, "rollback of pass "+pr.pass.Name+" failed"))
	}
	return out.Rollback, nil
}

func cmpString(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// rolloutProgress summarizes the rollout for the attributes: "stages passed/total stages".
func rolloutProgress(rs *RolloutState) string {
	return strconv.Itoa(len(rs.Completed())) + "/" + strconv.Itoa(len(rs.Stages))
}

// progressStarted reports a rollout that passed at least one stage.
func progressStarted(p string) bool {
	done, _, ok := strings.Cut(p, "/")
	return ok && done != "0"
}
