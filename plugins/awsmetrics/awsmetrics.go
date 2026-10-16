// Package awsmetrics reads the health gate metrics from Amazon CloudWatch: the query is a
// metric math expression or a Metrics Insights query (SELECT ...), read with GetMetricData.
package awsmetrics

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"

	"github.com/joeg-ita/manent/internal/rollout"
	"github.com/joeg-ita/manent/pkg/plugin"
	"github.com/joeg-ita/manent/plugins/internal/awsauth"
)

// API is the CloudWatch operation used by the source; abstracted for tests.
type API interface {
	GetMetricData(ctx context.Context, in *cloudwatch.GetMetricDataInput, opts ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error)
}

// Source is a rollout.MetricSource on CloudWatch.
type Source struct {
	Region   string
	Endpoint string
	// Credentials: secret references of the access keys (empty: SDK default chain) and the
	// role to assume.
	AccessKeyIDRef, SecretAccessKeyRef, RoleARN, ExternalID string
	Secret                                                  rollout.SecretFunc
	// API, if set, replaces the client built from the configuration (tests).
	API API

	once sync.Once
	api  API
	err  error
}

var _ rollout.MetricSource = (*Source)(nil)

// secrets resolves the references used as names by awsauth.
type secrets struct{ f rollout.SecretFunc }

func (s secrets) Secret(ctx context.Context, ref string) (string, error) { return s.f(ctx, ref) }

// client builds the CloudWatch client once: the SDK refreshes credentials and assumed roles.
func (s *Source) client(ctx context.Context) (API, error) {
	if s.API != nil {
		return s.API, nil
	}
	s.once.Do(func() {
		req := &plugin.Request{DeploymentID: "metrics", Secrets: secrets{s.Secret}, Params: map[string]string{
			awsauth.ParamAccessKeySecret: s.AccessKeyIDRef, awsauth.ParamSecretKeySecret: s.SecretAccessKeyRef,
			awsauth.ParamRoleARN: s.RoleARN, awsauth.ParamExternalID: s.ExternalID,
		}}
		// The client lives longer than the gate read that creates it.
		cfg, err := awsauth.Config(context.WithoutCancel(ctx), s.Region, req)
		if err != nil {
			s.err = fmt.Errorf("cloudwatch: credenziali: %w", err)
			return
		}
		s.api = cloudwatch.NewFromConfig(cfg, func(o *cloudwatch.Options) {
			if s.Endpoint != "" {
				o.BaseEndpoint = aws.String(s.Endpoint)
			}
		})
	})
	return s.api, s.err
}

// period returns a period CloudWatch accepts: 1, 5, 10, 30 or a multiple of 60 seconds.
func period(step time.Duration) int32 {
	secs := int32(step.Seconds())
	switch {
	case secs <= 1:
		return 1
	case secs <= 5:
		return 5
	case secs <= 10:
		return 10
	case secs <= 30:
		return 30
	}
	return (secs + 59) / 60 * 60
}

// Value implements rollout.MetricSource: the last point over the last five minutes.
func (s *Source) Value(ctx context.Context, query string, at time.Time) (float64, error) {
	v, err := s.Series(ctx, query, at.Add(-5*time.Minute), at, time.Minute)
	if err != nil {
		return 0, err
	}
	if len(v) == 0 {
		return 0, rollout.ErrNoData
	}
	return v[len(v)-1], nil
}

// Series implements rollout.MetricSource, oldest point first.
func (s *Source) Series(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]float64, error) {
	api, err := s.client(ctx)
	if err != nil {
		return nil, err
	}
	in := &cloudwatch.GetMetricDataInput{
		StartTime: aws.Time(start), EndTime: aws.Time(end), ScanBy: types.ScanByTimestampAscending,
		MetricDataQueries: []types.MetricDataQuery{{Id: aws.String("m"), Expression: aws.String(query), Period: aws.Int32(period(step)), ReturnData: aws.Bool(true)}},
	}
	var vals []float64
	for {
		out, err := api.GetMetricData(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("cloudwatch: %w", err)
		}
		for _, r := range out.MetricDataResults {
			if r.StatusCode == types.StatusCodeInternalError || r.StatusCode == types.StatusCodeForbidden {
				return nil, errors.New("cloudwatch: " + string(r.StatusCode))
			}
			vals = append(vals, r.Values...)
		}
		if out.NextToken == nil {
			return vals, nil
		}
		in.NextToken = out.NextToken
	}
}
