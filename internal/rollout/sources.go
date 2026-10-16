package rollout

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// MetricSource reads the metrics of a monitoring system. Queries are already expanded
// ({application}, {track}, ...) and written in the system's own language.
type MetricSource interface {
	// Value returns the latest value of the query at time at (ErrNoData without points).
	Value(ctx context.Context, query string, at time.Time) (float64, error)
	// Series returns the values of the query between start and end, about one every step.
	Series(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]float64, error)
}

// ErrNoData means a query without points: a gate that sees no data is not green.
var ErrNoData = errors.New("nessun dato")

// SecretFunc resolves a secret reference at use time.
type SecretFunc func(ctx context.Context, ref string) (string, error)

// valueWindow is the window read for an instant value by systems without instant queries.
const valueWindow = 5 * time.Minute

// last returns the last value of a series.
func last(v []float64) (float64, error) {
	if len(v) == 0 {
		return 0, ErrNoData
	}
	return v[len(v)-1], nil
}

// httpJSON sends a request with an optional JSON body and decodes a JSON response; a status
// outside 2xx is an error with an excerpt of the response.
func httpJSON(ctx context.Context, c *http.Client, method, u string, body any, header http.Header, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("stato %d: %.200s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return json.Unmarshal(data, out)
}

// --- Prometheus ---

// PrometheusSource reads PromQL queries from the Prometheus HTTP API.
type PrometheusSource struct {
	URL    string
	Client *http.Client
}

type promResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	} `json:"data"`
}

func (p *PrometheusSource) get(ctx context.Context, path string, q url.Values) (promResponse, error) {
	var out promResponse
	if err := httpJSON(ctx, p.Client, http.MethodGet, strings.TrimRight(p.URL, "/")+path+"?"+q.Encode(), nil, nil, &out); err != nil {
		return out, fmt.Errorf("prometheus: %w", err)
	}
	if out.Status != "success" {
		return out, fmt.Errorf("prometheus: %s", out.Error)
	}
	return out, nil
}

// Value implements MetricSource with an instant query: the first sample of a vector, or a scalar.
func (p *PrometheusSource) Value(ctx context.Context, query string, at time.Time) (float64, error) {
	out, err := p.get(ctx, "/api/v1/query", url.Values{"query": {query}, "time": {promTime(at)}})
	if err != nil {
		return 0, err
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
			return 0, ErrNoData
		}
		sample = vec[0].Value
	default:
		return 0, fmt.Errorf("prometheus: risultato %q non supportato (serve uno scalare o un vettore)", out.Data.ResultType)
	}
	return promSample(sample)
}

// Series implements MetricSource with a range query: the values of the first series.
func (p *PrometheusSource) Series(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]float64, error) {
	out, err := p.get(ctx, "/api/v1/query_range", url.Values{"query": {query}, "start": {promTime(start)}, "end": {promTime(end)},
		"step": {strconv.FormatFloat(step.Seconds(), 'f', -1, 64)}})
	if err != nil {
		return nil, err
	}
	if out.Data.ResultType != "matrix" {
		return nil, fmt.Errorf("prometheus: risultato %q non supportato in un intervallo", out.Data.ResultType)
	}
	var m []struct {
		Values [][]any `json:"values"`
	}
	if err := json.Unmarshal(out.Data.Result, &m); err != nil {
		return nil, err
	}
	if len(m) == 0 {
		return nil, nil
	}
	var vals []float64
	for _, s := range m[0].Values {
		if v, err := promSample(s); err == nil && !math.IsNaN(v) {
			vals = append(vals, v)
		}
	}
	return vals, nil
}

func promTime(t time.Time) string {
	return strconv.FormatFloat(float64(t.UnixMilli())/1000, 'f', 3, 64)
}

func promSample(s []any) (float64, error) {
	if len(s) != 2 {
		return 0, errors.New("prometheus: campione non valido")
	}
	str, _ := s[1].(string)
	return strconv.ParseFloat(str, 64)
}

// --- Datadog ---

// DatadogSource reads metric queries from the Datadog API (v1 query endpoint) with the API
// and application keys, read from the secrets at every call.
type DatadogSource struct {
	// URL is the API site, e.g. https://api.datadoghq.eu (default https://api.datadoghq.com).
	URL       string
	APIKeyRef string
	AppKeyRef string
	Secret    SecretFunc
	Client    *http.Client
}

