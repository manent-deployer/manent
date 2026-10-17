// Package awselb integrates AWS load balancers (ALB and NLB) with manent: the aws-elb pool
// removes and restores the targets of a target group around the deploy, also through the
// standby of an Auto Scaling group, and the aws-alb router shifts traffic between the target
// groups of two slots.
package awselb

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"

	"github.com/joeg-ita/manent/internal/traffic"
	"github.com/joeg-ita/manent/pkg/plugin"
	"github.com/joeg-ita/manent/plugins/internal/awsauth"
)

// Settings of the aws-elb pool, besides the awsauth credential ones.
const (
	SettingRegion         = "region"             // load balancer region
	SettingTargetGroupARN = "target_group_arn"   // target group of the members
	SettingAutoScaling    = "auto_scaling_group" // Auto Scaling group: draining with EnterStandby/ExitStandby
	SettingPort           = "port"               // port of the targets in the target group (default: the target's, or the target group's)
)

// States of the targets in a target group.
const (
	healthHealthy  = "healthy"
	healthDraining = "draining"
	healthUnused   = "unused"
)

var tgARNRe = regexp.MustCompile(`^arn:aws[a-z-]*:elasticloadbalancing:[a-z0-9-]+:[0-9]{12}:targetgroup/[A-Za-z0-9-]{1,32}/[0-9a-f]{16}$`)

// Target is a target of a target group: instance ID (or IP, or ARN) and port.
type Target struct {
	ID   string
	Port int32
}

// TargetHealth is the state of a target in the target group.
type TargetHealth struct {
	Target
	State  string
	Reason string
}

// API are the Elastic Load Balancing operations used by pool and router.
type API interface {
	DescribeTargetHealth(ctx context.Context, targetGroup string, targets []Target) ([]TargetHealth, error)
	RegisterTargets(ctx context.Context, targetGroup string, targets []Target) error
	DeregisterTargets(ctx context.Context, targetGroup string, targets []Target) error
	// ForwardWeights reads the target group weights of the forward action of a listener or rule.
	ForwardWeights(ctx context.Context, listenerOrRule string) (map[string]int32, error)
	// SetForwardWeights sets the target group weights of the forward action.
	SetForwardWeights(ctx context.Context, listenerOrRule string, weights map[string]int32) error
}

// AutoScaling are the Auto Scaling group operations used by the pool.
type AutoScaling interface {
	// LifecycleStates returns the state (InService, Standby, ...) of the instances.
	LifecycleStates(ctx context.Context, group string, ids []string) (map[string]string, error)
	EnterStandby(ctx context.Context, group string, ids []string) error
	ExitStandby(ctx context.Context, group string, ids []string) error
}

// Connector opens access to the services in a region.
type Connector interface {
	Connect(ctx context.Context, region string, req *plugin.Request) (API, AutoScaling, error)
}

// ValidatePool checks the settings of the aws-elb pool.
func ValidatePool(s map[string]string) error {
	var errs []error
	if !awsauth.ValidRegion(s[SettingRegion]) {
		errs = append(errs, fmt.Errorf("invalid %s %q", SettingRegion, s[SettingRegion]))
	}
	if !tgARNRe.MatchString(s[SettingTargetGroupARN]) {
		errs = append(errs, fmt.Errorf("invalid %s %q", SettingTargetGroupARN, s[SettingTargetGroupARN]))
	}
	if p := s[SettingPort]; p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			errs = append(errs, fmt.Errorf("invalid %s %q", SettingPort, p))
		}
	}
	errs = append(errs, awsauth.Validate(s))
	return errors.Join(errs...)
}

