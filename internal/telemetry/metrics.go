// Package telemetry exposes manent's Prometheus metrics and OpenTelemetry tracing.
package telemetry

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/manent-deployer/manent/internal/cluster"
	"github.com/manent-deployer/manent/internal/service"
)

// BuildInfo identifies the running binary.
type BuildInfo struct {
	Version string
	Commit  string
	// WorkflowVersion is the application version of the DBOS workflows (app.version).
	WorkflowVersion string
}

// Recorder records the metrics produced by workers and API.
type Recorder interface {
	// DeploymentFinished records the end of a deploy run by this process.
	DeploymentFinished(application, environment, kind, outcome string, dryRun bool, d time.Duration)
	// StepAttempt records an attempt to run a step.
	StepAttempt(step, outcome string, d time.Duration)
	// HTTPRequest records an API request; route is the path template, not the URL.
	HTTPRequest(method, route string, status int, d time.Duration)
	// PassFinished records the outcome of a wave rollout pass.
	PassFinished(application, environment, region, az, outcome string)
}

// Noop is the Recorder without metrics.
type Noop struct{}

func (Noop) DeploymentFinished(string, string, string, string, bool, time.Duration) {}
func (Noop) StepAttempt(string, string, time.Duration)                              {}
func (Noop) HTTPRequest(string, string, int, time.Duration)                         {}
func (Noop) PassFinished(string, string, string, string, string)                    {}

// Metrics is the Prometheus Recorder, with its own registry.
type Metrics struct {
	reg         *prometheus.Registry
	deployments *prometheus.CounterVec
	durations   *prometheus.HistogramVec
	steps       *prometheus.HistogramVec
	http        *prometheus.HistogramVec
	passes      *prometheus.CounterVec
}

var _ Recorder = (*Metrics)(nil)

// NewMetrics creates the metrics, including the Go runtime and process ones.
func NewMetrics(b BuildInfo) *Metrics {
	m := &Metrics{
		reg: prometheus.NewRegistry(),
		deployments: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "manent_deployments_total",
			Help: "Deploys ended by this process, by outcome.",
		}, []string{"application", "environment", "kind", "outcome", "dry_run"}),
		durations: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "manent_deployment_duration_seconds",
			Help:    "Duration of the deploys from the request to the end (queue waits and approvals excluded).",
			Buckets: prometheus.ExponentialBuckets(5, 2, 12),
		}, []string{"application", "environment", "outcome"}),
		steps: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "manent_step_duration_seconds",
			Help:    "Duration of every attempt to run a step, by outcome.",
			Buckets: prometheus.ExponentialBuckets(0.1, 3, 10),
		}, []string{"step", "outcome"}),
		http: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "manent_http_request_duration_seconds",
			Help:    "Duration of the API requests.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "route", "code"}),
		passes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "manent_rollout_passes_total",
			Help: "Passes of wave rollouts ended by this process, by region, AZ and outcome.",
		}, []string{"application", "environment", "region", "az", "outcome"}),
	}
	info := prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "manent_build_info",
		Help:        "Version of manent and of the workflows.",
		ConstLabels: prometheus.Labels{"version": b.Version, "commit": b.Commit, "workflow_version": b.WorkflowVersion},
	})
	info.Set(1)
	m.reg.MustRegister(m.deployments, m.durations, m.steps, m.http, m.passes, info,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}

// DeploymentFinished implements Recorder.
func (m *Metrics) DeploymentFinished(application, environment, kind, outcome string, dryRun bool, d time.Duration) {
	m.deployments.WithLabelValues(application, environment, kind, outcome, strconv.FormatBool(dryRun)).Inc()
	if !dryRun && d > 0 {
		m.durations.WithLabelValues(application, environment, outcome).Observe(d.Seconds())
	}
}

// StepAttempt implements Recorder.
func (m *Metrics) StepAttempt(step, outcome string, d time.Duration) {
	m.steps.WithLabelValues(step, outcome).Observe(d.Seconds())
}

// HTTPRequest implements Recorder.
func (m *Metrics) HTTPRequest(method, route string, status int, d time.Duration) {
	m.http.WithLabelValues(method, route, strconv.Itoa(status)).Observe(d.Seconds())
}

// PassFinished implements Recorder.
func (m *Metrics) PassFinished(application, environment, region, az, outcome string) {
	m.passes.WithLabelValues(application, environment, region, az, outcome).Inc()
}

// Register adds a collector (e.g. StateCollector).
func (m *Metrics) Register(c prometheus.Collector) error { return m.reg.Register(c) }

// Handler serves the metrics in Prometheus format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{Registry: m.reg})
}