// Value implements MetricSource: the last point of the series in the last minutes.
func (d *DatadogSource) Value(ctx context.Context, query string, at time.Time) (float64, error) {
	v, err := d.Series(ctx, query, at.Add(-valueWindow), at, 0)
	if err != nil {
		return 0, err
	}
	return last(v)
}

// Series implements MetricSource: the points of the first series (Datadog chooses the
// resolution from the window; ".rollup(avg, <s>)" in the query sets it).
func (d *DatadogSource) Series(ctx context.Context, query string, start, end time.Time, _ time.Duration) ([]float64, error) {
	api, err := d.Secret(ctx, d.APIKeyRef)
	if err != nil {
		return nil, fmt.Errorf("datadog: api key: %w", err)
	}
	app, err := d.Secret(ctx, d.AppKeyRef)
	if err != nil {
		return nil, fmt.Errorf("datadog: application key: %w", err)
	}
	base := strings.TrimRight(d.URL, "/")
	if base == "" {
		base = "https://api.datadoghq.com"
	}
	q := url.Values{"query": {query}, "from": {strconv.FormatInt(start.Unix(), 10)}, "to": {strconv.FormatInt(end.Unix(), 10)}}
	h := http.Header{"Dd-Api-Key": {strings.TrimSpace(api)}, "Dd-Application-Key": {strings.TrimSpace(app)}}
	var out struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Series []struct {
			Pointlist [][]*float64 `json:"pointlist"`
		} `json:"series"`
	}
	if err := httpJSON(ctx, d.Client, http.MethodGet, base+"/api/v1/query?"+q.Encode(), nil, h, &out); err != nil {
		return nil, fmt.Errorf("datadog: %w", err)
	}
	if out.Status == "error" {
		return nil, fmt.Errorf("datadog: %s", out.Error)
	}
	if len(out.Series) == 0 {
		return nil, nil
	}
	var vals []float64
	for _, p := range out.Series[0].Pointlist {
		if len(p) == 2 && p[1] != nil {
			vals = append(vals, *p[1])
		}
	}
	return vals, nil
}

// --- New Relic ---

// NewRelicSource reads NRQL queries through NerdGraph with a user API key. The query must
// not contain SINCE, UNTIL or TIMESERIES: manent adds them for the window it reads.
type NewRelicSource struct {
	// URL is the NerdGraph endpoint (default https://api.newrelic.com/graphql; EU:
	// https://api.eu.newrelic.com/graphql).
	URL       string
	AccountID int64
	APIKeyRef string
	Secret    SecretFunc
	Client    *http.Client
}

// Value implements MetricSource: the aggregate over the last minutes.
func (n *NewRelicSource) Value(ctx context.Context, query string, at time.Time) (float64, error) {
	rows, err := n.nrql(ctx, fmt.Sprintf("%s SINCE %d UNTIL %d", query, at.Add(-valueWindow).UnixMilli(), at.UnixMilli()))
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, ErrNoData
	}
	v, ok := numericField(rows[0])
	if !ok {
		return 0, ErrNoData
	}
	return v, nil
}

// Series implements MetricSource with TIMESERIES buckets of step.
func (n *NewRelicSource) Series(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]float64, error) {
	secs := max(int(step.Seconds()), 1)
	rows, err := n.nrql(ctx, fmt.Sprintf("%s SINCE %d UNTIL %d TIMESERIES %d seconds", query, start.UnixMilli(), end.UnixMilli(), secs))
	if err != nil {
		return nil, err
	}
	var vals []float64
	for _, r := range rows {
		if v, ok := numericField(r); ok {
			vals = append(vals, v)
		}
	}
	return vals, nil
}

