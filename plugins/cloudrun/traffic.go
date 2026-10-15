package cloudrun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"

	"github.com/joeg-ita/manent/pkg/plugin"
	"github.com/joeg-ita/manent/plugins/internal/pluginutil"
)

// Rilasci per percentuale: le revisioni sono gli slot. La candidata è una revisione con nome
// fisso (derivato dal deploy) creata senza traffico; il traffico del servizio si divide tra la
// revisione stabile e la candidata, e i tester la raggiungono con l'URL del suo tag.

// stateStable è la chiave dello stato con la revisione stabile di una region ("stable@<region>").
const stateStable = "stable"

func stateKey(k string, t plugin.Target) string { return k + "@" + t.Host }

// candidateRevision è il nome della revisione candidata: <servizio>-m<hash del deploy>.
func candidateRevision(req *plugin.Request) string {
	sum := sha256.Sum256([]byte(req.DeploymentID))
	return serviceName(req) + "-m" + hex.EncodeToString(sum[:])[:10]
}

// PrepareSlots implementa plugin.SlotDeployer: registra la revisione che riceve il traffico.
func (p *Plugin) PrepareSlots(ctx context.Context, req *plugin.Request) (map[string]string, error) {
	state := map[string]string{}
	has := false
	for _, t := range req.Targets {
		s, err := p.read(ctx, t, req)
		if err != nil {
			return nil, fmt.Errorf("region %s: %w", t.Host, err)
		}
		rev, err := serving(s.svc)
		if err != nil {
			return nil, fmt.Errorf("region %s: %w", t.Host, err)
		}
		if rev != "" {
			state[stateKey(stateStable, t)] = rev
			has = true
		}
	}
	state[plugin.StateHasStable] = strconv.FormatBool(has)
	state["candidate"] = candidateRevision(req)
	return state, nil
}

func stableOf(req *plugin.Request, t plugin.Target) string {
	if req.Traffic == nil {
		return ""
	}
	return req.Traffic.State[stateKey(stateStable, t)]
}

// split restituisce il traffico tra stabile e candidata, con il tag dei tester sulla candidata.
func split(req *plugin.Request, stable, cand string, percent int) []any {
	c := Object{"type": trafficRevision, "revision": cand, "percent": percent}
	if req.Traffic != nil && req.Traffic.TesterTag != "" {
		c["tag"] = req.Traffic.TesterTag
	}
	if stable == "" || stable == cand {
		c["percent"] = 100
		return []any{c}
	}
	return []any{Object{"type": trafficRevision, "revision": stable, "percent": 100 - percent}, c}
}

// DeployCandidate implementa plugin.SlotDeployer: crea la revisione candidata con il traffico
// fermo sulla revisione stabile.
func (p *Plugin) DeployCandidate(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	image, err := plugin.ImageReference(req.Artifact)
	if err != nil {
		return nil, err
	}
	res := &plugin.Result{Plugin: Name}
	for _, t := range req.Targets {
		details, err := p.deployCandidate(ctx, t, req, image)
		if err != nil {
			res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusFailed, Details: details})
			return res, fmt.Errorf("region %s: %w", t.Host, err)
		}
		res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusDeployed, Details: details})
	}
	return res, nil
}

