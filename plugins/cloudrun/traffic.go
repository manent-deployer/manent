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

// Percentage rollouts: revisions are the slots. The candidate is a revision with a fixed name
// (derived from the deploy) created without traffic; the service traffic is split between
// the stable revision and the candidate, and testers reach it through its tag URL.

// stateStable is the state key with the stable revision of a region ("stable@<region>").
const stateStable = "stable"

func stateKey(k string, t plugin.Target) string { return k + "@" + t.Host }

// candidateRevision is the name of the candidate revision: <service>-m<deploy hash>.
func candidateRevision(req *plugin.Request) string {
	sum := sha256.Sum256([]byte(req.DeploymentID))
	return serviceName(req) + "-m" + hex.EncodeToString(sum[:])[:10]
}

// PrepareSlots implements plugin.SlotDeployer: it records the revision receiving traffic.
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

// split returns the traffic between stable and candidate, with the testers' tag on the candidate.
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

// DeployCandidate implements plugin.SlotDeployer: it creates the candidate revision with
// traffic held on the stable revision.
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
			return details, fmt.Errorf("creating the candidate revision: %w", err)
		}
		if err := p.wait(ctx, s.api, op, timeout); err != nil {
			return details, err
		}
	}
	// An operation ended without errors guarantees a ready revision; only check that it exists
	// with the expected name.
	if _, err := s.api.GetRevision(ctx, s.name+"/revisions/"+cand); err != nil {
		return details, fmt.Errorf("reading revision %s: %w", cand, err)
	}
	p.logger.InfoContext(ctx, "revisione candidata pronta, senza traffico", "deployment", req.DeploymentID, "region", s.region, "revision", cand, "stable", stable)
	return details, nil
}

// setTraffic updates the service traffic and waits for the operation.
func (p *Plugin) setTraffic(ctx context.Context, s *state, req *plugin.Request, traffic []any) error {
	timeout, _ := pluginutil.Duration(req.Params, ParamWaitTimeout, defaultWaitTimeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	s.svc["traffic"] = traffic
	op, err := s.api.UpdateService(ctx, s.name, s.svc, false)
	if err != nil {
		return fmt.Errorf("shifting traffic: %w", err)
	}
	return p.wait(ctx, s.api, op, timeout)
}

// SetCandidateWeight implements plugin.TrafficRouter.
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

// revisionPercent reads a revision's traffic share from the service status.
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

// CandidateWeight implements plugin.TrafficRouter.
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

// RouteTesters implements plugin.TesterRouter: it returns the URL of the candidate's tag,
// assigned when the revision was created.
func (p *Plugin) RouteTesters(ctx context.Context, req *plugin.Request) (map[string]string, error) {
	if req.Traffic == nil || req.Traffic.TesterTag == "" {
		return nil, errors.New("rollout.traffic.testers.tag is required with the cloudrun plugin")
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
			return nil, fmt.Errorf("region %s: tag %s of the candidate revision without URL", t.Host, req.Traffic.TesterTag)
		}
	}
	return out, nil
}

// PromoteCandidate implements plugin.SlotDeployer: all traffic to the latest revision, as
// after a normal deploy (the testers' tag is removed).
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

// RevertCandidate implements plugin.SlotDeployer: all traffic to the stable revision.
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

// RetirePrevious implements plugin.SlotDeployer: revisions without traffic scale to zero by
// themselves, so there is nothing to do.
func (p *Plugin) RetirePrevious(context.Context, *plugin.Request) error { return nil }
