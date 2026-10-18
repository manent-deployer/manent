package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/joeg-ita/manent/internal/service"
)

// NewTenantsDataSource returns the manent_tenants data source.
func NewTenantsDataSource() datasource.DataSource { return &tenantsDataSource{} }

// tenantsDataSource lists the tenants of the registry: with for_each, a manent_deployment per
// tenant.
type tenantsDataSource struct {
	svc service.Service
}

type tenantsModel struct {
	Application types.String `tfsdk:"application"`
	Environment types.String `tfsdk:"environment"`
	Selector    types.String `tfsdk:"selector"`
	Names       []string     `tfsdk:"names"`
	Tenants     []tenantItem `tfsdk:"tenants"`
}

type tenantItem struct {
	Name types.String            `tfsdk:"name"`
	Tags map[string]types.String `tfsdk:"tags"`
}

// Metadata implements datasource.DataSource.
func (d *tenantsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_tenants"
}

// Schema implements datasource.DataSource.
func (d *tenantsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Tenant del registro di manent, filtrati per applicazione, ambiente e selettore sui tag.",
		Attributes: map[string]schema.Attribute{
			"application": schema.StringAttribute{Optional: true, Description: "Solo i tenant collegati a questa applicazione."},
			"environment": schema.StringAttribute{Optional: true, Description: "Solo i tenant collegati a questo ambiente (con application)."},
			"selector":    schema.StringAttribute{Optional: true, Description: "Selettore sui tag e sul nome, es. tier=gold."},
			"names":       schema.ListAttribute{Computed: true, ElementType: types.StringType, Description: "Nomi dei tenant, per for_each."},
			"tenants": schema.ListNestedAttribute{
				Computed: true,
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"name": schema.StringAttribute{Computed: true},
					"tags": schema.MapAttribute{Computed: true, ElementType: types.StringType},
				}},
			},
		},
	}
}

// Configure implements datasource.DataSourceWithConfigure.
func (d *tenantsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	svc, ok := serviceFrom(req.ProviderData)
	if !ok {
		resp.Diagnostics.AddError("provider not configured", fmt.Sprintf("unexpected type %T", req.ProviderData))
		return
	}
	d.svc = svc
}

// Read implements datasource.DataSource.
func (d *tenantsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var m tenantsModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	ts, err := d.svc.Tenants(ctx, service.TenantFilter{Application: m.Application.ValueString(), Environment: m.Environment.ValueString(), Selector: m.Selector.ValueString()})
	if err != nil {
		resp.Diagnostics.AddError("reading the tenants", err.Error())
		return
	}
	m.Names, m.Tenants = []string{}, []tenantItem{}
	for _, t := range ts {
		tags := map[string]types.String{}
		for k, v := range t.Tags {
			tags[k] = types.StringValue(v)
		}
		m.Names = append(m.Names, t.Name)
		m.Tenants = append(m.Tenants, tenantItem{Name: types.StringValue(t.Name), Tags: tags})
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}
