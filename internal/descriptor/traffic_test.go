package descriptor

import (
	"strings"
	"testing"
)

func TestRolloutTraffic(t *testing.T) {
	ok := &Rollout{Strategy: StrategyTraffic, Traffic: &TrafficRollout{Steps: []int{1, 5, 25, 100}, Bake: "10m",
		Testers: &Testers{Header: "x-canary: always"}}, ApprovalsBetween: []string{"0", "25"}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	if ok.OnFailureOrDefault() != OnFailureRollbackAll {
		t.Errorf("default di onFailure: %s", ok.OnFailureOrDefault())
	}
	if k, v := ok.Traffic.Testers.HeaderPair(); k != "x-canary" || v != "always" {
		t.Errorf("header: %q %q", k, v)
	}
	cases := map[string]*Rollout{
		"l'ultimo gradino deve essere 100": {Strategy: StrategyTraffic, Traffic: &TrafficRollout{Steps: []int{10, 50}}},
		"crescenti":                        {Strategy: StrategyTraffic, Traffic: &TrafficRollout{Steps: []int{50, 10, 100}}},
		"obbligatorio con strategy":        {Strategy: StrategyTraffic},
		"testers.header":                   {Strategy: StrategyTraffic, Traffic: &TrafficRollout{Steps: []int{100}, Testers: &Testers{Header: "senza valore"}}},
		"approvalsBetween: ondata \"50\"":  {Strategy: StrategyTraffic, Traffic: &TrafficRollout{Steps: []int{100}}, ApprovalsBetween: []string{"50"}},
		"richiede strategy: traffic":       {Waves: []Wave{{Name: "a", Select: "x=y"}}, Traffic: &TrafficRollout{Steps: []int{100}}},
		"strategy \"blu\"":                 {Strategy: "blu"},
	}
	for want, r := range cases {
		if err := r.Validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("atteso %q, ricevuto %v", want, err)
		}
	}
}

func TestMetricGateConfronto(t *testing.T) {
	ratio := 1.5
	h := &Health{Metrics: []MetricGate{{Name: "errori", Query: `rate(x{track="{track}"}[5m])`, Compare: CompareStable, MaxRatio: &ratio, SamplesQuery: "sum(y)", MinSamples: 100}}}
	if err := h.validate(); err != nil {
		t.Fatal(err)
	}
	h.Metrics[0].Query = "rate(x[5m])"
	if err := h.validate(); err == nil || !strings.Contains(err.Error(), "{track}") {
		t.Errorf("query senza {track}: %v", err)
	}
	h = &Health{Metrics: []MetricGate{{Name: "e", Query: "q", Max: &ratio, MinSamples: 10}}}
	if err := h.validate(); err == nil || !strings.Contains(err.Error(), "samplesQuery") {
		t.Errorf("minSamples senza query: %v", err)
	}
}

func TestTrafficConfig(t *testing.T) {
	c := &TrafficConfig{Pool: &PoolConfig{Provider: "haproxy", DrainTimeout: "2m", MinHealthy: "50%", Settings: map[string]string{"backend": "shop"}}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	drain, restore, poll := c.Pool.Durations()
	if drain.Minutes() != 2 || restore != DefaultRestoreTimeout || poll != DefaultPollInterval {
		t.Errorf("durate: %s %s %s", drain, restore, poll)
	}
	bad := &TrafficConfig{Pool: &PoolConfig{MinHealthy: "120%", OnTimeout: "attendi", Settings: map[string]string{"Chiave": "x"}}}
	err := bad.Validate()
	for _, want := range []string{"provider obbligatorio", "min_healthy", "on_timeout", "chiave \"Chiave\""} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("manca %q: %v", want, err)
		}
	}
	d := Descriptor{Traffic: c}
	if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "solo nella configurazione di ambiente") {
		t.Errorf("traffic nel descrittore: %v", err)
	}
}

func TestMetricGateAnalisi(t *testing.T) {
	q := `rate(x{track="{track}"}[1m])`
	h := &Health{Analysis: &CanaryAnalysis{Pass: 90, Marginal: 60}, Metrics: []MetricGate{
		{Name: "errori", Query: q, Compare: CompareStable, Analysis: AnalysisMannWhitney, Provider: ProviderDatadog, Critical: true, Step: "10s"},
		{Name: "latenza", Query: q, Compare: CompareStable, Analysis: AnalysisMannWhitney, Direction: DirectionEither, Alpha: 0.01, Lookback: "5m"},
	}}
	if err := h.validate(); err != nil {
		t.Fatal(err)
	}
	if p, m := h.Analysis.Thresholds(); p != 90 || m != 60 {
		t.Errorf("soglie: %g %g", p, m)
	}
	if p, m := (*CanaryAnalysis)(nil).Thresholds(); p != DefaultAnalysisPass || m != DefaultAnalysisMarginal {
		t.Errorf("soglie di default: %g %g", p, m)
	}
	cases := map[string]MetricGate{
		"provider \"grafana\"":      {Name: "a", Query: "x", Max: new(float64), Provider: "grafana"},
		"richiede compare: stable":  {Name: "a", Query: "x", Max: new(float64), Analysis: AnalysisMannWhitney},
		"analysis \"bayes\"":        {Name: "a", Query: q, Compare: CompareStable, MaxRatio: new(float64), Analysis: "bayes"},
		"direction \"giu\"":         {Name: "a", Query: q, Compare: CompareStable, Analysis: AnalysisMannWhitney, Direction: "giu"},
		"alpha deve essere":         {Name: "a", Query: q, Compare: CompareStable, Analysis: AnalysisMannWhitney, Alpha: 0.9},
		"step \"1ms\" non valido":   {Name: "a", Query: q, Compare: CompareStable, Analysis: AnalysisMannWhitney, Step: "1ms"},
		"maxRatio o maxDelta (o an": {Name: "a", Query: q, Compare: CompareStable},
	}
	for want, m := range cases {
		if err := (&Health{Metrics: []MetricGate{m}}).validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("atteso %q, ricevuto %v", want, err)
		}
	}
	if err := (&Health{Analysis: &CanaryAnalysis{Pass: 50, Marginal: 80}}).validate(); err == nil || !strings.Contains(err.Error(), "marginal non oltre pass") {
		t.Errorf("soglie invertite: %v", err)
	}
}
