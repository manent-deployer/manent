package awselb

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/traffic"
	"github.com/joeg-ita/manent/pkg/plugin"
	"github.com/joeg-ita/manent/plugins/internal/awsauth"
)

// Settings of the aws-alb router, besides region and credentials.
const (
	SettingForwardARN    = "forward_arn"     // listener or rule with the weighted forward action
	SettingTargetGroupA  = "target_group_a"  // target group of slot a
	SettingTargetGroupB  = "target_group_b"  // target group of slot b
	SettingTesterRuleARN = "tester_rule_arn" // rule (e.g. with a header condition) that leads testers to the candidate
)

var forwardARNRe = regexp.MustCompile(`^arn:aws[a-z-]*:elasticloadbalancing:[a-z0-9-]+:[0-9]{12}:listener(-rule)?/app/[A-Za-z0-9-]+/[0-9a-f]+/[0-9a-f]+(/[0-9a-f]+)?$`)

// ValidateRouter checks the settings of the aws-alb router.
func ValidateRouter(s map[string]string) error {
	var errs []error
	if !awsauth.ValidRegion(s[SettingRegion]) {
		errs = append(errs, fmt.Errorf("%s %q non valida", SettingRegion, s[SettingRegion]))
	}
	if !forwardARNRe.MatchString(s[SettingForwardARN]) {
		errs = append(errs, fmt.Errorf("%s %q non valido: listener o regola di un Application Load Balancer", SettingForwardARN, s[SettingForwardARN]))
	}
	if r := s[SettingTesterRuleARN]; r != "" && !forwardARNRe.MatchString(r) {
		errs = append(errs, fmt.Errorf("%s %q non valido", SettingTesterRuleARN, r))
	}
	for _, k := range []string{SettingTargetGroupA, SettingTargetGroupB} {
		if !tgARNRe.MatchString(s[k]) {
			errs = append(errs, fmt.Errorf("%s %q non valido", k, s[k]))
		}
	}
	if s[SettingTargetGroupA] != "" && s[SettingTargetGroupA] == s[SettingTargetGroupB] {
		errs = append(errs, errors.New("i target group dei due slot devono essere diversi"))
	}
	errs = append(errs, awsauth.Validate(s))
	return errors.Join(errs...)
}

// NewRouter returns the constructor of the aws-alb router: the weights of the two slots'
// target groups in the forward action of an ALB listener or rule.
func NewRouter(c Connector) traffic.RouterFactory {
	return func(ctx context.Context, env traffic.Env) (traffic.Router, error) {
		if err := ValidateRouter(env.Settings); err != nil {
			return nil, err
		}
		req := &plugin.Request{DeploymentID: env.DeploymentID, Params: env.Settings, Secrets: env.Secrets}
		api, _, err := c.Connect(ctx, env.Settings[SettingRegion], req)
		if err != nil {
			return nil, err
		}
		return &router{api: api, s: env.Settings, tg: map[string]string{"a": env.Settings[SettingTargetGroupA], "b": env.Settings[SettingTargetGroupB]}}, nil
	}
}

type router struct {
	api API
	s   map[string]string
	tg  map[string]string
}

// Weights implements traffic.Router.
func (r *router) Weights(ctx context.Context) (map[string]int, error) {
	w, err := r.api.ForwardWeights(ctx, r.s[SettingForwardARN])
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	total := 0
	for _, slot := range descriptor.Slots {
		total += int(w[r.tg[slot]])
	}
	for _, slot := range descriptor.Slots {
		if total > 0 {
			out[slot] = int(w[r.tg[slot]]) * 100 / total
		}
	}
	return out, nil
}

// SetWeights implements traffic.Router: the forward action always holds both target groups.
func (r *router) SetWeights(ctx context.Context, weights map[string]int) error {
	w := map[string]int32{}
	for _, slot := range descriptor.Slots {
		w[r.tg[slot]] = int32(weights[slot])
	}
	return r.api.SetForwardWeights(ctx, r.s[SettingForwardARN], w)
}

// RouteTesters implements traffic.TesterRouter: the testers' rule leads only to the slot.
func (r *router) RouteTesters(ctx context.Context, slot string) (map[string]string, error) {
	rule := r.s[SettingTesterRuleARN]
	if rule == "" {
		return nil, fmt.Errorf("%s non configurato: nessuna regola per i tester", SettingTesterRuleARN)
	}
	if err := r.api.SetForwardWeights(ctx, rule, map[string]int32{r.tg[slot]: 1}); err != nil {
		return nil, err
	}
	return map[string]string{"tester_rule": rule}, nil
}
