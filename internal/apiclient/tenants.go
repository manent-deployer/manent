package apiclient

import (
	"context"
	"net/http"
	"net/url"

	"github.com/joeg-ita/manent/internal/service"
)

// Tenants implements service.Service.
func (c *Client) Tenants(ctx context.Context, f service.TenantFilter) ([]service.TenantInfo, error) {
	q := url.Values{}
	for k, v := range map[string]string{"application": f.Application, "selector": f.Selector} {
		if v != "" {
			q.Set(k, v)
		}
	}
	var out struct {
		Tenants []service.TenantInfo `json:"tenants"`
	}
	err := c.do(ctx, http.MethodGet, "/tenants", q, nil, &out)
	return out.Tenants, err
}

// Tenant implements service.Service.
func (c *Client) Tenant(ctx context.Context, name string) (*service.TenantInfo, error) {
	var t service.TenantInfo
	if err := c.do(ctx, http.MethodGet, "/tenants/"+url.PathEscape(name), nil, nil, &t); err != nil {
		return nil, err
	}
	return &t, nil
}
