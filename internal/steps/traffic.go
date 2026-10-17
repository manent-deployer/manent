package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/pipeline"
	"github.com/joeg-ita/manent/internal/secrets"
	"github.com/joeg-ita/manent/internal/traffic"
	"github.com/joeg-ita/manent/pkg/plugin"
)

// Names of the pool steps: the engine inserts them around the steps of every batch when the
// environment has a pool (traffic.pool).
const (
	DrainStep   = "drain"
	RestoreStep = "restore"
)

// Output keys of the traffic steps.
const (
	OutMembers  = "members"
	OutCapacity = "capacity"
	OutDetail   = "detail"
	// CapacityInsufficient means a postponed drain: the engine waits and retries.
	CapacityInsufficient = "insufficient"
)

// TrafficOutput is the key in pipeline.Input.Outputs with the state of the percentage
// rollout, recorded by Traffic.Prepare.
const TrafficOutput = "traffic"

// Output keys of Traffic.Prepare.
const (
	OutMode       = "mode"
	OutStable     = "stable"
	OutCandidate  = "candidate"
	OutHasStable  = "has_stable"
	OutState      = "state"
	OutCandidates = "candidate_hosts"
	OutRouter     = "router"
	ModePlugin    = "plugin"
	ModeRouter    = "router"
)

// Pool builds the environment pool for a deploy. Shared by the drain and restore steps and by
// the engine's waits.
type Pool struct {
	Providers *traffic.Providers
	Secrets   secrets.Provider
	// Dialer opens the SSH sessions of target-based providers (nil: SSH with known_hosts).
	Dialer  traffic.Dialer
	Timeout time.Duration
	Logger  *slog.Logger
}