func (p *Plugin) deployCandidate(ctx context.Context, t plugin.Target, req *plugin.Request, image string) (map[string]string, error) {
	s, err := p.read(ctx, t, req)
	if err != nil {
		return nil, err
	}
	cand, stable := candidateRevision(req), stableOf(req, t)
	details := map[string]string{"region": s.region, "service": serviceName(req), DetailRevision: cand}
	timeout, _ := pluginutil.Duration(req.Params, ParamWaitTimeout, defaultWaitTimeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if str(s.svc, "template", "revision") != cand {
		svc, err := withImage(s.svc, req, image)
		if err != nil {
			return details, err
		}
		child(svc, "template")["revision"] = cand
		pct := 0
		if stable == "" {
			pct = 100
		}
		svc["traffic"] = split(req, stable, cand, pct)
		op, err := s.api.UpdateService(ctx, s.name, svc, false)
		if err != nil {
			return details, fmt.Errorf("creazione della revisione candidata: %w", err)
		}
		if err := p.wait(ctx, s.api, op, timeout); err != nil {
			return details, err
		}
	}
	// L'operazione conclusa senza errori garantisce una revisione pronta; si verifica solo
	// che esista con il nome atteso.
	if _, err := s.api.GetRevision(ctx, s.name+"/revisions/"+cand); err != nil {
		return details, fmt.Errorf("lettura della revisione %s: %w", cand, err)
	}
	p.logger.InfoContext(ctx, "revisione candidata pronta, senza traffico", "deployment", req.DeploymentID, "region", s.region, "revision", cand, "stable", stable)
	return details, nil
}

// setTraffic aggiorna il traffico del servizio e attende l'operazione.
func (p *Plugin) setTraffic(ctx context.Context, s *state, req *plugin.Request, traffic []any) error {
	timeout, _ := pluginutil.Duration(req.Params, ParamWaitTimeout, defaultWaitTimeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	s.svc["traffic"] = traffic
	op, err := s.api.UpdateService(ctx, s.name, s.svc, false)
	if err != nil {
		return fmt.Errorf("spostamento del traffico: %w", err)
	}
	return p.wait(ctx, s.api, op, timeout)
}

// SetCandidateWeight implementa plugin.TrafficRouter.
func (p *Plugin) SetCandidateWeight(ctx context.Context, req *plugin.Request, percent int) error {
	cand := candidateRevision(req)
	for _, t := range req.Targets {
		s, err := p.read(ctx, t, req)
		if err != nil {
			return fmt.Errorf("region %s: %w", t.Host, err)
		}
		if w, ok := revisionPercent(s.svc, cand); ok && w == percent {
			continue
		}
		if err := p.setTraffic(ctx, s, req, split(req, stableOf(req, t), cand, percent)); err != nil {
			return fmt.Errorf("region %s: %w", t.Host, err)
		}
		p.logger.InfoContext(ctx, "traffico del servizio aggiornato", "deployment", req.DeploymentID, "region", s.region, "revision", cand, "percent", percent)
	}
	return nil
}

// revisionPercent legge dallo stato del servizio la quota di traffico di una revisione.
func revisionPercent(svc Object, rev string) (int, bool) {
	statuses, _ := svc["trafficStatuses"].([]any)
	total, found := 0.0, false
	for _, st := range statuses {
		m, _ := st.(map[string]any)
		r, _ := m["revision"].(string)
		if m["type"] == trafficLatest && short(str(svc, "latestReadyRevision")) == rev {
			r = rev
		}
		if short(r) != rev {
			continue
		}
		pct, _ := m["percent"].(float64)
		total += pct
		found = true
	}
	return int(math.Round(total)), found
}

// CandidateWeight implementa plugin.TrafficRouter.
func (p *Plugin) CandidateWeight(ctx context.Context, req *plugin.Request) (int, error) {
	cand := candidateRevision(req)
	weight := -2
	for _, t := range req.Targets {
		s, err := p.read(ctx, t, req)
		if err != nil {
			return 0, fmt.Errorf("region %s: %w", t.Host, err)
		}
		w, _ := revisionPercent(s.svc, cand)
		if weight != -2 && w != weight {
			return -1, nil
		}
		weight = w
	}
	return max(weight, 0), nil
}

// RouteTesters implementa plugin.TesterRouter: restituisce l'URL del tag della candidata,
// assegnato quando la revisione è stata creata.
func (p *Plugin) RouteTesters(ctx context.Context, req *plugin.Request) (map[string]string, error) {
	if req.Traffic == nil || req.Traffic.TesterTag == "" {
		return nil, errors.New("rollout.traffic.testers.tag obbligatorio con il plugin cloudrun")
	}
	out := map[string]string{}
	for _, t := range req.Targets {
		s, err := p.read(ctx, t, req)
		if err != nil {
			return nil, fmt.Errorf("region %s: %w", t.Host, err)
		}
		statuses, _ := s.svc["trafficStatuses"].([]any)
		for _, st := range statuses {
			m, _ := st.(map[string]any)
			if m["tag"] == req.Traffic.TesterTag {
				if u, _ := m["uri"].(string); u != "" {
					out["tester_url@"+t.Host] = u
				}
			}
		}
		if out["tester_url@"+t.Host] == "" {
			return nil, fmt.Errorf("region %s: tag %s della revisione candidata senza URL", t.Host, req.Traffic.TesterTag)
		}
	}
	return out, nil
}

// PromoteCandidate implementa plugin.SlotDeployer: tutto il traffico all'ultima revisione,
// come dopo un deploy normale (il tag dei tester viene tolto).
func (p *Plugin) PromoteCandidate(ctx context.Context, req *plugin.Request) error {
	for _, t := range req.Targets {
		s, err := p.read(ctx, t, req)
		if err != nil {
			return fmt.Errorf("region %s: %w", t.Host, err)
		}
		if latestOnly(s.svc) && short(str(s.svc, "latestReadyRevision")) == candidateRevision(req) {
			continue
		}
		if err := p.setTraffic(ctx, s, req, []any{Object{"type": trafficLatest, "percent": 100}}); err != nil {
			return fmt.Errorf("region %s: %w", t.Host, err)
		}
	}
	return nil
}

// RevertCandidate implementa plugin.SlotDeployer: tutto il traffico alla revisione stabile.
func (p *Plugin) RevertCandidate(ctx context.Context, req *plugin.Request) error {
	var errs []error
	for _, t := range req.Targets {
		stable := stableOf(req, t)
		if stable == "" {
			continue
		}
		s, err := p.read(ctx, t, req)
		if err != nil {
			errs = append(errs, fmt.Errorf("region %s: %w", t.Host, err))
			continue
		}
		if err := p.setTraffic(ctx, s, req, []any{Object{"type": trafficRevision, "revision": stable, "percent": 100}}); err != nil {
			errs = append(errs, fmt.Errorf("region %s: %w", t.Host, err))
		}
	}
	return errors.Join(errs...)
}

// RetirePrevious implementa plugin.SlotDeployer: le revisioni senza traffico scalano a zero da
// sole, quindi non c'è nulla da fare.
func (p *Plugin) RetirePrevious(context.Context, *plugin.Request) error { return nil }
