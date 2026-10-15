// Package awselb integra i load balancer AWS (ALB e NLB) con manent: il pool aws-elb toglie e
// rimette i target di un target group intorno al deploy, anche tramite lo standby di un Auto
// Scaling group, e il router aws-alb sposta il traffico tra i target group di due slot.
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

// Impostazioni del pool aws-elb, oltre a quelle delle credenziali di awsauth.
const (
	SettingRegion         = "region"             // region del load balancer
	SettingTargetGroupARN = "target_group_arn"   // target group dei membri
	SettingAutoScaling    = "auto_scaling_group" // Auto Scaling group: svuotamento con EnterStandby/ExitStandby
	SettingPort           = "port"               // porta dei target nel target group (default: quella del target, o del target group)
)

// Stati dei target in un target group.
const (
	healthHealthy  = "healthy"
	healthDraining = "draining"
	healthUnused   = "unused"
)

var tgARNRe = regexp.MustCompile(`^arn:aws[a-z-]*:elasticloadbalancing:[a-z0-9-]+:[0-9]{12}:targetgroup/[A-Za-z0-9-]{1,32}/[0-9a-f]{16}$`)

// Target è un target di un target group: ID dell'istanza (o IP, o ARN) e porta.
type Target struct {
	ID   string
	Port int32
}

// TargetHealth è lo stato di un target nel target group.
type TargetHealth struct {
	Target
	State  string
	Reason string
}

// API sono le operazioni di Elastic Load Balancing usate dal pool e dal router.
type API interface {
	DescribeTargetHealth(ctx context.Context, targetGroup string, targets []Target) ([]TargetHealth, error)
	RegisterTargets(ctx context.Context, targetGroup string, targets []Target) error
	DeregisterTargets(ctx context.Context, targetGroup string, targets []Target) error
	// ForwardWeights legge i pesi dei target group dell'azione forward di un listener o di una regola.
	ForwardWeights(ctx context.Context, listenerOrRule string) (map[string]int32, error)
	// SetForwardWeights imposta i pesi dei target group dell'azione forward.
	SetForwardWeights(ctx context.Context, listenerOrRule string, weights map[string]int32) error
}

// AutoScaling sono le operazioni sugli Auto Scaling group usate dal pool.
type AutoScaling interface {
	// LifecycleStates restituisce lo stato (InService, Standby, ...) delle istanze.
	LifecycleStates(ctx context.Context, group string, ids []string) (map[string]string, error)
	EnterStandby(ctx context.Context, group string, ids []string) error
	ExitStandby(ctx context.Context, group string, ids []string) error
}

// Connector apre l'accesso ai servizi in una region.
type Connector interface {
	Connect(ctx context.Context, region string, req *plugin.Request) (API, AutoScaling, error)
}

// ValidatePool verifica le impostazioni del pool aws-elb.
func ValidatePool(s map[string]string) error {
	var errs []error
	if !awsauth.ValidRegion(s[SettingRegion]) {
		errs = append(errs, fmt.Errorf("%s %q non valida", SettingRegion, s[SettingRegion]))
	}
	if !tgARNRe.MatchString(s[SettingTargetGroupARN]) {
		errs = append(errs, fmt.Errorf("%s %q non valido", SettingTargetGroupARN, s[SettingTargetGroupARN]))
	}
	if p := s[SettingPort]; p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			errs = append(errs, fmt.Errorf("%s %q non valida", SettingPort, p))
		}
	}
	errs = append(errs, awsauth.Validate(s))
	return errors.Join(errs...)
}

// NewPool restituisce il costruttore del pool aws-elb con il connettore indicato.
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

// Members implementa traffic.Pool: i target registrati nel target group.
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

// Drain implementa traffic.Pool: con un Auto Scaling group mette le istanze in standby (che le
// toglie dai target group senza farle sostituire), altrimenti le deregistra.
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
				return fmt.Errorf("istanza %s nello stato %q nell'Auto Scaling group %s", m, states[m], p.group)
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

// Connections implementa traffic.Pool: un target in draining ha ancora connessioni (il numero
// non è esposto); uno non più presente o unused no.
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

// Restore implementa traffic.Pool.
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

// Healthy implementa traffic.Pool.
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