// open builds the pool and computes the members of the input targets.
func (p Pool) open(ctx context.Context, in pipeline.Input) (traffic.Pool, *descriptor.PoolConfig, []string, error) {
	d := in.Descriptor
	if d.Traffic == nil || d.Traffic.Pool == nil {
		return nil, nil, nil, pipeline.Permanent(errors.New("no pool configured (traffic.pool)"))
	}
	cfg := d.Traffic.Pool
	pool, err := p.Providers.Pool(ctx, cfg, traffic.Env{
		Targets: d.Deploy.Targets, Secrets: secrets.DescriptorResolver{Provider: p.Secrets, Descriptor: d},
		Dialer: p.Dialer, DeploymentID: in.DeploymentID, Logger: p.Logger, Timeout: p.Timeout,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	members := make([]string, len(d.Deploy.Targets))
	for i, t := range d.Deploy.Targets {
		members[i] = cfg.MemberName(t)
	}
	return pool, cfg, members, nil
}

func (p Pool) log(in pipeline.Input) *slog.Logger {
	return p.Logger.With("deployment", in.DeploymentID, "pool", in.Descriptor.Traffic.Pool.Provider)
}

// Status reads the state of the members during an engine wait: with phase "drain" the
// remaining connections, with "restore" the health. done means the wait is over; min_wait is
// the minimum wait after draining for providers that do not count connections.
func (p Pool) Status(ctx context.Context, in pipeline.Input, phase string) (pipeline.Output, error) {
	pool, _, members, err := p.open(ctx, in)
	if err != nil {
		return nil, err
	}
	out := pipeline.Output{OutMembers: strings.Join(members, ","), "done": "true"}
	var pending []string
	switch phase {
	case DrainStep:
		conns, err := pool.Connections(ctx, members)
		if err != nil {
			return nil, err
		}
		for _, m := range members {
			if conns[m] > 0 {
				pending = append(pending, m+"="+strconv.Itoa(conns[m]))
			}
		}
		if dd, ok := pool.(traffic.DrainDelayer); ok {
			out["min_wait"] = dd.DrainDelay().String()
		}
	case RestoreStep:
		ok, err := pool.Healthy(ctx, members)
		if err != nil {
			return nil, err
		}
		for _, m := range members {
			if !ok[m] {
				pending = append(pending, m)
			}
		}
	default:
		return nil, fmt.Errorf("unknown phase %q", phase)
	}
	if len(pending) > 0 {
		out["done"], out["pending"] = "false", strings.Join(pending, ",")
	}
	return out, nil
}

// Drain removes the batch targets from the load balancer. It checks capacity first: if the
// remaining healthy members would be fewer than min_healthy it does not drain and says so in
// the output, so the engine waits and retries without consuming the step retries.
type Drain struct{ Pool Pool }

// Name implements pipeline.Step.
func (Drain) Name() string { return DrainStep }

// Run implements pipeline.Step.
func (s Drain) Run(ctx context.Context, in pipeline.Input) (pipeline.Output, error) {
	pool, cfg, members, err := s.Pool.open(ctx, in)
	if err != nil {
		return nil, err
	}
	log := s.Pool.log(in)
	all, err := pool.Members(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the pool: %w", err)
	}
	if all != nil {
		if miss := traffic.Missing(all, members); len(miss) > 0 {
			return nil, pipeline.Permanent(fmt.Errorf("members missing from the pool: %s (traffic.pool.member)", strings.Join(miss, ", ")))
		}
		if err := traffic.Capacity(all, members, cfg.MinHealthyFor(len(all))); err != nil {
			log.WarnContext(ctx, "svuotamento rimandato", "members", members, "error", err)
			return pipeline.Output{OutMembers: strings.Join(members, ","), OutCapacity: CapacityInsufficient, OutDetail: err.Error()}, nil
		}
	} else {
		log.InfoContext(ctx, "controllo di capacità non disponibile per il provider", "members", members)
	}
	if err := pool.Drain(ctx, members); err != nil {
		return nil, fmt.Errorf("draining %s: %w", strings.Join(members, ", "), err)
	}
	log.InfoContext(ctx, "target in svuotamento", "members", members)
	return pipeline.Output{OutMembers: strings.Join(members, ","), OutCapacity: "ok"}, nil
}

// Compensate puts the drained targets back in service and waits for their health: it is the
// last compensation of the batch, after the deploy rollback.
func (s Drain) Compensate(ctx context.Context, in pipeline.Input, _ pipeline.Output) (pipeline.Output, error) {
	if err := s.Pool.restore(ctx, in, true); err != nil {
		return nil, err
	}
	return pipeline.Output{pipeline.OutRollback: pipeline.RollbackCompleted, "status": "restored"}, nil
}

// Restore puts the batch targets back in service. The engine waits for their health.
type Restore struct{ Pool Pool }

// Name implements pipeline.Step.
func (Restore) Name() string { return RestoreStep }

// Run implements pipeline.Step.
func (s Restore) Run(ctx context.Context, in pipeline.Input) (pipeline.Output, error) {
	if err := s.Pool.restore(ctx, in, false); err != nil {
		return nil, err
	}
	pool := in.Descriptor.Traffic.Pool
	members := make([]string, len(in.Descriptor.Deploy.Targets))
	for i, t := range in.Descriptor.Deploy.Targets {
		members[i] = pool.MemberName(t)
	}
	return pipeline.Output{OutMembers: strings.Join(members, ",")}, nil
}

// Compensate drains the targets again before the deploy rollback touches them, and waits for
// the connections to end.
func (s Restore) Compensate(ctx context.Context, in pipeline.Input, _ pipeline.Output) (pipeline.Output, error) {
	pool, cfg, members, err := s.Pool.open(ctx, in)
	if err != nil {
		return nil, err
	}
	if err := pool.Drain(ctx, members); err != nil {
		return nil, err
	}
	drain, _, poll := cfg.Durations()
	if dd, ok := pool.(traffic.DrainDelayer); ok {
		if err := sleep(ctx, dd.DrainDelay()); err != nil {
			return nil, err
		}
	}
	err = waitUntil(ctx, drain, poll, func() (bool, error) {
		conns, err := pool.Connections(ctx, members)
		if err != nil {
			return false, err
		}
		return !slices.ContainsFunc(members, func(m string) bool { return conns[m] > 0 }), nil
	})
	if err != nil && cfg.OnTimeout == descriptor.OnTimeoutFail {
		return nil, fmt.Errorf("draining before the rollback: %w", err)
	}
	return pipeline.Output{pipeline.OutRollback: pipeline.RollbackCompleted, "status": "drained"}, nil
}

// restore puts the members back in service; with wait it also waits for their health (in
// compensations, where there is no durable engine wait).
func (p Pool) restore(ctx context.Context, in pipeline.Input, wait bool) error {
	pool, cfg, members, err := p.open(ctx, in)
	if err != nil {
		return err
	}
	if err := pool.Restore(ctx, members); err != nil {
		return fmt.Errorf("restoring %s: %w", strings.Join(members, ", "), err)
	}
	p.log(in).InfoContext(ctx, "target rimessi in servizio", "members", members)
	if !wait {
		return nil
	}
	_, restore, poll := cfg.Durations()
	return waitUntil(ctx, restore, poll, func() (bool, error) {
		ok, err := pool.Healthy(ctx, members)
		if err != nil {
			return false, err
		}
		return !slices.ContainsFunc(members, func(m string) bool { return !ok[m] }), nil
	})
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// waitUntil repeats check until it succeeds or the timeout expires.
func waitUntil(ctx context.Context, timeout, poll time.Duration, check func() (bool, error)) error {
	deadline := time.Now().Add(timeout)
	for {
		done, err := check()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("not completed within %s", timeout)
		}
		if err := sleep(ctx, poll); err != nil {
			return err
		}
	}
}

// Traffic runs the operations of percentage rollouts: with an environment router
// (traffic.router) the slots are the groups of targets with the slot label; otherwise the
// deploy plugin must implement plugin.SlotDeployer and plugin.TrafficRouter.
type Traffic struct {
	Deploy    Deploy
	Pool      Pool
	Providers *traffic.Providers
	Timeout   time.Duration
	Logger    *slog.Logger
}

// PoolStatus reads the state of the pool members during the engine waits.
func (t Traffic) PoolStatus(ctx context.Context, in pipeline.Input, phase string) (pipeline.Output, error) {
	return t.Pool.Status(ctx, in, phase)
}

// Check checks an environment's traffic configuration without contacting the systems.
func (t Traffic) Check(d *descriptor.Descriptor) error {
	if err := t.Providers.Check(d.Traffic); err != nil {
		return err
	}
	if !d.Rollout.IsTraffic() {
		return nil
	}
	if d.Traffic != nil && d.Traffic.Router != nil {
		if len(d.Rollout.Waves) > 0 {
			return errors.New("strategy: traffic with rollout.waves requires a plugin acting as router: traffic.router shifts the traffic of the whole environment")
		}
		return checkSlotLabels(d.Deploy.Targets)
	}
	dep, _, err := t.Deploy.selectPlugin(pipeline.Input{Descriptor: d})
	if err != nil {
		// The plugin is chosen by the environment configuration: the config step checks it.
		return nil
	}
	if _, ok := dep.(plugin.TrafficChecker); ok {
		// An external plugin declares its support at startup: it is checked at execution, without
		// starting the process at submission.
		return nil
	}
	if _, ok := dep.(plugin.SlotDeployer); !ok {
		return fmt.Errorf("strategy: traffic: plugin %q cannot release a side-by-side version; configure traffic.router in the environment", dep.Name())
	}
	if _, ok := dep.(plugin.TrafficRouter); !ok {
		return fmt.Errorf("strategy: traffic: plugin %q cannot shift traffic; configure traffic.router in the environment", dep.Name())
	}
	if d.Rollout.Traffic.Replicas == descriptor.ReplicasProportional {
		if _, ok := dep.(plugin.SlotScaler); !ok {
			return fmt.Errorf("rollout.traffic.replicas: proportional is not supported by plugin %q", dep.Name())
		}
	}
	return nil
}

// checkSlotLabels checks that every target has slot a or b.
func checkSlotLabels(ts []descriptor.Target) error {
	var errs []error
	for _, t := range ts {
		if !descriptor.ValidSlot(t.Labels[descriptor.SlotLabel]) {
			errs = append(errs, fmt.Errorf("target %s: with traffic.router the label %s (a or b) is required", t.Host, descriptor.SlotLabel))
		}
	}
	return errors.Join(errs...)
}

func (t Traffic) router(ctx context.Context, in pipeline.Input) (traffic.Router, error) {
	d := in.Descriptor
	return t.Providers.Router(ctx, d.Traffic.Router, traffic.Env{
		Targets: d.Deploy.Targets, Secrets: secrets.DescriptorResolver{Provider: t.Deploy.Secrets, Descriptor: d},
		DeploymentID: in.DeploymentID, Logger: t.Logger, Timeout: t.Timeout,
	})
}

func routerMode(d *descriptor.Descriptor) bool { return d.Traffic != nil && d.Traffic.Router != nil }

// pluginCall prepares plugin and request for the SlotDeployer operations.
func (t Traffic) pluginCall(in pipeline.Input) (plugin.Deployer, *plugin.Request, *slog.Logger, error) {
	dep, log, err := t.Deploy.selectPlugin(in)
	if err != nil {
		return nil, nil, nil, err
	}
	req, err := t.Deploy.request(dep, in, in.Outputs["artifacts"][OutLocalPath])
	if err != nil {
		return nil, nil, nil, err
	}
	return dep, req, log, nil
}

// Prepare records the state before the rollout: stable version and candidate slot. It
// changes nothing.
func (t Traffic) Prepare(ctx context.Context, in pipeline.Input) (pipeline.Output, error) {
	d := in.Descriptor
	if routerMode(d) {
		r, err := t.router(ctx, in)
		if err != nil {
			return nil, err
		}
		w, err := r.Weights(ctx)
		if err != nil {
			return nil, fmt.Errorf("reading the router weights: %w", err)
		}
		stable := ""
		for _, s := range descriptor.Slots {
			if w[s] > 0 && (stable == "" || w[s] > w[stable]) {
				stable = s
			}
		}
		cand := descriptor.OtherSlot(stable)
		var hosts []string
		for _, tg := range d.Deploy.Targets {
			if tg.Labels[descriptor.SlotLabel] == cand {
				hosts = append(hosts, tg.Host)
			}
		}
		if len(hosts) == 0 {
			return nil, pipeline.Permanent(fmt.Errorf("no target in slot %s", cand))
		}
		out := pipeline.Output{OutMode: ModeRouter, OutRouter: d.Traffic.Router.Provider, OutStable: stable, OutCandidate: cand,
			OutHasStable: strconv.FormatBool(stable != ""), OutCandidates: strings.Join(hosts, ",")}
		t.Logger.InfoContext(ctx, "rilascio affiancato preparato", "deployment", in.DeploymentID, "stable", stable, "candidate", cand, "weights", w)
		return out, nil
	}
	dep, req, log, err := t.pluginCall(in)
	if err != nil {
		return nil, err
	}
	sd, ok := dep.(plugin.SlotDeployer)
	if !ok {
		return nil, pipeline.Permanent(fmt.Errorf("plugin %q cannot release a side-by-side version", dep.Name()))
	}
	state, err := sd.PrepareSlots(ctx, req)
	if err != nil {
		return nil, unsupported(dep, err)
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	out := pipeline.Output{OutMode: ModePlugin, OutRouter: dep.Name(), OutState: string(raw), OutHasStable: strconv.FormatBool(state[plugin.StateHasStable] == "true")}
	for _, k := range []string{OutStable, OutCandidate} {
		if v := state[k]; v != "" {
			out[k] = v
		}
	}
	log.InfoContext(ctx, "rilascio affiancato preparato", "state", state)
	return out, nil
}

// CandidateTargets returns the targets of the candidate slot (router mode), nil if the
// rollout uses all targets.
func CandidateTargets(d *descriptor.Descriptor, prep pipeline.Output) []descriptor.Target {
	if prep[OutMode] != ModeRouter {
		return nil
	}
	var out []descriptor.Target
	for _, t := range d.Deploy.Targets {
		if t.Labels[descriptor.SlotLabel] == prep[OutCandidate] {
			out = append(out, t)
		}
	}
	return out
}

// SetWeight sends percent% of the traffic to the candidate.
func (t Traffic) SetWeight(ctx context.Context, in pipeline.Input, percent int) (pipeline.Output, error) {
	prep := in.Outputs[TrafficOutput]
	if prep[OutMode] == ModeRouter {
		r, err := t.router(ctx, in)
		if err != nil {
			return nil, err
		}
		w := map[string]int{prep[OutCandidate]: percent}
		if s := prep[OutStable]; s != "" {
			w[s] = 100 - percent
		}
		if err := r.SetWeights(ctx, w); err != nil {
			return nil, err
		}
		t.Logger.InfoContext(ctx, "traffico spostato", "deployment", in.DeploymentID, "candidate", prep[OutCandidate], "percent", percent)
		return pipeline.Output{"weight": strconv.Itoa(percent)}, nil
	}
	dep, req, log, err := t.pluginCall(in)
	if err != nil {
		return nil, err
	}
	tr, ok := dep.(plugin.TrafficRouter)
	if !ok {
		return nil, pipeline.Permanent(fmt.Errorf("plugin %q cannot shift traffic", dep.Name()))
	}
	if err := tr.SetCandidateWeight(ctx, req, percent); err != nil {
		return nil, unsupported(dep, err)
	}
	log.InfoContext(ctx, "traffico spostato", "percent", percent)
	return pipeline.Output{"weight": strconv.Itoa(percent)}, nil
}

// Weight reads the candidate's current share (-1 if the targets disagree).
func (t Traffic) Weight(ctx context.Context, in pipeline.Input) (pipeline.Output, error) {
	prep := in.Outputs[TrafficOutput]
	w := 0
	if prep[OutMode] == ModeRouter {
		r, err := t.router(ctx, in)
		if err != nil {
			return nil, err
		}
		ws, err := r.Weights(ctx)
		if err != nil {
			return nil, err
		}
		total := 0
		for _, v := range ws {
			total += v
		}
		if total > 0 {
			w = ws[prep[OutCandidate]] * 100 / total
		}
	} else {
		dep, req, _, err := t.pluginCall(in)
		if err != nil {
			return nil, err
		}
		tr, ok := dep.(plugin.TrafficRouter)
		if !ok {
			return nil, pipeline.Permanent(fmt.Errorf("plugin %q cannot shift traffic", dep.Name()))
		}
		if w, err = tr.CandidateWeight(ctx, req); err != nil {
			return nil, unsupported(dep, err)
		}
	}
	return pipeline.Output{"weight": strconv.Itoa(w)}, nil
}

// Testers routes only the testers' requests to the candidate.
func (t Traffic) Testers(ctx context.Context, in pipeline.Input) (pipeline.Output, error) {
	prep := in.Outputs[TrafficOutput]
	var details map[string]string
	if prep[OutMode] == ModeRouter {
		r, err := t.router(ctx, in)
		if err != nil {
			return nil, err
		}
		tr, ok := r.(traffic.TesterRouter)
		if !ok {
			return nil, pipeline.Permanent(fmt.Errorf("router %s cannot route the testers", prep[OutRouter]))
		}
		if details, err = tr.RouteTesters(ctx, prep[OutCandidate]); err != nil {
			return nil, err
		}
	} else {
		dep, req, _, err := t.pluginCall(in)
		if err != nil {
			return nil, err
		}
		tr, ok := dep.(plugin.TesterRouter)
		if !ok {
			return nil, pipeline.Permanent(fmt.Errorf("plugin %q cannot route the testers", dep.Name()))
		}
		if details, err = tr.RouteTesters(ctx, req); err != nil {
			return nil, unsupported(dep, err)
		}
	}
	out := pipeline.Output{"testers": "routed"}
	for k, v := range details {
		out[k] = v
	}
	return out, nil
}

// Scale sizes the candidate in proportion to its traffic share.
func (t Traffic) Scale(ctx context.Context, in pipeline.Input, percent int) (pipeline.Output, error) {
	if in.Outputs[TrafficOutput][OutMode] == ModeRouter {
		return nil, pipeline.Permanent(errors.New("rollout.traffic.replicas: proportional requires a plugin that scales the candidate"))
	}
	dep, req, _, err := t.pluginCall(in)
	if err != nil {
		return nil, err
	}
	sc, ok := dep.(plugin.SlotScaler)
	if !ok {
		return nil, pipeline.Permanent(fmt.Errorf("plugin %q does not scale the candidate", dep.Name()))
	}
	if err := sc.ScaleCandidate(ctx, req, percent); err != nil {
		return nil, unsupported(dep, err)
	}
	return pipeline.Output{"replicas_percent": strconv.Itoa(percent)}, nil
}

// Promote makes the candidate the version in service.
func (t Traffic) Promote(ctx context.Context, in pipeline.Input) (pipeline.Output, error) {
	prep := in.Outputs[TrafficOutput]
	if prep[OutMode] == ModeRouter {
		return t.SetWeight(ctx, in, 100)
	}
	return t.slotCall(ctx, in, "promoted", plugin.SlotDeployer.PromoteCandidate)
}

// Revert brings all traffic back to the stable version and retires the candidate.
func (t Traffic) Revert(ctx context.Context, in pipeline.Input) (pipeline.Output, error) {
	prep := in.Outputs[TrafficOutput]
	if prep[OutMode] == ModeRouter {
		if prep[OutHasStable] != "true" {
			return pipeline.Output{pipeline.OutRollback: pipeline.RollbackSkipped, "reason": "nessuna versione stabile"}, nil
		}
		r, err := t.router(ctx, in)
		if err != nil {
			return nil, err
		}
		if err := r.SetWeights(ctx, map[string]int{prep[OutStable]: 100, prep[OutCandidate]: 0}); err != nil {
			return nil, err
		}
		return pipeline.Output{pipeline.OutRollback: pipeline.RollbackCompleted, "weight": "0"}, nil
	}
	out, err := t.slotCall(ctx, in, "reverted", plugin.SlotDeployer.RevertCandidate)
	if err != nil {
		return nil, err
	}
	out[pipeline.OutRollback] = pipeline.RollbackCompleted
	return out, nil
}

// Retire scales down the replaced version after the promotion.
func (t Traffic) Retire(ctx context.Context, in pipeline.Input) (pipeline.Output, error) {
	if in.Outputs[TrafficOutput][OutMode] == ModeRouter {
		return pipeline.Output{"status": "skipped"}, nil
	}
	return t.slotCall(ctx, in, "retired", plugin.SlotDeployer.RetirePrevious)
}

func (t Traffic) slotCall(ctx context.Context, in pipeline.Input, status string, fn func(plugin.SlotDeployer, context.Context, *plugin.Request) error) (pipeline.Output, error) {
	dep, req, log, err := t.pluginCall(in)
	if err != nil {
		return nil, err
	}
	sd, ok := dep.(plugin.SlotDeployer)
	if !ok {
		return nil, pipeline.Permanent(fmt.Errorf("plugin %q cannot release a side-by-side version", dep.Name()))
	}
	if err := fn(sd, ctx, req); err != nil {
		return nil, unsupported(dep, err)
	}
	log.InfoContext(ctx, "rilascio affiancato: "+status)
	return pipeline.Output{"status": status}, nil
}

// trafficRequest rebuilds the percentage rollout data for the plugin.
func trafficRequest(in pipeline.Input) *plugin.TrafficRequest {
	prep, ok := in.Outputs[TrafficOutput]
	if !ok || prep[OutMode] != ModePlugin {
		return nil
	}
	tr := &plugin.TrafficRequest{}
	_ = json.Unmarshal([]byte(prep[OutState]), &tr.State)
	if r := in.Descriptor.Rollout; r.IsTraffic() {
		if r.Traffic.Testers != nil {
			tr.TesterHeader, tr.TesterTag = r.Traffic.Testers.Header, r.Traffic.Testers.Tag
		}
		tr.ProportionalReplicas = r.Traffic.Replicas == descriptor.ReplicasProportional
	}
	return tr
}

// unsupported makes permanent the error of an operation the plugin does not support (an
// external plugin finds out only at runtime): a retry would not change the outcome.
func unsupported(dep plugin.Deployer, err error) error {
	if errors.Is(err, plugin.ErrTrafficUnsupported) {
		return pipeline.Permanent(fmt.Errorf("plugin %q: %w", dep.Name(), err))
	}
	return err
}
