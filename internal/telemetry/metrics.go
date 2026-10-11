// Package telemetry espone le metriche Prometheus e il tracing OpenTelemetry di manent.
package telemetry

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/joeg-ita/manent/internal/cluster"
)

// BuildInfo identifica il binario in esecuzione.
type BuildInfo struct {
	Version string
	Commit  string
	// WorkflowVersion è la versione applicativa dei workflow DBOS (app.version).
	WorkflowVersion string
}

// Recorder registra le metriche prodotte da worker e API.
type Recorder interface {
	// DeploymentFinished registra la conclusione di un deploy eseguito da questo processo.
	DeploymentFinished(application, environment, kind, outcome string, dryRun bool, d time.Duration)
	// StepAttempt registra un tentativo di esecuzione di uno step.
	StepAttempt(step, outcome string, d time.Duration)
	// HTTPRequest registra una richiesta all'API; route è il modello del percorso, non l'URL.
	HTTPRequest(method, route string, status int, d time.Duration)
	// PassFinished registra l'esito di un passo di un rollout a ondate.
	PassFinished(application, environment, region, az, outcome string)
}

// Noop è il Recorder senza metriche.
type Noop struct{}

func (Noop) DeploymentFinished(string, string, string, string, bool, time.Duration) {}
func (Noop) StepAttempt(string, string, time.Duration)                              {}
func (Noop) HTTPRequest(string, string, int, time.Duration)                         {}
func (Noop) PassFinished(string, string, string, string, string)                    {}

// Metrics è il Recorder Prometheus, con un registry proprio.
type Metrics struct {
	reg         *prometheus.Registry
	deployments *prometheus.CounterVec
	durations   *prometheus.HistogramVec
	steps       *prometheus.HistogramVec
	http        *prometheus.HistogramVec
	passes      *prometheus.CounterVec
}

var _ Recorder = (*Metrics)(nil)

// NewMetrics crea le metriche, comprese quelle del runtime Go e del processo.
func NewMetrics(b BuildInfo) *Metrics {
	m := &Metrics{
		reg: prometheus.NewRegistry(),
		deployments: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "manent_deployments_total",
			Help: "Deploy conclusi da questo processo, per esito.",
		}, []string{"application", "environment", "kind", "outcome", "dry_run"}),
		durations: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "manent_deployment_duration_seconds",
			Help:    "Durata dei deploy dalla richiesta alla conclusione (attese in coda e approvazioni escluse).",
			Buckets: prometheus.ExponentialBuckets(5, 2, 12),
		}, []string{"application", "environment", "outcome"}),
		steps: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "manent_step_duration_seconds",
			Help:    "Durata di ogni tentativo di esecuzione di uno step, per esito.",
			Buckets: prometheus.ExponentialBuckets(0.1, 3, 10),
		}, []string{"step", "outcome"}),
		http: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "manent_http_request_duration_seconds",
			Help:    "Durata delle richieste all'API.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "route", "code"}),
		passes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "manent_rollout_passes_total",
			Help: "Passi dei rollout a ondate conclusi da questo processo, per region, AZ ed esito.",
		}, []string{"application", "environment", "region", "az", "outcome"}),
	}
	info := prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "manent_build_info",
		Help:        "Versione di manent e dei workflow.",
		ConstLabels: prometheus.Labels{"version": b.Version, "commit": b.Commit, "workflow_version": b.WorkflowVersion},
	})
	info.Set(1)
	m.reg.MustRegister(m.deployments, m.durations, m.steps, m.http, m.passes, info,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}

// DeploymentFinished implementa Recorder.
func (m *Metrics) DeploymentFinished(application, environment, kind, outcome string, dryRun bool, d time.Duration) {
	m.deployments.WithLabelValues(application, environment, kind, outcome, strconv.FormatBool(dryRun)).Inc()
	if !dryRun && d > 0 {
		m.durations.WithLabelValues(application, environment, outcome).Observe(d.Seconds())
	}
}

// StepAttempt implementa Recorder.
func (m *Metrics) StepAttempt(step, outcome string, d time.Duration) {
	m.steps.WithLabelValues(step, outcome).Observe(d.Seconds())
}

// HTTPRequest implementa Recorder.
func (m *Metrics) HTTPRequest(method, route string, status int, d time.Duration) {
	m.http.WithLabelValues(method, route, strconv.Itoa(status)).Observe(d.Seconds())
}

// PassFinished implementa Recorder.
func (m *Metrics) PassFinished(application, environment, region, az, outcome string) {
	m.passes.WithLabelValues(application, environment, region, az, outcome).Inc()
}

// Register aggiunge un collector (es. StateCollector).
func (m *Metrics) Register(c prometheus.Collector) error { return m.reg.Register(c) }

// Handler serve le metriche in formato Prometheus.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{Registry: m.reg})
}

// WorkflowCount è il numero di workflow di un tipo in uno stato.
type WorkflowCount struct {
	Workflow string
	Status   string
	Queue    string
	Count    int64
}

// StateSource fornisce lo stato condiviso del cluster, letto dal database a ogni raccolta.
type StateSource interface {
	WorkflowCounts(ctx context.Context) ([]WorkflowCount, error)
	ExecutorList(ctx context.Context) ([]cluster.Executor, error)
}

// StateCollector espone code, workflow per stato ed executor. I valori sono globali: ogni
// istanza di manent riporta gli stessi, quindi vanno aggregati con max, non con sum.
type StateCollector struct {
	Source StateSource
	// StaleAfter è il ritardo di heartbeat oltre il quale un executor è considerato perso.
	StaleAfter time.Duration
	// TTL evita di interrogare il database a ogni scrape (default 15s).
	TTL time.Duration

	mu      sync.Mutex
	at      time.Time
	metrics []prometheus.Metric
}

var (
	descWorkflows = prometheus.NewDesc("manent_workflows",
		"Workflow per tipo, stato e coda (globale).", []string{"workflow", "status", "queue"}, nil)
	descHeartbeat = prometheus.NewDesc("manent_executor_heartbeat_timestamp_seconds",
		"Ultimo heartbeat di ogni executor registrato (globale).", []string{"executor", "version"}, nil)
	descUp = prometheus.NewDesc("manent_executor_up",
		"1 se l'executor invia heartbeat, 0 se è perso o arrestato (globale).", []string{"executor", "version"}, nil)
	descScrapeError = prometheus.NewDesc("manent_state_scrape_error",
		"1 se l'ultima lettura dello stato dal database è fallita.", nil, nil)
)

// Describe implementa prometheus.Collector.
func (c *StateCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- descWorkflows
	ch <- descHeartbeat
	ch <- descUp
	ch <- descScrapeError
}

// Collect implementa prometheus.Collector.
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
	return append(out, prometheus.MustNewConstMetric(descScrapeError, prometheus.GaugeValue, failed))
}
