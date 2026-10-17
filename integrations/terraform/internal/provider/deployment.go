package provider

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/service"
)

// outOfSyncSuffix marks in the state a version whose targets were changed outside manent: the
// difference from the configuration makes Terraform plan a new deploy.
const outOfSyncSuffix = " (target fuori sincronia)"

// NewDeploymentResource returns the manent_deployment resource.
func NewDeploymentResource() resource.Resource { return &deploymentResource{} }

// deploymentResource is the release in service of an application in an environment. Creating
// or changing it runs a deploy and waits for it; reading it compares the configuration with
// the inventory, so a version released (or rolled back) outside Terraform shows in the plan.
type deploymentResource struct {
	svc service.Service
}

type deploymentModel struct {
	ID                types.String `tfsdk:"id"`
	Application       types.String `tfsdk:"application"`
	Environment       types.String `tfsdk:"environment"`
	Descriptor        types.String `tfsdk:"descriptor"`
	Version           types.String `tfsdk:"version"`
	Source            types.String `tfsdk:"source"`
	SHA256            types.String `tfsdk:"sha256"`
	Timeout           types.String `tfsdk:"timeout"`
	RedeployOutOfSync types.Bool   `tfsdk:"redeploy_out_of_sync"`
	State             types.String `tfsdk:"state"`
	OutOfSync         types.Bool   `tfsdk:"out_of_sync"`
}

// Metadata implements resource.Resource.
func (r *deploymentResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_deployment"
}

// Schema implements resource.Resource.
func (r *deploymentResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	replace := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		Description: "Release in esercizio di un'applicazione in un ambiente. Crearla o cambiarla esegue un deploy e ne attende la fine " +
			"(approvazioni comprese, entro timeout); eliminarla non tocca i target.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "ID del deploy in esercizio.",
			},
			"application": schema.StringAttribute{Required: true, PlanModifiers: replace, Description: "Applicazione."},
			"environment": schema.StringAttribute{Required: true, PlanModifiers: replace, Description: "Ambiente."},
			"descriptor": schema.StringAttribute{
				Required:    true,
				Description: "Descrittore di deploy YAML (es. file(\"deploy/prod.yml\")); version, source e sha256 lo sovrascrivono.",
			},
			"version": schema.StringAttribute{Required: true, Description: "Versione dell'artefatto da tenere in esercizio."},
			"source":  schema.StringAttribute{Optional: true, Description: "Sorgente dell'artefatto, al posto di quella del descrittore."},
			"sha256":  schema.StringAttribute{Optional: true, Description: "sha256 atteso dell'artefatto."},
			"timeout": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString("30m"),
				Description: "Tempo massimo di attesa del deploy (es. 30m).",
			},
			"redeploy_out_of_sync": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				Description: "Se true, un ambiente con target cambiati fuori da manent (riconciliazione) viene rilasciato di nuovo al prossimo apply.",
			},
			"state":       schema.StringAttribute{Computed: true, Description: "Stato del deploy in esercizio."},
			"out_of_sync": schema.BoolAttribute{Computed: true, Description: "Target cambiati fuori da manent secondo l'ultima riconciliazione."},
		},
	}
}

// Configure implements resource.ResourceWithConfigure.
func (r *deploymentResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	svc, ok := serviceFrom(req.ProviderData)
	if !ok {
		resp.Diagnostics.AddError("provider not configured", fmt.Sprintf("unexpected type %T", req.ProviderData))
		return
	}
	r.svc = svc
}

// Create implements resource.Resource.
func (r *deploymentResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m deploymentModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	diags := r.deploy(ctx, &m)
	// Also on failure: the resource is saved as tainted, and the next apply tries again.
	if m.ID.ValueString() != "" {
		resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
	}
	resp.Diagnostics.Append(diags...)
}

// Update implements resource.Resource: every change is a new deploy.
func (r *deploymentResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m deploymentModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	diags := r.deploy(ctx, &m)
	if !diags.HasError() {
		resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
	}
	resp.Diagnostics.Append(diags...)
}

