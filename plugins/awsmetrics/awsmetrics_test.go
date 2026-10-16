package awsmetrics

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
)

// fakeAPI answers GetMetricData in two pages and records the inputs.
type fakeAPI struct {
	ins []*cloudwatch.GetMetricDataInput
}

func (f *fakeAPI) GetMetricData(_ context.Context, in *cloudwatch.GetMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
	c := *in
	f.ins = append(f.ins, &c)
	if in.NextToken == nil {
		return &cloudwatch.GetMetricDataOutput{NextToken: aws.String("p2"),
			MetricDataResults: []types.MetricDataResult{{Id: aws.String("m"), Values: []float64{1, 2}, StatusCode: types.StatusCodePartialData}}}, nil
	}
	return &cloudwatch.GetMetricDataOutput{MetricDataResults: []types.MetricDataResult{{Id: aws.String("m"), Values: []float64{3}, StatusCode: types.StatusCodeComplete}}}, nil
}

func TestSeriesEValore(t *testing.T) {
	api := &fakeAPI{}
	s := &Source{API: api}
	end := time.Unix(1700000300, 0)
	v, err := s.Series(context.Background(), `SELECT AVG(Latency) FROM "AWS/ApplicationELB"`, end.Add(-5*time.Minute), end, 15*time.Second)
	if err != nil || len(v) != 3 || v[2] != 3 {
		t.Fatalf("serie: %v %v", v, err)
	}
	q := api.ins[0].MetricDataQueries[0]
	if *q.Period != 30 || *q.Expression == "" || api.ins[0].ScanBy != types.ScanByTimestampAscending || *api.ins[1].NextToken != "p2" {
		t.Errorf("richiesta: periodo %d, %+v", *q.Period, api.ins)
	}
	if x, err := s.Value(context.Background(), "m", end); err != nil || x != 3 {
		t.Errorf("valore: %v %v", x, err)
	}
	for step, want := range map[time.Duration]int32{time.Second: 1, 7 * time.Second: 10, time.Minute: 60, 90 * time.Second: 120} {
		if got := period(step); got != want {
			t.Errorf("periodo di %s: %d, atteso %d", step, got, want)
		}
	}
}
