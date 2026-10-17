// Package azuremonitor reads the health gate metrics from Azure Monitor Logs: a KQL query on a
// Log Analytics workspace (also the one of a workspace-based Application Insights), through
// the Log Analytics query API. The query receives the window as timespan and the {step}
// placeholder (e.g. "60s") for bin(); its last numeric column is the metric, one row per point.
package azuremonitor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/joeg-ita/manent/internal/rollout"
	"github.com/joeg-ita/manent/plugins/internal/azure"
)

// Defaults of the Log Analytics query API.
const (
	DefaultEndpoint = "https://api.loganalytics.io"
	scope           = "https://api.loganalytics.io/.default"
)

// valueWindow is the window read for an instant value.
const valueWindow = 5 * time.Minute

// Source is a rollout.MetricSource on Log Analytics.
type Source struct {
	WorkspaceID string
	// Endpoint of the query API (default https://api.loganalytics.io).
	Endpoint string
	// Credential: service principal (TenantID, ClientID and the secret reference
	// ClientSecretRef, resolved at every token request) or, without it, the worker's identity.
	TenantID, ClientID, ClientSecretRef string
	// Authority and IMDS replace the Entra ID and metadata endpoints (sovereign clouds, tests).
	Authority, IMDS string
	Secret          rollout.SecretFunc
	Client          *http.Client

	once sync.Once
	auth *azure.Auth
}

var _ rollout.MetricSource = (*Source)(nil)

// Value implements rollout.MetricSource: the last row of the last minutes.
func (s *Source) Value(ctx context.Context, query string, at time.Time) (float64, error) {
	v, err := s.Series(ctx, query, at.Add(-valueWindow), at, valueWindow)
	if err != nil {
		return 0, err
	}
	if len(v) == 0 {
		return 0, rollout.ErrNoData
	}
	return v[len(v)-1], nil
}

// Series implements rollout.MetricSource.
func (s *Source) Series(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]float64, error) {
	s.once.Do(func() { s.auth = &azure.Auth{Authority: s.Authority, IMDS: s.IMDS, Client: s.Client} })
	cred := azure.Credential{TenantID: s.TenantID, ClientID: s.ClientID}
	if s.ClientSecretRef != "" {
		sec, err := s.Secret(ctx, s.ClientSecretRef)
		if err != nil {
			return nil, fmt.Errorf("azure-monitor: client secret: %w", err)
		}
		cred.ClientSecret = strings.TrimSpace(sec)
	}
	endpoint := strings.TrimRight(s.Endpoint, "/")
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	c := &azure.Client{Endpoint: endpoint, Auth: s.auth, Cred: cred, HTTP: s.Client, Scope: scope}
	q := strings.ReplaceAll(query, "{step}", strconv.Itoa(max(int(step.Seconds()), 1))+"s")
	body := map[string]string{"query": q, "timespan": start.UTC().Format(time.RFC3339) + "/" + end.UTC().Format(time.RFC3339)}
	var out struct {
		Tables []struct {
			Columns []struct {
				Name string `json:"name"`
				Type string `json:"type"`
			} `json:"columns"`
			Rows [][]any `json:"rows"`
		} `json:"tables"`
	}
	if _, err := c.Do(ctx, http.MethodPost, "/v1/workspaces/"+s.WorkspaceID+"/query", "", body, &out); err != nil {
		return nil, fmt.Errorf("azure-monitor: %w", err)
	}
	if len(out.Tables) == 0 {
		return nil, nil
	}
	t := out.Tables[0]
	col := -1
	for i, cl := range t.Columns {
		switch strings.ToLower(cl.Type) {
		case "real", "long", "int", "decimal":
			col = i
		}
	}
	if col < 0 {
		if len(t.Rows) == 0 {
			return nil, nil
		}
		return nil, errors.New("azure-monitor: the query does not return a numeric column")
	}
	var vals []float64
	for _, row := range t.Rows {
		if col < len(row) {
			switch v := row[col].(type) {
			case float64:
				vals = append(vals, v)
			case string:
				if f, err := strconv.ParseFloat(v, 64); err == nil {
					vals = append(vals, f)
				}
			}
		}
	}
	return vals, nil
}
