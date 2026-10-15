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

// Nomi degli step del pool: il motore li inserisce intorno agli step di ogni lotto quando
// l'ambiente ha un pool (traffic.pool).
const (
	DrainStep   = "drain"
	RestoreStep = "restore"
)

// Chiavi degli output degli step di traffico.
const (
	OutMembers  = "members"
	OutCapacity = "capacity"
	OutDetail   = "detail"
	// CapacityInsufficient indica uno svuotamento rimandato: il motore attende e riprova.
	CapacityInsufficient = "insufficient"
)

// TrafficOutput è la chiave in pipeline.Input.Outputs con lo stato del rilascio per
// percentuale, registrato da Traffic.Prepare.
const TrafficOutput = "traffic"

// Chiavi dell'output di Traffic.Prepare.
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

// Pool costruisce il pool dell'ambiente per un deploy. Condiviso dagli step drain e restore e
// dalle attese del motore.
type Pool struct {
	Providers *traffic.Providers
	Secrets   secrets.Provider
	// Dialer apre le sessioni SSH dei provider basati sui target (nil: SSH con known_hosts).
	Dialer  traffic.Dialer
	Timeout time.Duration
	Logger  *slog.Logger
}

// open costruisce il pool e calcola i membri dei target dell'input.
func (p Pool) open(ctx context.Context, in pipeline.Input) (traffic.Pool, *descriptor.PoolConfig, []string, error) {
	d := in.Descriptor
	if d.Traffic == nil || d.Traffic.Pool == nil {
		return nil, nil, nil, pipeline.Permanent(errors.New("nessun pool configurato (traffic.pool)"))
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

// Status legge lo stato dei membri durante un'attesa del motore: con phase "drain" le
// connessioni residue, con "restore" la salute. done indica l'attesa conclusa; min_wait è
// l'attesa minima dopo lo svuotamento per i provider che non contano le connessioni.
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
		return nil, fmt.Errorf("fase %q sconosciuta", phase)
	}
	if len(pending) > 0 {
		out["done"], out["pending"] = "false", strings.Join(pending, ",")
	}
	return out, nil
}

// Drain toglie i target del lotto dal load balancer. Prima verifica la capacità: se i membri
// sani rimasti sarebbero meno di min_healthy non svuota e lo segnala nell'output, così il
// motore attende e riprova senza consumare i retry dello step.
type Drain struct{ Pool Pool }

// Name implementa pipeline.Step.
func (Drain) Name() string { return DrainStep }

// Run implementa pipeline.Step.
func (s Drain) Run(ctx context.Context, in pipeline.Input) (pipeline.Output, error) {
	pool, cfg, members, err := s.Pool.open(ctx, in)
	if err != nil {
		return nil, err
	}
	log := s.Pool.log(in)
	all, err := pool.Members(ctx)
	if err != nil {
		return nil, fmt.Errorf("lettura del pool: %w", err)
	}
	if all != nil {
		if miss := traffic.Missing(all, members); len(miss) > 0 {
			return nil, pipeline.Permanent(fmt.Errorf("membri assenti dal pool: %s (traffic.pool.member)", strings.Join(miss, ", ")))
		}
		if err := traffic.Capacity(all, members, cfg.MinHealthyFor(len(all))); err != nil {
			log.WarnContext(ctx, "svuotamento rimandato", "members", members, "error", err)
			return pipeline.Output{OutMembers: strings.Join(members, ","), OutCapacity: CapacityInsufficient, OutDetail: err.Error()}, nil
		}
	} else {
		log.InfoContext(ctx, "controllo di capacità non disponibile per il provider", "members", members)
	}
	if err := pool.Drain(ctx, members); err != nil {
		return nil, fmt.Errorf("svuotamento di %s: %w", strings.Join(members, ", "), err)
	}
	log.InfoContext(ctx, "target in svuotamento", "members", members)
	return pipeline.Output{OutMembers: strings.Join(members, ","), OutCapacity: "ok"}, nil
}

// Compensate rimette in servizio i target svuotati e ne attende la salute: è l'ultima
// compensazione del lotto, dopo il rollback del deploy.
func (s Drain) Compensate(ctx context.Context, in pipeline.Input, _ pipeline.Output) (pipeline.Output, error) {
	if err := s.Pool.restore(ctx, in, true); err != nil {
		return nil, err
	}
	return pipeline.Output{pipeline.OutRollback: pipeline.RollbackCompleted, "status": "restored"}, nil
}