// NewPool returns the constructor of the aws-elb pool with the given connector.
func NewPool(c Connector) traffic.PoolFactory {
	return func(ctx context.Context, env traffic.Env) (traffic.Pool, error) {
		if err := ValidatePool(env.Settings); err != nil {
			return nil, err
		}
		req := &plugin.Request{DeploymentID: env.DeploymentID, Params: env.Settings, Secrets: env.Secrets}
		api, asg, err := c.Connect(ctx, env.Settings[SettingRegion], req)
		if err != nil {
			return nil, err
		}
		p := &pool{api: api, asg: asg, tg: env.Settings[SettingTargetGroupARN], group: env.Settings[SettingAutoScaling], ports: map[string]int32{}}
		def := int32(0)
		if v := env.Settings[SettingPort]; v != "" {
			n, _ := strconv.Atoi(v)
			def = int32(n)
		}
		for _, t := range env.Targets {
			port := def
			if port == 0 {
				port = int32(t.Port)
			}
			p.ports[env.Member(t)] = port
		}
		return p, nil
	}
}

type pool struct {
	api   API
	asg   AutoScaling
	tg    string
	group string
	ports map[string]int32
}

func (p *pool) targets(members []string) []Target {
	out := make([]Target, len(members))
	for i, m := range members {
		out[i] = Target{ID: m, Port: p.ports[m]}
	}
	return out
}

// Members implements traffic.Pool: the targets registered in the target group.
func (p *pool) Members(ctx context.Context) ([]traffic.Member, error) {
	list, err := p.api.DescribeTargetHealth(ctx, p.tg, nil)
	if err != nil {
		return nil, err
	}
	var out []traffic.Member
	for _, h := range list {
		m := traffic.Member{Name: h.ID, State: traffic.StateActive, Healthy: h.State == healthHealthy}
		switch h.State {
		case healthDraining:
			m.State = traffic.StateDraining
		case healthUnused:
			m.State = traffic.StateDisabled
		}
		out = append(out, m)
	}
	return out, nil
}

// Drain implements traffic.Pool: with an Auto Scaling group it puts the instances in standby
// (which removes them from the target groups without having them replaced), otherwise it
// deregisters them.
func (p *pool) Drain(ctx context.Context, members []string) error {
	if p.group != "" {
		states, err := p.asg.LifecycleStates(ctx, p.group, members)
		if err != nil {
			return err
		}
		var ids []string
		for _, m := range members {
			if states[m] == "InService" {
				ids = append(ids, m)
			} else if !isStandby(states[m]) {
				return fmt.Errorf("instance %s in state %q in Auto Scaling group %s", m, states[m], p.group)
			}
		}
		if len(ids) == 0 {
			return nil
		}
		return p.asg.EnterStandby(ctx, p.group, ids)
	}
	return p.api.DeregisterTargets(ctx, p.tg, p.targets(members))
}

func isStandby(s string) bool {
	return s == "Standby" || s == "EnteringStandby"
}

// Connections implements traffic.Pool: a draining target still has connections (the number is
// not exposed); one no longer present or unused has none.
func (p *pool) Connections(ctx context.Context, members []string) (map[string]int, error) {
	list, err := p.api.DescribeTargetHealth(ctx, p.tg, p.targets(members))
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	for _, m := range members {
		out[m] = 0
	}
	for _, h := range list {
		if h.State == healthDraining {
			out[h.ID] = 1
		}
	}
	return out, nil
}

// Restore implements traffic.Pool.
func (p *pool) Restore(ctx context.Context, members []string) error {
	if p.group != "" {
		states, err := p.asg.LifecycleStates(ctx, p.group, members)
		if err != nil {
			return err
		}
		var ids []string
		for _, m := range members {
			if isStandby(states[m]) {
				ids = append(ids, m)
			}
		}
		if len(ids) == 0 {
			return nil
		}
		return p.asg.ExitStandby(ctx, p.group, ids)
	}
	return p.api.RegisterTargets(ctx, p.tg, p.targets(members))
}

// Healthy implements traffic.Pool.
func (p *pool) Healthy(ctx context.Context, members []string) (map[string]bool, error) {
	list, err := p.api.DescribeTargetHealth(ctx, p.tg, p.targets(members))
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, m := range members {
		out[m] = false
	}
	for _, h := range list {
		if _, ok := out[h.ID]; ok {
			out[h.ID] = h.State == healthHealthy
		}
	}
	return out, nil
}