// Read implements resource.Resource: the inventory says what is in service.
func (r *deploymentResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m deploymentModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	inv, err := r.svc.Inventory(ctx, service.InventoryFilter{Application: m.Application.ValueString(), Environment: m.Environment.ValueString()})
	if err != nil {
		resp.Diagnostics.AddError("reading the inventory", err.Error())
		return
	}
	if len(inv) == 0 || inv[0].Current == nil {
		// Nothing in service (e.g. history deleted): the next apply deploys again.
		resp.State.RemoveResource(ctx)
		return
	}
	readInventory(&m, inv[0])
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

// readInventory writes into the model the release in service.
func readInventory(m *deploymentModel, e service.InventoryEntry) {
	cur := e.Current
	m.ID, m.State, m.OutOfSync = types.StringValue(cur.ID), types.StringValue(cur.State), types.BoolValue(e.OutOfSync)
	m.Version = types.StringValue(cur.Version)
	if e.OutOfSync && m.RedeployOutOfSync.ValueBool() {
		m.Version = types.StringValue(cur.Version + outOfSyncSuffix)
	}
}

// Delete implements resource.Resource: manent does not remove releases from the targets.
func (r *deploymentResource) Delete(_ context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	resp.Diagnostics.AddWarning("release lasciata in esercizio",
		"manent non rimuove le release dai target: la risorsa esce dallo stato di Terraform, i target restano invariati.")
}

// ImportState implements resource.ResourceWithImportState: the ID is <application>/<environment>.
func (r *deploymentResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	app, env, ok := cutPair(req.ID)
	if !ok {
		resp.Diagnostics.AddError("invalid import ID", "use <application>/<environment>")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("application"), app)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("environment"), env)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("timeout"), "30m")...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("redeploy_out_of_sync"), false)...)
}

// deploy runs the deploy of the model and waits for it, writing ID and state into the model.
func (r *deploymentResource) deploy(ctx context.Context, m *deploymentModel) diag.Diagnostics {
	var diags diag.Diagnostics
	d, err := buildDescriptor(m)
	if err != nil {
		diags.AddError("invalid descriptor", err.Error())
		return diags
	}
	timeout, err := time.ParseDuration(m.Timeout.ValueString())
	if err != nil || timeout <= 0 {
		diags.AddError("invalid timeout", fmt.Sprintf("%q: provide a duration, e.g. 30m", m.Timeout.ValueString()))
		return diags
	}
	sub, err := r.svc.Submit(ctx, d)
	if err != nil {
		diags.AddError("deploy rifiutato", err.Error())
		return diags
	}
	m.ID = types.StringValue(sub.ID)
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, werr := r.svc.Wait(wctx, sub.ID)
	st, err := r.svc.Status(ctx, sub.ID)
	if err != nil {
		diags.AddError("stato del deploy "+sub.ID, err.Error())
		return diags
	}
	m.State, m.OutOfSync = types.StringValue(st.State), types.BoolValue(false)
	if werr != nil {
		diags.AddError("deploy "+sub.ID+" failed", werr.Error())
	}
	return diags
}

// buildDescriptor parses the descriptor and applies the attributes. The descriptor ID is
// dropped: a Terraform change is always a new deploy, also with the same version.
func buildDescriptor(m *deploymentModel) (*descriptor.Descriptor, error) {
	d, err := descriptor.ParseYAML([]byte(m.Descriptor.ValueString()))
	if err != nil {
		return nil, err
	}
	app, env := m.Application.ValueString(), m.Environment.ValueString()
	if d.Application != app || d.Environment != env {
		return nil, fmt.Errorf("the descriptor is for %s/%s, the resource for %s/%s", d.Application, d.Environment, app, env)
	}
	d.ID = ""
	d.Artifact.Version = m.Version.ValueString()
	if v := m.Source.ValueString(); v != "" {
		d.Artifact.Source = v
	}
	if v := m.SHA256.ValueString(); v != "" {
		d.Artifact.SHA256 = v
	}
	if d.Schedule != nil || d.DryRun {
		return nil, fmt.Errorf("schedule and dryRun are not allowed: the resource represents the release in service")
	}
	return d, d.Validate()
}

// cutPair splits "<application>/<environment>".
func cutPair(s string) (string, string, bool) {
	a, b, ok := strings.Cut(s, "/")
	return a, b, ok && a != "" && b != ""
}
