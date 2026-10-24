package awselb

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	elb "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"

	"github.com/manent-deployer/manent/pkg/plugin"
	"github.com/manent-deployer/manent/plugins/internal/awsauth"
)

// SDKConnector opens access to Elastic Load Balancing and Auto Scaling with the AWS SDK.
type SDKConnector struct {
	// Endpoint, if set, replaces the services' one (e.g. LocalStack).
	Endpoint *string
}

// Connect implements Connector.
func (c SDKConnector) Connect(ctx context.Context, region string, req *plugin.Request) (API, AutoScaling, error) {
	cfg, err := awsauth.Config(ctx, region, req)
	if err != nil {
		return nil, nil, err
	}
	return sdkELB{elb.NewFromConfig(cfg, func(o *elb.Options) { o.BaseEndpoint = c.Endpoint })},
		sdkASG{autoscaling.NewFromConfig(cfg, func(o *autoscaling.Options) { o.BaseEndpoint = c.Endpoint })}, nil
}

type sdkELB struct{ c *elb.Client }

func descriptions(ts []Target) []types.TargetDescription {
	out := make([]types.TargetDescription, len(ts))
	for i, t := range ts {
		out[i] = types.TargetDescription{Id: aws.String(t.ID)}
		if t.Port != 0 {
			out[i].Port = aws.Int32(t.Port)
		}
	}
	return out
}

func (a sdkELB) DescribeTargetHealth(ctx context.Context, tg string, ts []Target) ([]TargetHealth, error) {
	in := &elb.DescribeTargetHealthInput{TargetGroupArn: aws.String(tg)}
	if len(ts) > 0 {
		in.Targets = descriptions(ts)
	}
	out, err := a.c.DescribeTargetHealth(ctx, in)
	if err != nil {
		return nil, err
	}
	var list []TargetHealth
	for _, d := range out.TargetHealthDescriptions {
		h := TargetHealth{Target: Target{ID: aws.ToString(d.Target.Id), Port: aws.ToInt32(d.Target.Port)}}
		if d.TargetHealth != nil {
			h.State, h.Reason = string(d.TargetHealth.State), string(d.TargetHealth.Reason)
		}
		list = append(list, h)
	}
	return list, nil
}

func (a sdkELB) RegisterTargets(ctx context.Context, tg string, ts []Target) error {
	_, err := a.c.RegisterTargets(ctx, &elb.RegisterTargetsInput{TargetGroupArn: aws.String(tg), Targets: descriptions(ts)})
	return err
}

func (a sdkELB) DeregisterTargets(ctx context.Context, tg string, ts []Target) error {
	_, err := a.c.DeregisterTargets(ctx, &elb.DeregisterTargetsInput{TargetGroupArn: aws.String(tg), Targets: descriptions(ts)})
	return err
}

// isRule tells a rule ARN apart from a listener ARN.
func isRule(arn string) bool { return strings.Contains(arn, ":listener-rule/") }

func (a sdkELB) actions(ctx context.Context, arn string) ([]types.Action, error) {
	if isRule(arn) {
		out, err := a.c.DescribeRules(ctx, &elb.DescribeRulesInput{RuleArns: []string{arn}})
		if err != nil {
			return nil, err
		}
		if len(out.Rules) == 0 {
			return nil, fmt.Errorf("rule %s not found", arn)
		}
		return out.Rules[0].Actions, nil
	}
	out, err := a.c.DescribeListeners(ctx, &elb.DescribeListenersInput{ListenerArns: []string{arn}})
	if err != nil {
		return nil, err
	}
	if len(out.Listeners) == 0 {
		return nil, fmt.Errorf("listener %s not found", arn)
	}
	return out.Listeners[0].DefaultActions, nil
}

func (a sdkELB) ForwardWeights(ctx context.Context, arn string) (map[string]int32, error) {
	acts, err := a.actions(ctx, arn)
	if err != nil {
		return nil, err
	}
	for _, act := range acts {
		if act.Type != types.ActionTypeEnumForward {
			continue
		}
		out := map[string]int32{}
		if act.ForwardConfig != nil {
			for _, tg := range act.ForwardConfig.TargetGroups {
				out[aws.ToString(tg.TargetGroupArn)] = aws.ToInt32(tg.Weight)
			}
		} else if act.TargetGroupArn != nil {
			out[*act.TargetGroupArn] = 1
		}
		return out, nil
	}
	return nil, errors.New("no forward action")
}

func (a sdkELB) SetForwardWeights(ctx context.Context, arn string, weights map[string]int32) error {
	var tgs []types.TargetGroupTuple
	for tg, w := range weights {
		tgs = append(tgs, types.TargetGroupTuple{TargetGroupArn: aws.String(tg), Weight: aws.Int32(w)})
	}
	act := []types.Action{{Type: types.ActionTypeEnumForward, ForwardConfig: &types.ForwardActionConfig{TargetGroups: tgs}}}
	if isRule(arn) {
		_, err := a.c.ModifyRule(ctx, &elb.ModifyRuleInput{RuleArn: aws.String(arn), Actions: act})
		return err
	}
	_, err := a.c.ModifyListener(ctx, &elb.ModifyListenerInput{ListenerArn: aws.String(arn), DefaultActions: act})
	return err
}

type sdkASG struct{ c *autoscaling.Client }

func (a sdkASG) LifecycleStates(ctx context.Context, group string, ids []string) (map[string]string, error) {
	out, err := a.c.DescribeAutoScalingInstances(ctx, &autoscaling.DescribeAutoScalingInstancesInput{InstanceIds: ids})
	if err != nil {
		return nil, err
	}
	states := map[string]string{}
	for _, i := range out.AutoScalingInstances {
		if aws.ToString(i.AutoScalingGroupName) == group {
			states[aws.ToString(i.InstanceId)] = aws.ToString(i.LifecycleState)
		}
	}
	return states, nil
}

func (a sdkASG) EnterStandby(ctx context.Context, group string, ids []string) error {
	_, err := a.c.EnterStandby(ctx, &autoscaling.EnterStandbyInput{
		AutoScalingGroupName: aws.String(group), InstanceIds: ids, ShouldDecrementDesiredCapacity: aws.Bool(true),
	})
	return err
}

func (a sdkASG) ExitStandby(ctx context.Context, group string, ids []string) error {
	_, err := a.c.ExitStandby(ctx, &autoscaling.ExitStandbyInput{AutoScalingGroupName: aws.String(group), InstanceIds: ids})
	return err
}
