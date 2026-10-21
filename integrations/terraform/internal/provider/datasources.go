package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/service"
)

// NewInventoryDataSource returns the manent_inventory data source.
func NewInventoryDataSource() datasource.DataSource { return &inventoryDataSource{} }

// inventoryDataSource reads the release in service per application and environment.
type inventoryDataSource struct {
	svc service.Service
}

type inventoryModel struct {
	Application types.String     `tfsdk:"application"`
	Environment types.String     `tfsdk:"environment"`
	Artifact    types.String     `tfsdk:"artifact"`
	Entries     []inventoryEntry `tfsdk:"entries"`
}

type inventoryEntry struct {
	Application  types.String `tfsdk:"application"`
	Artifact     types.String `tfsdk:"artifact"`
	Environment  types.String `tfsdk:"environment"`
	Tenant       types.String `tfsdk:"tenant"`
	Version      types.String `tfsdk:"version"`
	DeploymentID types.String `tfsdk:"deployment_id"`
	SHA256       types.String `tfsdk:"sha256"`
	OutOfSync    types.Bool   `tfsdk:"out_of_sync"`
}

// Metadata implements datasource.DataSource.
func (d *inventoryDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_inventory"
}

// Schema implements datasource.DataSource.
func (d *inventoryDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Release in esercizio per applicazione e ambiente (inventario di manent).",
		Attributes: map[string]schema.Attribute{
			"application": schema.StringAttribute{Optional: true, Description: "Solo questa applicazione."},
			"environment": schema.StringAttribute{Optional: true, Description: "Solo questo ambiente."},
			"artifact":    schema.StringAttribute{Optional: true, Description: "Solo questo artefatto (artifact.name)."},
			"entries": schema.ListNestedAttribute{
				Computed: true,
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"application":   schema.StringAttribute{Computed: true},
					"artifact":      schema.StringAttribute{Computed: true, Description: "Artefatto della voce con deploy.identity: artifact."},
					"environment":   schema.StringAttribute{Computed: true},
					"tenant":        schema.StringAttribute{Computed: true},
					"version":       schema.StringAttribute{Computed: true},
					"deployment_id": schema.StringAttribute{Computed: true},
					"sha256":        schema.StringAttribute{Computed: true},
					"out_of_sync":   schema.BoolAttribute{Computed: true},
				}},
			},
		},
	}
}

// Configure implements datasource.DataSourceWithConfigure.
func (d *inventoryDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	svc, ok := serviceFrom(req.ProviderData)
	if !ok {
		resp.Diagnostics.AddError("provider not configured", fmt.Sprintf("unexpected type %T", req.ProviderData))
		return
	}
	d.svc = svc
}

