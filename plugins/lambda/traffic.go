package lambda

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"

	"github.com/joeg-ita/manent/pkg/plugin"
	"github.com/joeg-ita/manent/plugins/internal/pluginutil"
)

// Percentage rollouts: published versions are the slots and the alias with an additional
// weight (AdditionalVersionWeights) splits traffic between the stable version and the
// candidate. Whoever invokes the alias gets the stable or the candidate according to the weight.

// Keys of the state recorded by PrepareSlots, per region ("<key>@<region>").
const (
	stateStable     = "stable"      // stable function version
	stateStableDesc = "stable_desc" // description of the stable alias (version and deploy)
)

func stateKey(k string, t plugin.Target) string { return k + "@" + t.Host }

// PrepareSlots implements plugin.SlotDeployer: it records the function version the alias
// points to in every region.
func (p *Plugin) PrepareSlots(ctx context.Context, req *plugin.Request) (map[string]string, error) {
	state := map[string]string{}
	has := false
	for _, t := range req.Targets {
		s, err := p.read(ctx, t, req)
		if err != nil {
			return nil, fmt.Errorf("region %s: %w", t.Host, err)
		}
		if s.alias == nil {
			continue
		}
		if len(s.alias.Weights) > 0 {
			return nil, fmt.Errorf("region %s: alias %s already splits traffic (%v): finish the release in progress first", t.Host, aliasName(req), s.alias.Weights)
		}
		state[stateKey(stateStable, t)], state[stateKey(stateStableDesc, t)] = s.alias.FunctionVersion, s.alias.Description
		has = true
	}
	state[plugin.StateHasStable] = strconv.FormatBool(has)
	return state, nil
}

// DeployCandidate implements plugin.SlotDeployer: it updates the code and publishes the
// version, without moving the alias.
func (p *Plugin) DeployCandidate(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	res := &plugin.Result{Plugin: Name}
	for _, t := range req.Targets {
		s, err := p.read(ctx, t, req)
		if err != nil {
			res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusFailed})
			return res, fmt.Errorf("region %s: %w", t.Host, err)
		}
		details := s.details(req)
		log := p.logger.With("deployment", req.DeploymentID, "region", s.region, "function", functionName(req))
		timeout, _ := pluginutil.Duration(req.Params, ParamWaitTimeout, defaultWaitTimeout)
		tctx, cancel := context.WithTimeout(ctx, timeout)
		v, err := p.publish(tctx, s, req, details, log)
		cancel()
		if err != nil {
			res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusFailed, Details: details})
			return res, fmt.Errorf("region %s: %w", t.Host, err)
		}
		log.InfoContext(ctx, "candidate version published, alias unchanged", "function_version", v)
		res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusDeployed, Details: details})
	}
	return res, nil
}

// candidate returns the function version published by this rollout.
func (p *Plugin) candidate(ctx context.Context, s *state, req *plugin.Request) (string, error) {
	v, dep, err := p.findVersion(ctx, s.api, functionName(req), req.Artifact.Version)
	if err != nil {
		return "", err
	}
	if dep != req.DeploymentID {
		return "", fmt.Errorf("version %s of the function was published by another deploy (%s)", v, dep)
	}
	return v, nil
}

// SetCandidateWeight implements plugin.TrafficRouter.
func (p *Plugin) SetCandidateWeight(ctx context.Context, req *plugin.Request, percent int) error {
	for _, t := range req.Targets {
		if err := p.setWeight(ctx, t, req, percent); err != nil {
			return fmt.Errorf("region %s: %w", t.Host, err)
		}
	}
	return nil
}

func (p *Plugin) setWeight(ctx context.Context, t plugin.Target, req *plugin.Request, percent int) error {
	s, err := p.read(ctx, t, req)
	if err != nil {
		return err
	}
	cand, err := p.candidate(ctx, s, req)
	if err != nil {
		return err
	}
	name, alias := functionName(req), aliasName(req)
	stable := ""
	if req.Traffic != nil {
		stable = req.Traffic.State[stateKey(stateStable, t)]
	}
	switch {
	case stable == "" || percent >= 100 || stable == cand:
		// All traffic to the candidate: the alias points to it, without weights.
		if s.alias != nil && s.alias.FunctionVersion == cand && len(s.alias.Weights) == 0 {
			return nil
		}
		err = s.api.PutAlias(ctx, name, alias, cand, description(req.Artifact.Version, req.DeploymentID), s.alias == nil)
	case percent <= 0:
		err = s.api.PutAlias(ctx, name, alias, stable, req.Traffic.State[stateKey(stateStableDesc, t)], false)
	default:
		err = s.api.RouteAlias(ctx, name, alias, stable, req.Traffic.State[stateKey(stateStableDesc, t)], map[string]float64{cand: float64(percent) / 100})
	}
	if err != nil {
		return fmt.Errorf("updating alias %s: %w", alias, err)
	}
	p.logger.InfoContext(ctx, "alias traffic updated", "deployment", req.DeploymentID, "region", t.Host, "alias", alias, "candidate", cand, "percent", percent)
	return nil
}

// CandidateWeight implements plugin.TrafficRouter.
func (p *Plugin) CandidateWeight(ctx context.Context, req *plugin.Request) (int, error) {
	weight := -2
	for _, t := range req.Targets {
		s, err := p.read(ctx, t, req)
		if err != nil {
			return 0, fmt.Errorf("region %s: %w", t.Host, err)
		}
		cand, err := p.candidate(ctx, s, req)
		if err != nil {
			return 0, fmt.Errorf("region %s: %w", t.Host, err)
		}
		w := 0
		switch {
		case s.alias == nil:
		case s.alias.FunctionVersion == cand:
			w = 100
		default:
			w = int(math.Round(s.alias.Weights[cand] * 100))
		}
		if weight != -2 && w != weight {
			return -1, nil
		}
		weight = w
	}
	return max(weight, 0), nil
}

// RouteTesters implements plugin.TesterRouter: testers invoke the candidate version directly
// (ARN qualified with the version number), which already has the code.
func (p *Plugin) RouteTesters(ctx context.Context, req *plugin.Request) (map[string]string, error) {
	out := map[string]string{}
	for _, t := range req.Targets {
		s, err := p.read(ctx, t, req)
		if err != nil {
			return nil, fmt.Errorf("region %s: %w", t.Host, err)
		}
		cand, err := p.candidate(ctx, s, req)
		if err != nil {
			return nil, fmt.Errorf("region %s: %w", t.Host, err)
		}
		out["tester_qualifier@"+t.Host] = cand
	}
	return out, nil
}

// PromoteCandidate implements plugin.SlotDeployer: the alias points to the candidate.
func (p *Plugin) PromoteCandidate(ctx context.Context, req *plugin.Request) error {
	return p.SetCandidateWeight(ctx, req, 100)
}

// RevertCandidate implements plugin.SlotDeployer: the alias goes back to the stable version,
// without weights. Without a stable version (first release) there is nothing to go back to.
func (p *Plugin) RevertCandidate(ctx context.Context, req *plugin.Request) error {
	var errs []error
	for _, t := range req.Targets {
		if req.Traffic == nil || req.Traffic.State[stateKey(stateStable, t)] == "" {
			continue
		}
		if err := p.setWeight(ctx, t, req, 0); err != nil {
			errs = append(errs, fmt.Errorf("region %s: %w", t.Host, err))
		}
	}
	return errors.Join(errs...)
}

// RetirePrevious implements plugin.SlotDeployer: versions the alias does not point to use no
// resources, so there is nothing to scale down.
func (p *Plugin) RetirePrevious(context.Context, *plugin.Request) error { return nil }
