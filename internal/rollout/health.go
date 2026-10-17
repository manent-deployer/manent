package rollout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/joeg-ita/manent/internal/descriptor"
)

// GateInput describes the targets to check after a stage.
type GateInput struct {
	Application string
	Environment string
	Stage       string
	Passes      []Pass
	Health      descriptor.Health
	// Track and StableTrack are the slots of the candidate and stable versions in percentage
	// rollouts ({track} in queries); Version is the candidate version.
	Track       string `json:",omitempty"`
	StableTrack string `json:",omitempty"`
	Version     string `json:",omitempty"`
	// Elapsed is the time spent in the stage's observation so far (the window of the series
	// without lookback); Final marks the read at the end of the bake, when an inconclusive
	// metric is red.
	Elapsed time.Duration `json:",omitempty"`
	Final   bool          `json:",omitempty"`
}

// CheckResult is the outcome of a single check.
type CheckResult struct {
	Kind   string   `json:"kind"`
	Name   string   `json:"name"`
	Region string   `json:"region,omitempty"`
	AZ     string   `json:"az,omitempty"`
	Host   string   `json:"host,omitempty"`
	OK     bool     `json:"ok"`
	Value  *float64 `json:"value,omitempty"`
	Detail string   `json:"detail,omitempty"`
	// Baseline is the stable version's value, PValue the p-value of the statistical test.
	Baseline *float64 `json:"baseline,omitempty"`
	PValue   *float64 `json:"p_value,omitempty"`
	// Inconclusive marks a metric without enough data yet: green during the bake, not scored.
	Inconclusive bool `json:"inconclusive,omitempty"`
}

// GateResult is the outcome of a health gate read: green only if all checks are.
type GateResult struct {
	OK     bool          `json:"ok"`
	Checks []CheckResult `json:"checks"`
	// Score is the canary analysis score (0-100), with rollout.health.analysis.
	Score *float64 `json:"score,omitempty"`
}