func (n *NewRelicSource) nrql(ctx context.Context, nrql string) ([]map[string]any, error) {
	key, err := n.Secret(ctx, n.APIKeyRef)
	if err != nil {
		return nil, fmt.Errorf("newrelic: api key: %w", err)
	}
	u := n.URL
	if u == "" {
		u = "https://api.newrelic.com/graphql"
	}
	body := map[string]any{
		"query":     "query($id: Int!, $nrql: Nrql!) { actor { account(id: $id) { nrql(query: $nrql) { results } } } }",
		"variables": map[string]any{"id": n.AccountID, "nrql": nrql},
	}
	var out struct {
		Data struct {
			Actor struct {
				Account struct {
					NRQL struct {
						Results []map[string]any `json:"results"`
					} `json:"nrql"`
				} `json:"account"`
			} `json:"actor"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := httpJSON(ctx, n.Client, http.MethodPost, u, body, http.Header{"Api-Key": {strings.TrimSpace(key)}}, &out); err != nil {
		return nil, fmt.Errorf("newrelic: %w", err)
	}
	if len(out.Errors) > 0 {
		return nil, fmt.Errorf("newrelic: %s", out.Errors[0].Message)
	}
	return out.Data.Actor.Account.NRQL.Results, nil
}

// numericField returns the aggregate of an NRQL row: the first numeric field other than the
// bucket bounds, in alphabetical order for determinism.
func numericField(row map[string]any) (float64, bool) {
	keys := make([]string, 0, len(row))
	for k := range row {
		if k != "beginTimeSeconds" && k != "endTimeSeconds" && k != "timestamp" {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	for _, k := range keys {
		if v, ok := row[k].(float64); ok {
			return v, true
		}
	}
	return 0, false
}

// --- Elastic ---

// ElasticSource reads ES|QL queries from Elasticsearch. The query receives the window as text
// placeholders {start}, {end} (RFC 3339) and {step} (e.g. "15 seconds"), and its last numeric
// column is the metric: one row for a value, one row per bucket, sorted, for a series.
type ElasticSource struct {
	URL string
	// APIKeyRef (Authorization: ApiKey) or Username with PasswordRef (basic).
	APIKeyRef   string
	Username    string
	PasswordRef string
	Secret      SecretFunc
	Client      *http.Client
}

// Value implements MetricSource: the last row over the last minutes.
func (e *ElasticSource) Value(ctx context.Context, query string, at time.Time) (float64, error) {
	v, err := e.Series(ctx, query, at.Add(-valueWindow), at, valueWindow)
	if err != nil {
		return 0, err
	}
	return last(v)
}

// Series implements MetricSource.
func (e *ElasticSource) Series(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]float64, error) {
	q := strings.NewReplacer("{start}", start.UTC().Format(time.RFC3339), "{end}", end.UTC().Format(time.RFC3339),
		"{step}", strconv.Itoa(max(int(step.Seconds()), 1))+" seconds").Replace(query)
	h := http.Header{}
	switch {
	case e.APIKeyRef != "":
		k, err := e.Secret(ctx, e.APIKeyRef)
		if err != nil {
			return nil, fmt.Errorf("elastic: api key: %w", err)
		}
		h.Set("Authorization", "ApiKey "+strings.TrimSpace(k))
	case e.Username != "":
		pw, err := e.Secret(ctx, e.PasswordRef)
		if err != nil {
			return nil, fmt.Errorf("elastic: password: %w", err)
		}
		req, _ := http.NewRequest(http.MethodGet, "http://x", nil)
		req.SetBasicAuth(e.Username, strings.TrimSpace(pw))
		h.Set("Authorization", req.Header.Get("Authorization"))
	}
	var out struct {
		Columns []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"columns"`
		Values [][]any `json:"values"`
	}
	if err := httpJSON(ctx, e.Client, http.MethodPost, strings.TrimRight(e.URL, "/")+"/_query?format=json", map[string]string{"query": q}, h, &out); err != nil {
		return nil, fmt.Errorf("elastic: %w", err)
	}
	col := -1
	for i, c := range out.Columns {
		switch c.Type {
		case "double", "long", "integer", "float", "half_float", "scaled_float", "unsigned_long":
			col = i
		}
	}
	if col < 0 {
		if len(out.Values) == 0 {
			return nil, nil
		}
		return nil, errors.New("elastic: la query non restituisce una colonna numerica")
	}
	var vals []float64
	for _, row := range out.Values {
		if col < len(row) {
			if v, ok := row[col].(float64); ok {
				vals = append(vals, v)
			}
		}
	}
	return vals, nil
}