// Restore rimette in servizio i target del lotto. L'attesa della salute la fa il motore.
type Restore struct{ Pool Pool }

// Name implementa pipeline.Step.
func (Restore) Name() string { return RestoreStep }

// Run implementa pipeline.Step.
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

// Compensate svuota di nuovo i target prima che il rollback del deploy li tocchi, e attende
// la fine delle connessioni.
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
		return nil, fmt.Errorf("svuotamento prima del rollback: %w", err)
	}
	return pipeline.Output{pipeline.OutRollback: pipeline.RollbackCompleted, "status": "drained"}, nil
}

// restore rimette in servizio i membri; con wait attende anche la salute (nelle
// compensazioni, dove non c'è l'attesa durevole del motore).
func (p Pool) restore(ctx context.Context, in pipeline.Input, wait bool) error {
	pool, cfg, members, err := p.open(ctx, in)
	if err != nil {
		return err
	}
	if err := pool.Restore(ctx, members); err != nil {
		return fmt.Errorf("rientro di %s: %w", strings.Join(members, ", "), err)
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

// waitUntil ripete check fino all'esito positivo o allo scadere del timeout.
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
			return fmt.Errorf("non concluso entro %s", timeout)
		}
		if err := sleep(ctx, poll); err != nil {
			return err
		}
	}
}

// Traffic esegue le operazioni dei rilasci per percentuale: con un router dell'ambiente
// (traffic.router) gli slot sono i gruppi di target con l'etichetta slot; altrimenti il plugin
// di deploy deve implementare plugin.SlotDeployer e plugin.TrafficRouter.
type Traffic struct {
	Deploy    Deploy
	Pool      Pool
	Providers *traffic.Providers
	Timeout   time.Duration
	Logger    *slog.Logger
}

// PoolStatus legge lo stato dei membri del pool durante le attese del motore.
func (t Traffic) PoolStatus(ctx context.Context, in pipeline.Input, phase string) (pipeline.Output, error) {
	return t.Pool.Status(ctx, in, phase)
}

// Check verifica la configurazione di traffico di un ambiente senza contattare i sistemi.
func (t Traffic) Check(d *descriptor.Descriptor) error {
	if err := t.Providers.Check(d.Traffic); err != nil {
		return err
	}
	if !d.Rollout.IsTraffic() {
		return nil
	}
	if d.Traffic != nil && d.Traffic.Router != nil {
		return checkSlotLabels(d.Deploy.Targets)
	}
	dep, _, err := t.Deploy.selectPlugin(pipeline.Input{Descriptor: d})
	if err != nil {
		// Il plugin si sceglie con la configurazione di ambiente: lo verifica lo step config.
		return nil
	}
	if _, ok := dep.(plugin.SlotDeployer); !ok {
		return fmt.Errorf("strategy: traffic: il plugin %q non sa rilasciare una versione affiancata; configurare traffic.router nell'ambiente", dep.Name())
	}
	if _, ok := dep.(plugin.TrafficRouter); !ok {
		return fmt.Errorf("strategy: traffic: il plugin %q non sa spostare il traffico; configurare traffic.router nell'ambiente", dep.Name())
	}
	if d.Rollout.Traffic.Replicas == descriptor.ReplicasProportional {
		if _, ok := dep.(plugin.SlotScaler); !ok {
			return fmt.Errorf("rollout.traffic.replicas: proportional non è supportato dal plugin %q", dep.Name())
		}
	}
	return nil
}