// Failed returns the red checks, for error messages.
func (g GateResult) Failed() string {
	var parts []string
	if g.Score != nil {
		parts = append(parts, fmt.Sprintf("score %.0f/100", *g.Score))
	}
	for _, c := range g.Checks {
		if !c.OK {
			p := c.Kind + " " + c.Name
			if loc := strings.Trim(c.Region+"/"+c.AZ+"/"+c.Host, "/"); loc != "" {
				p += " (" + loc + ")"
			}
			if c.Detail != "" {
				p += ": " + c.Detail
			}
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, "; ")
}

// Merge merges the partial reads of a gate (e.g. probes run in the regions and metrics read by
// the orchestrator): green only if all are.
func Merge(rs ...GateResult) GateResult {
	res := GateResult{OK: true}
	for _, r := range rs {
		res.OK = res.OK && r.OK
		res.Checks = append(res.Checks, r.Checks...)
		if r.Score != nil {
			res.Score = r.Score
		}
	}
	return res
}

// Checker runs the health gate. An implementation for another metrics system (e.g. Datadog)
// is registered in app.NewHealthChecker.
type Checker interface {
	// Check runs one read. Errors of the queried systems are red checks, not errors of the
	// function: a gate that cannot verify is not green.
	Check(ctx context.Context, in GateInput) GateResult
	// Supports checks that the required checks can run (e.g. Prometheus configured).
	Supports(h *descriptor.Health) error
}

// Check types.
const (
	KindProbe  = "probe"
	KindMetric = "metric"
	KindAlert  = "alert"
)

// HTTPChecker runs HTTP probes, metric queries on the configured sources and the
// Alertmanager alerts read.
type HTTPChecker struct {
	Client *http.Client
	// PrometheusURL is the address of the Prometheus API (e.g. http://prometheus:9090): the
	// prometheus source when Sources does not have one.
	PrometheusURL string
	// Sources are the metrics systems by provider name (descriptor.MetricProviders).
	Sources map[string]MetricSource
	// AlertmanagerURL is the address of the Alertmanager API (e.g. http://alertmanager:9093).
	AlertmanagerURL string
	// Now returns the current time; replaceable in tests.
	Now func() time.Time
}

// source returns the metrics system of a provider, nil if not configured.
func (c *HTTPChecker) source(provider string) MetricSource {
	if s := c.Sources[provider]; s != nil {
		return s
	}
	if provider == descriptor.ProviderPrometheus && c.PrometheusURL != "" {
		return &PrometheusSource{URL: c.PrometheusURL, Client: c.Client}
	}
	return nil
}

func (c *HTTPChecker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

var _ Checker = (*HTTPChecker)(nil)

// Supports implements Checker.
func (c *HTTPChecker) Supports(h *descriptor.Health) error {
	if h == nil {
		return nil
	}
	var errs []error
	for _, m := range h.Metrics {
		if p := m.ProviderOrDefault(); c.source(p) == nil {
			setting := "rollout." + p
			if p == descriptor.ProviderPrometheus {
				setting = "rollout.prometheus_url"
			}
			errs = append(errs, fmt.Errorf("rollout.health.metrics %q requires %s in the manent configuration", m.Name, setting))
		}
	}
	if h.Alerts != nil && c.AlertmanagerURL == "" {
		errs = append(errs, errors.New("rollout.health.alerts requires rollout.alertmanager_url in the manent configuration"))
	}
	return errors.Join(errs...)
}

// Check implements Checker.
func (c *HTTPChecker) Check(ctx context.Context, in GateInput) GateResult {
	var res GateResult
	for _, p := range in.Passes {
		for _, pr := range in.Health.Probes {
			for _, t := range p.Targets {
				res.Checks = append(res.Checks, c.probe(ctx, pr, p, t.Host))
			}
		}
		for _, m := range in.Health.Metrics {
			res.Checks = append(res.Checks, c.metric(ctx, m, in, p))
		}
	}
	if in.Health.Alerts != nil {
		res.Checks = append(res.Checks, c.alerts(ctx, *in.Health.Alerts, in)...)
	}
	if in.Health.Analysis != nil && len(in.Health.Metrics) > 0 {
		return score(res, in)
	}
	res.OK = true
	for _, ch := range res.Checks {
		res.OK = res.OK && ch.OK
	}
	return res
}

// score judges the read as a canary analysis: probes and alerts must be green, a failed
// critical metric is red, the others make a weighted score (inconclusive ones excluded)
// that must reach marginal during the bake and pass at its end.
func score(res GateResult, in GateInput) GateResult {
	weights := map[string]descriptor.MetricGate{}
	for _, m := range in.Health.Metrics {
		weights[m.Name] = m
	}
	res.OK = true
	var total, good float64
	for _, ch := range res.Checks {
		if ch.Kind != KindMetric {
			res.OK = res.OK && ch.OK
			continue
		}
		m := weights[ch.Name]
		if ch.Inconclusive {
			if in.Final {
				res.OK = false
			}
			continue
		}
		if !ch.OK && m.Critical {
			res.OK = false
		}
		total += m.WeightOrDefault()
		if ch.OK {
			good += m.WeightOrDefault()
		}
	}
	pass, marginal := in.Health.Analysis.Thresholds()
	if total > 0 {
		sc := math.Round(good/total*1000) / 10
		res.Score = &sc
		threshold := marginal
		if in.Final {
			threshold = pass
		}
		res.OK = res.OK && sc >= threshold
	} else if in.Final {
		// No scored metric at the end of the bake: nothing says the release is healthy.
		res.OK = false
	}
	return res
}

func (c *HTTPChecker) probe(ctx context.Context, pr descriptor.Probe, p Pass, host string) CheckResult {
	u := strings.ReplaceAll(pr.URL, "{host}", host)
	r := CheckResult{Kind: KindProbe, Name: u, Region: p.Region, AZ: p.AZ, Host: host}
	want := pr.ExpectStatus
	if want == 0 {
		want = http.StatusOK
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		r.Detail = err.Error()
		return r
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		r.Detail = err.Error()
		return r
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	r.OK = resp.StatusCode == want
	if !r.OK {
		r.Detail = fmt.Sprintf("status %d, expected %d", resp.StatusCode, want)
	}
	return r
}

// metric evaluates a metric for the pass. No data is a red check: a gate that sees no traffic
// cannot say the release is healthy (with Prometheus, "or vector(0)" accepts it).
func (c *HTTPChecker) metric(ctx context.Context, m descriptor.MetricGate, in GateInput, p Pass) CheckResult {
	r := CheckResult{Kind: KindMetric, Name: m.Name, Region: p.Region, AZ: p.AZ}
	src := c.source(m.ProviderOrDefault())
	if src == nil {
		r.Detail = "source " + m.ProviderOrDefault() + " not configured"
		return r
	}
	expand := func(q, track string) string {
		return strings.NewReplacer("{application}", in.Application, "{environment}", in.Environment, "{region}", p.Region, "{az}", p.AZ,
			"{track}", track, "{version}", in.Version).Replace(q)
	}
	now := c.now()
	if m.SamplesQuery != "" && m.MinSamples > 0 {
		n, err := src.Value(ctx, expand(m.SamplesQuery, in.Track), now)
		if err != nil {
			r.Detail = "samples: " + err.Error()
			return r
		}
		if n < m.MinSamples {
			r.Detail = fmt.Sprintf("insufficient samples: %g, minimum %g", n, m.MinSamples)
			return r
		}
	}
	if m.Analysis == descriptor.AnalysisMannWhitney {
		return c.mannWhitney(ctx, src, m, in, r, expand, now)
	}
	v, err := src.Value(ctx, expand(m.Query, in.Track), now)
	if err != nil {
		r.Detail = err.Error()
		return r
	}
	r.Value = &v
	if m.Compare != descriptor.CompareStable {
		r.OK = (m.Max == nil || v <= *m.Max) && (m.Min == nil || v >= *m.Min)
		if !r.OK {
			r.Detail = fmt.Sprintf("value %g out of threshold", v)
		}
		return r
	}
	if in.StableTrack == "" {
		// First release: there is no stable version to compare with.
		r.OK, r.Detail = true, "nessuna versione stabile da confrontare"
		return r
	}
	base, err := src.Value(ctx, expand(m.Query, in.StableTrack), now)
	if err != nil {
		r.Detail = "stable version: " + err.Error()
		return r
	}
	r.Baseline = &base
	r.OK = true
	if m.MaxDelta != nil && v-base > *m.MaxDelta {
		r.OK, r.Detail = false, fmt.Sprintf("candidate %g, stable %g: difference over %g", v, base, *m.MaxDelta)
	}
	if m.MaxRatio != nil {
		switch {
		case base > 0 && v/base > *m.MaxRatio:
			r.OK, r.Detail = false, fmt.Sprintf("candidate %g, stable %g: ratio %.2f over %g", v, base, v/base, *m.MaxRatio)
		case base == 0 && v > 0 && m.MaxDelta == nil:
			r.OK, r.Detail = false, fmt.Sprintf("candidate %g, stable 0", v)
		}
	}
	return r
}

// mannWhitney compares the series of candidate and stable with the Mann-Whitney U test, in the
// style of Kayenta: the metric fails when the difference is significant (p < alpha) and goes
// in the direction that counts as worse beyond maxRatio (increase) or minRatio (decrease),
// both 1 by default. Values and ratio are the means of the series.
func (c *HTTPChecker) mannWhitney(ctx context.Context, src MetricSource, m descriptor.MetricGate, in GateInput, r CheckResult,
	expand func(string, string) string, now time.Time) CheckResult {
	if in.StableTrack == "" {
		r.OK, r.Detail = true, "nessuna versione stabile da confrontare"
		return r
	}
	step := m.StepDuration()
	window := m.LookbackDuration()
	if window == 0 {
		window = in.Elapsed
	}
	window = max(window, step)
	start := now.Add(-window)
	cand, err := src.Series(ctx, expand(m.Query, in.Track), start, now, step)
	if err != nil {
		r.Detail = err.Error()
		return r
	}
	stable, err := src.Series(ctx, expand(m.Query, in.StableTrack), start, now, step)
	if err != nil {
		r.Detail = "stable version: " + err.Error()
		return r
	}
	minPoints := m.MinPoints
	if minPoints == 0 {
		minPoints = descriptor.DefaultMinPoints
	}
	if len(cand) < minPoints || len(stable) < minPoints {
		r.Inconclusive, r.OK = true, !in.Final
		r.Detail = fmt.Sprintf("insufficient data: %d and %d points, minimum %d", len(cand), len(stable), minPoints)
		return r
	}
	cm, sm := mean(cand), mean(stable)
	r.Value, r.Baseline = &cm, &sm
	_, pv := MannWhitney(cand, stable)
	r.PValue = &pv
	alpha := m.Alpha
	if alpha == 0 {
		alpha = descriptor.DefaultAlpha
	}
	maxR, minR := 1.0, 1.0
	if m.MaxRatio != nil {
		maxR = *m.MaxRatio
	}
	if m.MinRatio != nil {
		minR = *m.MinRatio
	}
	rt := ratio(cm, sm)
	dir := m.Direction
	if dir == "" {
		dir = descriptor.DirectionIncrease
	}
	worse := (dir != descriptor.DirectionDecrease && rt > maxR) || (dir != descriptor.DirectionIncrease && rt < minR)
	r.OK = pv >= alpha || !worse
	r.Detail = fmt.Sprintf("candidate %.4g, stable %.4g (means of %d and %d points): ratio %.3g, p=%.3g", cm, sm, len(cand), len(stable), rt, pv)
	if !r.OK {
		r.Detail += fmt.Sprintf(" < %g", alpha)
	}
	return r
}

// alerts returns a red check for every active alert that blocks a pass of the stage.
func (c *HTTPChecker) alerts(ctx context.Context, a descriptor.AlertGate, in GateInput) []CheckResult {
	u := strings.TrimRight(c.AlertmanagerURL, "/") + "/api/v2/alerts?active=true&silenced=false&inhibited=false"
	var list []struct {
		Labels map[string]string `json:"labels"`
	}
	if err := c.getJSON(ctx, u, &list); err != nil {
		return []CheckResult{{Kind: KindAlert, Name: "alertmanager", Detail: err.Error()}}
	}
	var out []CheckResult
	for _, al := range list {
		if !matchAll(al.Labels, a.BlockOn) {
			continue
		}
		app := al.Labels["application"]
		if app == "" {
			app = al.Labels["app"]
		}
		if app != "" && app != in.Application {
			continue
		}
		for _, p := range in.Passes {
			if (al.Labels["region"] == "" || al.Labels["region"] == p.Region) && (al.Labels["az"] == "" || al.Labels["az"] == p.AZ) {
				out = append(out, CheckResult{Kind: KindAlert, Name: al.Labels["alertname"], Region: p.Region, AZ: p.AZ, Detail: "alert firing"})
				break
			}
		}
	}
	if len(out) == 0 {
		out = append(out, CheckResult{Kind: KindAlert, Name: "alertmanager", OK: true})
	}
	return out
}

func matchAll(l map[string]string, conds []string) bool {
	for _, c := range conds {
		k, v, _ := strings.Cut(c, "=")
		if l[strings.TrimSpace(k)] != strings.TrimSpace(v) {
			return false
		}
	}
	return true
}

func (c *HTTPChecker) getJSON(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 && !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return json.Unmarshal(data, out)
}
