package rollout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/joeg-ita/manent/internal/descriptor"
)

// GateInput descrive i target da controllare dopo uno stadio.
type GateInput struct {
	Application string
	Environment string
	Stage       string
	Passes      []Pass
	Health      descriptor.Health
}

// CheckResult è l'esito di un singolo controllo.
type CheckResult struct {
	Kind   string   `json:"kind"`
	Name   string   `json:"name"`
	Region string   `json:"region,omitempty"`
	AZ     string   `json:"az,omitempty"`
	Host   string   `json:"host,omitempty"`
	OK     bool     `json:"ok"`
	Value  *float64 `json:"value,omitempty"`
	Detail string   `json:"detail,omitempty"`
}

// GateResult è l'esito di una lettura dell'health gate: verde solo se tutti i controlli lo sono.
type GateResult struct {
	OK     bool          `json:"ok"`
	Checks []CheckResult `json:"checks"`
}

// Failed restituisce i controlli rossi, per i messaggi di errore.
func (g GateResult) Failed() string {
	var parts []string
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

// Merge unisce le letture parziali di un gate (es. sonde eseguite nelle region e metriche
// lette dall'orchestratore): verde solo se lo sono tutte.
func Merge(rs ...GateResult) GateResult {
	res := GateResult{OK: true}
	for _, r := range rs {
		res.OK = res.OK && r.OK
		res.Checks = append(res.Checks, r.Checks...)
	}
	return res
}

// Checker esegue l'health gate. Un'implementazione per un altro sistema di metriche (es.
// Datadog) si registra in app.NewHealthChecker.
type Checker interface {
	// Check esegue una lettura. Gli errori dei sistemi interrogati sono controlli rossi, non
	// errori della funzione: un gate che non può verificare non è verde.
	Check(ctx context.Context, in GateInput) GateResult
	// Supports verifica che i controlli richiesti siano eseguibili (es. Prometheus configurato).
	Supports(h *descriptor.Health) error
}

// Tipi di controllo.
const (
	KindProbe  = "probe"
	KindMetric = "metric"
	KindAlert  = "alert"
)

// HTTPChecker esegue sonde HTTP, query Prometheus e la lettura degli allarmi di Alertmanager.
type HTTPChecker struct {
	Client *http.Client
	// PrometheusURL è l'indirizzo dell'API di Prometheus (es. http://prometheus:9090).
	PrometheusURL string
	// AlertmanagerURL è l'indirizzo dell'API di Alertmanager (es. http://alertmanager:9093).
	AlertmanagerURL string
}

var _ Checker = (*HTTPChecker)(nil)

// Supports implementa Checker.
func (c *HTTPChecker) Supports(h *descriptor.Health) error {
	if h == nil {
		return nil
	}
	var errs []error
	if len(h.Metrics) > 0 && c.PrometheusURL == "" {
		errs = append(errs, errors.New("rollout.health.metrics richiede rollout.prometheus_url nella configurazione di manent"))
	}
	if h.Alerts != nil && c.AlertmanagerURL == "" {
		errs = append(errs, errors.New("rollout.health.alerts richiede rollout.alertmanager_url nella configurazione di manent"))
	}
	return errors.Join(errs...)
}

// Check implementa Checker.
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
	res.OK = true
	for _, ch := range res.Checks {
		res.OK = res.OK && ch.OK
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
		r.Detail = fmt.Sprintf("stato %d, atteso %d", resp.StatusCode, want)
	}
	return r
}

// metric valuta una query PromQL per il passo. Nessun dato è un controllo rosso: un gate che
// non vede traffico non può dire che il rilascio è sano (per accettarlo, "or vector(0)").
func (c *HTTPChecker) metric(ctx context.Context, m descriptor.MetricGate, in GateInput, p Pass) CheckResult {
	r := CheckResult{Kind: KindMetric, Name: m.Name, Region: p.Region, AZ: p.AZ}
	q := strings.NewReplacer("{application}", in.Application, "{environment}", in.Environment, "{region}", p.Region, "{az}", p.AZ).Replace(m.Query)
	v, err := c.query(ctx, q)
	if err != nil {
		r.Detail = err.Error()
		return r
	}
	r.Value = &v
	r.OK = (m.Max == nil || v <= *m.Max) && (m.Min == nil || v >= *m.Min)
	if !r.OK {
		r.Detail = fmt.Sprintf("valore %g fuori soglia", v)
	}
	return r
}

// query esegue una query istantanea e restituisce il primo valore.
func (c *HTTPChecker) query(ctx context.Context, q string) (float64, error) {
	u := strings.TrimRight(c.PrometheusURL, "/") + "/api/v1/query?" + url.Values{"query": {q}}.Encode()
	var out struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string          `json:"resultType"`
			Result     json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, u, &out); err != nil {
		return 0, fmt.Errorf("prometheus: %w", err)
	}
	if out.Status != "success" {
		return 0, fmt.Errorf("prometheus: %s", out.Error)
	}
	var sample []any
	switch out.Data.ResultType {
	case "scalar":
		if err := json.Unmarshal(out.Data.Result, &sample); err != nil {
			return 0, err
		}
	case "vector":
		var vec []struct {
			Value []any `json:"value"`
		}
		if err := json.Unmarshal(out.Data.Result, &vec); err != nil {
			return 0, err
		}
		if len(vec) == 0 {
			return 0, errors.New("nessun dato")
		}
		sample = vec[0].Value
	default:
		return 0, fmt.Errorf("risultato %q non supportato (serve uno scalare o un vettore)", out.Data.ResultType)
	}
	if len(sample) != 2 {
		return 0, errors.New("risultato non valido")
	}
	s, _ := sample[1].(string)
	return strconv.ParseFloat(s, 64)
}

// alerts restituisce un controllo rosso per ogni allarme attivo che blocca un passo dello stadio.
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
				out = append(out, CheckResult{Kind: KindAlert, Name: al.Labels["alertname"], Region: p.Region, AZ: p.AZ, Detail: "allarme attivo"})
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
		return fmt.Errorf("stato %d", resp.StatusCode)
	}
	return json.Unmarshal(data, out)
}