// Read implements datasource.DataSource.
func (d *inventoryDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var m inventoryModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	inv, err := d.svc.Inventory(ctx, service.InventoryFilter{Application: m.Application.ValueString(), Environment: m.Environment.ValueString(), Artifact: m.Artifact.ValueString()})
	if err != nil {
		resp.Diagnostics.AddError("reading the inventory", err.Error())
		return
	}
	m.Entries = []inventoryEntry{}
	for _, e := range inv {
		if e.Current == nil {
			continue
		}
		m.Entries = append(m.Entries, inventoryEntry{
			Application: types.StringValue(e.Application), Artifact: types.StringValue(e.Artifact), Environment: types.StringValue(e.Environment), Tenant: types.StringValue(e.Tenant),
			Version: types.StringValue(e.Current.Version), DeploymentID: types.StringValue(e.Current.ID),
			SHA256: types.StringValue(e.Current.SHA256), OutOfSync: types.BoolValue(e.OutOfSync),
		})
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

// NewEnvironmentCheckDataSource returns the manent_environment_check data source.
func NewEnvironmentCheckDataSource() datasource.DataSource { return &environmentCheckDataSource{} }

// environmentCheckDataSource checks a candidate environment configuration on the server: an
// invalid configuration fails the plan, before another resource (consul_keys, local_file, a
// commit) writes it into the configuration system.
type environmentCheckDataSource struct {
	svc service.Service
}

type environmentCheckModel struct {
	Application types.String   `tfsdk:"application"`
	Environment types.String   `tfsdk:"environment"`
	Config      types.String   `tfsdk:"config"`
	Descriptor  types.String   `tfsdk:"descriptor"`
	Valid       types.Bool     `tfsdk:"valid"`
	Plugin      types.String   `tfsdk:"plugin"`
	Targets     []types.String `tfsdk:"targets"`
	Stages      []types.String `tfsdk:"stages"`
	Warnings    []types.String `tfsdk:"warnings"`
}

// Metadata implements datasource.DataSource.
func (d *environmentCheckDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_environment_check"
}

// Schema implements datasource.DataSource.
func (d *environmentCheckDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Verifica sul server una configurazione di ambiente candidata, come la userebbe un deploy: " +
			"una configurazione non valida fa fallire il plan.",
		Attributes: map[string]schema.Attribute{
			"application": schema.StringAttribute{Required: true},
			"environment": schema.StringAttribute{Required: true},
			"config":      schema.StringAttribute{Required: true, Description: "Configurazione di ambiente YAML o JSON."},
			"descriptor":  schema.StringAttribute{Optional: true, Description: "Descrittore di esempio YAML; default la richiesta del deploy in esercizio."},
			"valid":       schema.BoolAttribute{Computed: true},
			"plugin":      schema.StringAttribute{Computed: true, Description: "Plugin del deploy effettivo."},
			"targets":     schema.ListAttribute{Computed: true, ElementType: types.StringType, Description: "Host dei target del deploy effettivo."},
			"stages":      schema.ListAttribute{Computed: true, ElementType: types.StringType, Description: "Stadi del rollout, se c'è."},
			"warnings":    schema.ListAttribute{Computed: true, ElementType: types.StringType},
		},
	}
}

// Configure implements datasource.DataSourceWithConfigure.
func (d *environmentCheckDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	svc, ok := serviceFrom(req.ProviderData)
	if !ok {
		resp.Diagnostics.AddError("provider not configured", fmt.Sprintf("unexpected type %T", req.ProviderData))
		return
	}
	d.svc = svc
}

// Read implements datasource.DataSource.
func (d *environmentCheckDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var m environmentCheckModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	check := service.EnvironmentCheck{Application: m.Application.ValueString(), Environment: m.Environment.ValueString(), Config: m.Config.ValueString()}
	if raw := m.Descriptor.ValueString(); raw != "" {
		ex, err := descriptor.ParseYAML([]byte(raw))
		if err != nil {
			resp.Diagnostics.AddError("invalid example descriptor", err.Error())
			return
		}
		check.Descriptor = ex
	}
	v, err := d.svc.ValidateEnvironment(ctx, check)
	if err != nil {
		resp.Diagnostics.AddError(fmt.Sprintf("invalid configuration of %s/%s", check.Application, check.Environment), err.Error())
		return
	}
	m.Valid = types.BoolValue(v.Valid)
	m.Plugin = types.StringValue("")
	m.Targets, m.Stages, m.Warnings = []types.String{}, []types.String{}, []types.String{}
	if v.Deploy != nil {
		m.Plugin = types.StringValue(v.Deploy.Plugin)
		for _, t := range v.Deploy.Targets {
			m.Targets = append(m.Targets, types.StringValue(t.Host))
		}
	}
	if v.Rollout != nil {
		for _, s := range v.Rollout.Stages {
			m.Stages = append(m.Stages, types.StringValue(s.Name))
		}
	}
	for _, w := range v.Warnings {
		m.Warnings = append(m.Warnings, types.StringValue(w))
		resp.Diagnostics.AddWarning("verifica della configurazione", w)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}
