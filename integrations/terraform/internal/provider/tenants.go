package provider

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/manent-deployer/manent/internal/service"
)

// NewTenantsDataSource returns the manent_tenants data source.
func NewTenantsDataSource() datasource.DataSource { return &tenantsDataSource{} }

// tenantsDataSource lists the tenants of the registry, the teams that own the applications,
// with the applications and their tags.
type tenantsDataSource struct {
	svc service.Service
}

type tenantsModel struct {
	Application types.String `tfsdk:"application"`
	Selector    types.String `tfsdk:"selector"`
	Names       []string     `tfsdk:"names"`
	Tenants     []tenantItem `tfsdk:"tenants"`
}

type tenantItem struct {
	Name         types.String            `tfsdk:"name"`
	Tags         map[string]types.String `tfsdk:"tags"`
	Applications []string                `tfsdk:"applications"`
}

// Metadata implements datasource.DataSource.
func (d *tenantsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_tenants"
}

// Schema implements datasource.DataSource.
func (d *tenantsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Tenants of the manent registry (the teams that own the applications), filtered by owned application and tag selector.",
		Attributes: map[string]schema.Attribute{
			"application": schema.StringAttribute{Optional: true, Description: "Only the tenant that owns this application."},
			"selector":    schema.StringAttribute{Optional: true, Description: "Selector on the tenant tags and name, e.g. division=commerce."},
			"names":       schema.ListAttribute{Computed: true, ElementType: types.StringType, Description: "Tenant names, for for_each."},
			"tenants": schema.ListNestedAttribute{
				Computed: true,
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"name":         schema.StringAttribute{Computed: true},
					"tags":         schema.MapAttribute{Computed: true, ElementType: types.StringType},
					"applications": schema.ListAttribute{Computed: true, ElementType: types.StringType, Description: "Owned applications."},
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
	ts, err := d.svc.Tenants(ctx, service.TenantFilter{Application: m.Application.ValueString(), Selector: m.Selector.ValueString()})
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
		apps := slices.Sorted(maps.Keys(t.Applications))
		if apps == nil {
			apps = []string{}
		}
		m.Names = append(m.Names, t.Name)
		m.Tenants = append(m.Tenants, tenantItem{Name: types.StringValue(t.Name), Tags: tags, Applications: apps})
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}
