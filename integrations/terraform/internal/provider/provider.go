// Package provider implements the Terraform provider of manent on the manent API, with the same
// client as the manent CLI (internal/apiclient).
package provider

import (
	"cmp"
	"context"
	"os"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/joeg-ita/manent/internal/apiclient"
	"github.com/joeg-ita/manent/internal/service"
)

// New returns the provider factory.
func New(version string) func() provider.Provider {
	return func() provider.Provider { return &manentProvider{version: version} }
}

type manentProvider struct {
	version string
}

type providerModel struct {
	Server    types.String `tfsdk:"server"`
	Token     types.String `tfsdk:"token"`
	TokenFile types.String `tfsdk:"token_file"`
	CAFile    types.String `tfsdk:"ca_file"`
}

// Metadata implements provider.Provider.
func (p *manentProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "manent"
	resp.Version = p.version
}

// Schema implements provider.Provider.
func (p *manentProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Rilasci, inventario e verifica della configurazione di ambiente di manent, tramite la sua API.",
		Attributes: map[string]schema.Attribute{
			"server": schema.StringAttribute{
				Optional:    true,
				Description: "URL del server manent; default MANENT_SERVER.",
			},
			"token": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				Description: "Token dell'API; default MANENT_TOKEN, poi token_file.",
			},
			"token_file": schema.StringAttribute{
				Optional:    true,
				Description: "File con il token dell'API; default MANENT_TOKEN_FILE.",
			},
			"ca_file": schema.StringAttribute{
				Optional:    true,
				Description: "Certificato della CA del server, se privata; default MANENT_CA_FILE.",
			},
		},
	}
}

// Configure implements provider.Provider: the client is shared by resources and data sources.
func (p *manentProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var m providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	server := cmp.Or(m.Server.ValueString(), os.Getenv("MANENT_SERVER"))
	token := cmp.Or(m.Token.ValueString(), os.Getenv("MANENT_TOKEN"))
	if file := cmp.Or(m.TokenFile.ValueString(), os.Getenv("MANENT_TOKEN_FILE")); token == "" && file != "" {
		raw, err := os.ReadFile(file)
		if err != nil {
			resp.Diagnostics.AddError("unreadable token_file", err.Error())
			return
		}
		token = strings.TrimSpace(string(raw))
	}
	if server == "" {
		resp.Diagnostics.AddError("missing server", "set server in the provider or the MANENT_SERVER variable")
		return
	}
	c, err := apiclient.NewWithOptions(apiclient.Options{Server: server, Token: token, CAFile: cmp.Or(m.CAFile.ValueString(), os.Getenv("MANENT_CA_FILE"))})
	if err != nil {
		resp.Diagnostics.AddError("client manent", err.Error())
		return
	}
	var svc service.Service = c
	resp.ResourceData, resp.DataSourceData = svc, svc
}

// Resources implements provider.Provider.
func (p *manentProvider) Resources(context.Context) []func() resource.Resource {
	return []func() resource.Resource{NewDeploymentResource}
}

// DataSources implements provider.Provider.
func (p *manentProvider) DataSources(context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{NewInventoryDataSource, NewEnvironmentCheckDataSource}
}

// serviceFrom returns the client set by Configure (nil before the provider is configured).
func serviceFrom(data any) (service.Service, bool) {
	if data == nil {
		return nil, true
	}
	svc, ok := data.(service.Service)
	return svc, ok
}
