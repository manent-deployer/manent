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

// Impostazioni del router aws-alb, oltre a region e credenziali.
const (
	SettingForwardARN    = "forward_arn"     // listener o regola con l'azione forward pesata
	SettingTargetGroupA  = "target_group_a"  // target group dello slot a
	SettingTargetGroupB  = "target_group_b"  // target group dello slot b
	SettingTesterRuleARN = "tester_rule_arn" // regola (es. con una condizione su un header) che porta i tester alla candidata
)

var forwardARNRe = regexp.MustCompile(`^arn:aws[a-z-]*:elasticloadbalancing:[a-z0-9-]+:[0-9]{12}:listener(-rule)?/app/[A-Za-z0-9-]+/[0-9a-f]+/[0-9a-f]+(/[0-9a-f]+)?$`)

// ValidateRouter verifica le impostazioni del router aws-alb.
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

// NewRouter restituisce il costruttore del router aws-alb: i pesi dei target group dei due slot
// nell'azione forward di un listener o di una regola dell'ALB.
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

// Weights implementa traffic.Router.
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

// SetWeights implementa traffic.Router: l'azione forward contiene sempre i due target group.
func (r *router) SetWeights(ctx context.Context, weights map[string]int) error {
	w := map[string]int32{}
	for _, slot := range descriptor.Slots {
		w[r.tg[slot]] = int32(weights[slot])
	}
	return r.api.SetForwardWeights(ctx, r.s[SettingForwardARN], w)
}

// RouteTesters implementa traffic.TesterRouter: la regola dei tester porta solo allo slot.
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