// WorkflowCount is the number of workflows of a kind in a state.
type WorkflowCount struct {
	Workflow string
	Status   string
	Queue    string
	Count    int64
}

// StateSource provides the shared cluster state, read from the database at every collection.
type StateSource interface {
	WorkflowCounts(ctx context.Context) ([]WorkflowCount, error)
	ExecutorList(ctx context.Context) ([]cluster.Executor, error)
}

// StateCollector exposes queues, workflows per state and executors. Values are global: every
// manent instance reports the same ones, so aggregate them with max, not sum.
type StateCollector struct {
	Source StateSource
	// StaleAfter is the heartbeat delay beyond which an executor is considered lost.
	StaleAfter time.Duration
	// TTL avoids querying the database at every scrape (default 15s).
	TTL time.Duration

	mu      sync.Mutex
	at      time.Time
	metrics []prometheus.Metric
}

var (
	descWorkflows = prometheus.NewDesc("manent_workflows",
		"Workflows by kind, state and queue (global).", []string{"workflow", "status", "queue"}, nil)
	descHeartbeat = prometheus.NewDesc("manent_executor_heartbeat_timestamp_seconds",
		"Last heartbeat of every registered executor (global).", []string{"executor", "version"}, nil)
	descUp = prometheus.NewDesc("manent_executor_up",
		"1 if the executor sends heartbeats, 0 if it is lost or stopped (global).", []string{"executor", "version"}, nil)
	descTargets = prometheus.NewDesc("manent_targets",
		"Targets by state read by the reconciliation (in-sync, drifted, missing, standby, unknown; global).", []string{"application", "environment", "state"}, nil)
	descScrapeError = prometheus.NewDesc("manent_state_scrape_error",
		"1 if the last read of the state from the database failed.", nil, nil)
)

// Describe implements prometheus.Collector.
func (c *StateCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- descWorkflows
	ch <- descHeartbeat
	ch <- descUp
	ch <- descTargets
	ch <- descScrapeError
}

// TargetStateSource is implemented by a StateSource that also provides the target states
// recorded by the reconciliation.
type TargetStateSource interface {
	TargetStates(ctx context.Context) ([]cluster.TargetsRecord, error)
}

// Collect implements prometheus.Collector.
func (c *StateCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ttl := c.TTL
	if ttl <= 0 {
		ttl = 15 * time.Second
	}
	if c.metrics == nil || time.Since(c.at) >= ttl {
		c.metrics, c.at = c.read(), time.Now()
	}
	for _, m := range c.metrics {
		ch <- m
	}
}

func (c *StateCollector) read() []prometheus.Metric {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out []prometheus.Metric
	failed := 0.0
	counts, err := c.Source.WorkflowCounts(ctx)
	if err != nil {
		failed = 1
	}
	for _, w := range counts {
		out = append(out, prometheus.MustNewConstMetric(descWorkflows, prometheus.GaugeValue, float64(w.Count), w.Workflow, w.Status, w.Queue))
	}
	execs, err := c.Source.ExecutorList(ctx)
	if err != nil {
		failed = 1
	}
	now := time.Now()
	for _, e := range execs {
		up := 1.0
		if e.Stale(now, c.StaleAfter) {
			up = 0
		}
		out = append(out,
			prometheus.MustNewConstMetric(descHeartbeat, prometheus.GaugeValue, float64(e.HeartbeatAt.Unix()), e.ID, e.Version),
			prometheus.MustNewConstMetric(descUp, prometheus.GaugeValue, up, e.ID, e.Version))
	}
	if ts, ok := c.Source.(TargetStateSource); ok {
		// Without a target store (or with the reconciliation never run) there are no series.
		if recs, err := ts.TargetStates(ctx); err == nil {
			out = append(out, targetMetrics(recs)...)
		}
	}
	return append(out, prometheus.MustNewConstMetric(descScrapeError, prometheus.GaugeValue, failed))
}

// targetMetrics counts the targets per application, environment and state, summing the scopes.
func targetMetrics(recs []cluster.TargetsRecord) []prometheus.Metric {
	type key struct{ app, env, state string }
	counts := map[key]int{}
	for _, r := range recs {
		var st service.TargetsState
		if json.Unmarshal([]byte(r.Data), &st) != nil {
			continue
		}
		for _, t := range st.Targets {
			counts[key{r.Application, r.Environment, t.State}]++
		}
	}
	var out []prometheus.Metric
	for k, n := range counts {
		out = append(out, prometheus.MustNewConstMetric(descTargets, prometheus.GaugeValue, float64(n), k.app, k.env, k.state))
	}
	return out
}
