package clientcli

import (
	"context"

	"github.com/joeg-ita/manent/internal/cliutil"
	"github.com/joeg-ita/manent/internal/service"
)

// TenantsListCmd lists the tenants.
type TenantsListCmd struct {
	global      *GlobalOptions
	Application string `short:"a" long:"application" description:"only the tenant that owns this application"`
	Selector    string `long:"selector" description:"selector on the tenant tags and name, e.g. division=commerce"`
}

// Execute implements flags.Commander.
func (c *TenantsListCmd) Execute([]string) error {
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		ts, err := svc.Tenants(ctx, service.TenantFilter{Application: c.Application, Selector: c.Selector})
		if err != nil {
			return err
		}
		return cliutil.PrintJSON(ts)
	})
}

// TenantShowCmd shows a tenant.
type TenantShowCmd struct {
	global *GlobalOptions
	Args   struct {
		Name string `positional-arg-name:"tenant" required:"true"`
	} `positional-args:"yes"`
}

// Execute implements flags.Commander.
func (c *TenantShowCmd) Execute([]string) error {
	return withService(c.global, func(ctx context.Context, svc service.Service) error {
		t, err := svc.Tenant(ctx, c.Args.Name)
		if err != nil {
			return err
		}
		return cliutil.PrintJSON(t)
	})
}