// checkSlotLabels verifica che ogni target abbia lo slot a o b.
func checkSlotLabels(ts []descriptor.Target) error {
	var errs []error
	for _, t := range ts {
		if !descriptor.ValidSlot(t.Labels[descriptor.SlotLabel]) {
			errs = append(errs, fmt.Errorf("target %s: con traffic.router serve l'etichetta %s (a o b)", t.Host, descriptor.SlotLabel))
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

// pluginCall prepara plugin e richiesta per le operazioni dello SlotDeployer.
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

// Prepare registra lo stato prima del rilascio: versione stabile e slot candidato. Non
// modifica nulla.
func (t Traffic) Prepare(ctx context.Context, in pipeline.Input) (pipeline.Output, error) {
	d := in.Descriptor
	if routerMode(d) {
		r, err := t.router(ctx, in)
		if err != nil {
			return nil, err
		}
		w, err := r.Weights(ctx)
		if err != nil {
			return nil, fmt.Errorf("lettura dei pesi del router: %w", err)
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
			return nil, pipeline.Permanent(fmt.Errorf("nessun target nello slot %s", cand))
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
		return nil, pipeline.Permanent(fmt.Errorf("il plugin %q non sa rilasciare una versione affiancata", dep.Name()))
	}
	state, err := sd.PrepareSlots(ctx, req)
	if err != nil {
		return nil, err
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

// CandidateTargets restituisce i target dello slot candidato (modalità router), nil se il
// rilascio usa tutti i target.
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

// SetWeight manda alla candidata percent% del traffico.
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
		return nil, pipeline.Permanent(fmt.Errorf("il plugin %q non sa spostare il traffico", dep.Name()))
	}
	if err := tr.SetCandidateWeight(ctx, req, percent); err != nil {
		return nil, err
	}
	log.InfoContext(ctx, "traffico spostato", "percent", percent)
	return pipeline.Output{"weight": strconv.Itoa(percent)}, nil
}

// Weight legge la quota corrente della candidata (-1 se i target non sono concordi).
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
			return nil, pipeline.Permanent(fmt.Errorf("il plugin %q non sa spostare il traffico", dep.Name()))
		}
		if w, err = tr.CandidateWeight(ctx, req); err != nil {
			return nil, err
		}
	}
	return pipeline.Output{"weight": strconv.Itoa(w)}, nil
}

// Testers instrada verso la candidata le sole richieste dei tester.
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
			return nil, pipeline.Permanent(fmt.Errorf("il router %s non sa instradare i tester", prep[OutRouter]))
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
			return nil, pipeline.Permanent(fmt.Errorf("il plugin %q non sa instradare i tester", dep.Name()))
		}
		if details, err = tr.RouteTesters(ctx, req); err != nil {
			return nil, err
		}
	}
	out := pipeline.Output{"testers": "routed"}
	for k, v := range details {
		out[k] = v
	}
	return out, nil
}

// Scale dimensiona la candidata in proporzione alla quota di traffico.
func (t Traffic) Scale(ctx context.Context, in pipeline.Input, percent int) (pipeline.Output, error) {
	if in.Outputs[TrafficOutput][OutMode] == ModeRouter {
		return nil, pipeline.Permanent(errors.New("rollout.traffic.replicas: proportional richiede un plugin che dimensioni la candidata"))
	}
	dep, req, _, err := t.pluginCall(in)
	if err != nil {
		return nil, err
	}
	sc, ok := dep.(plugin.SlotScaler)
	if !ok {
		return nil, pipeline.Permanent(fmt.Errorf("il plugin %q non dimensiona la candidata", dep.Name()))
	}
	if err := sc.ScaleCandidate(ctx, req, percent); err != nil {
		return nil, err
	}
	return pipeline.Output{"replicas_percent": strconv.Itoa(percent)}, nil
}

// Promote rende la candidata la versione in esercizio.
func (t Traffic) Promote(ctx context.Context, in pipeline.Input) (pipeline.Output, error) {
	prep := in.Outputs[TrafficOutput]
	if prep[OutMode] == ModeRouter {
		return t.SetWeight(ctx, in, 100)
	}
	return t.slotCall(ctx, in, "promoted", plugin.SlotDeployer.PromoteCandidate)
}

// Revert riporta tutto il traffico alla versione stabile e ritira la candidata.
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

// Retire ridimensiona la versione sostituita dopo la promozione.
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
		return nil, pipeline.Permanent(fmt.Errorf("il plugin %q non sa rilasciare una versione affiancata", dep.Name()))
	}
	if err := fn(sd, ctx, req); err != nil {
		return nil, err
	}
	log.InfoContext(ctx, "rilascio affiancato: "+status)
	return pipeline.Output{"status": status}, nil
}

// trafficRequest ricostruisce i dati del rilascio per percentuale per il plugin.
func trafficRequest(in pipeline.Input) *plugin.TrafficRequest {
	prep, ok := in.Outputs[TrafficOutput]
	if !ok || prep[OutMode] != ModePlugin {
		return nil
	}
	tr := &plugin.TrafficRequest{}
	_ = json.Unmarshal([]byte(prep[OutState]), &tr.State)
	if r := in.Descriptor.Rollout; r.IsTraffic() && r.Traffic.Testers != nil {
		tr.TesterHeader, tr.TesterTag = r.Traffic.Testers.Header, r.Traffic.Testers.Tag
	}
	return tr
}
