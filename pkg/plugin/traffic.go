package plugin

import (
	"context"
	"errors"
)

// TrafficRequest is the data of a traffic percentage rollout (rollout with strategy:
// traffic): the new version, the candidate, is released next to the one in service, the
// stable, and traffic shifts in steps.
type TrafficRequest struct {
	// State is the state recorded by PrepareSlots before the rollout (e.g. slot or stable version
	// of every target): every later call receives it unchanged, even after a recovery. It must not
	// contain secrets.
	State map[string]string `json:"state,omitempty"`
	// TesterHeader is the header ("name: value") of the testers' requests.
	TesterHeader string `json:"tester_header,omitempty"`
	// TesterTag is the candidate's label for routers that give testers a URL.
	TesterTag string `json:"tester_tag,omitempty"`
	// ProportionalReplicas asks for a small candidate, sized by ScaleCandidate in proportion to
	// its traffic share (rollout.traffic.replicas: proportional).
	ProportionalReplicas bool `json:"proportional_replicas,omitempty"`
}

// StateHasStable is the TrafficRequest.State key that PrepareSlots sets to "true" if a stable
// version exists; without it, the first release goes straight to 100%.
const StateHasStable = "has_stable"

// SlotDeployer is an optional interface of the Deployer: it releases the new version next to
// the one in service, without traffic, and promotes or retires it. All methods must be
// idempotent: retries and recoveries can run them again.
type SlotDeployer interface {
	// PrepareSlots reads the state of the targets before the rollout (stable version, free slot).
	// It must change nothing. The result arrives in Request.Traffic.State.
	PrepareSlots(ctx context.Context, req *Request) (map[string]string, error)
	// DeployCandidate releases the candidate without shifting traffic.
	DeployCandidate(ctx context.Context, req *Request) (*Result, error)
	// PromoteCandidate makes the candidate the release in service (all traffic and history).
	PromoteCandidate(ctx context.Context, req *Request) error
	// RevertCandidate brings all traffic and history back to the stable version and retires the
	// candidate, even if it had already been promoted.
	RevertCandidate(ctx context.Context, req *Request) error
	// RetirePrevious scales down the replaced version, after the promotion. It must not touch
	// the version in service, even if another release changed it meanwhile.
	RetirePrevious(ctx context.Context, req *Request) error
}

// TrafficRouter is an optional interface of the Deployer: it shifts traffic between the
// stable version and the candidate.
type TrafficRouter interface {
	// SetCandidateWeight sends percent% of the traffic to the candidate, the rest to the stable.
	SetCandidateWeight(ctx context.Context, req *Request, percent int) error
	// CandidateWeight reads the candidate's share; -1 if the targets disagree.
	CandidateWeight(ctx context.Context, req *Request) (int, error)
}

// TesterRouter is an optional interface of the TrafficRouter: it routes only the testers'
// requests to the candidate. It returns the details to show (e.g. the testers' URL).
type TesterRouter interface {
	RouteTesters(ctx context.Context, req *Request) (map[string]string, error)
}

// SlotScaler is an optional interface of the SlotDeployer: it sizes the candidate in
// proportion to its traffic share (rollout.traffic.replicas: proportional).
type SlotScaler interface {
	ScaleCandidate(ctx context.Context, req *Request, percent int) error
}

// TrafficSupport are the percentage rollout interfaces supported by a plugin.
type TrafficSupport struct {
	Slots, Router, Testers, Scale bool
}

// TrafficChecker is implemented by Deployers that know percentage rollout support only at
// runtime (e.g. an external plugin, which declares it when the process starts). Their
// SlotDeployer, TrafficRouter, TesterRouter and SlotScaler methods return
// ErrTrafficUnsupported if the plugin does not implement them.
type TrafficChecker interface {
	TrafficSupported(ctx context.Context) (TrafficSupport, error)
}

// ErrTrafficUnsupported means a plugin that does not support a percentage rollout operation.
var ErrTrafficUnsupported = errors.New("percentage release operation not supported by the plugin")

// SupportsTraffic reports the percentage rollout interfaces supported by the Deployer.
func SupportsTraffic(ctx context.Context, d Deployer) (TrafficSupport, error) {
	if c, ok := d.(TrafficChecker); ok {
		return c.TrafficSupported(ctx)
	}
	var s TrafficSupport
	_, s.Slots = d.(SlotDeployer)
	_, s.Router = d.(TrafficRouter)
	_, s.Testers = d.(TesterRouter)
	_, s.Scale = d.(SlotScaler)
	return s, nil
}
