package provider

import (
	"context"
	"fmt"
	"slices"
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

	"github.com/manent-deployer/manent/internal/descriptor"
	"github.com/manent-deployer/manent/internal/service"
)

// outOfSyncSuffix marks in the state a version whose targets were changed outside manent: the
// difference from the configuration makes Terraform plan a new deploy.
const outOfSyncSuffix = " (targets out of sync)"

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
	Tenant            types.String `tfsdk:"tenant"`
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
		Description: "Release in service of an application in an environment. Creating or changing it runs a deploy and waits for it to end " +
			"(approvals included, within timeout); deleting it does not touch the targets.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "ID of the deploy in service.",
			},
			"tenant": schema.StringAttribute{
				Optional: true, Computed: true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplaceIfConfigured(), stringplanmodifier.UseStateForUnknown()},
				Description:   "Tenant of the application: required only if several tenants have an application with this name (untenanted for those without tenant).",
			},
			"application": schema.StringAttribute{Required: true, PlanModifiers: replace, Description: "Application."},
			"environment": schema.StringAttribute{Required: true, PlanModifiers: replace, Description: "Environment."},
			"descriptor": schema.StringAttribute{
				Required:    true,
				Description: "Deploy descriptor in YAML (e.g. file(\"deploy/prod.yml\")); version, source and sha256 override it.",
			},
			"version": schema.StringAttribute{Required: true, Description: "Version of the artifact to keep in service."},
			"source":  schema.StringAttribute{Optional: true, Description: "Source of the artifact, instead of the descriptor's."},
			"sha256":  schema.StringAttribute{Optional: true, Description: "Expected sha256 of the artifact."},
			"timeout": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString("30m"),
				Description: "Maximum time to wait for the deploy (e.g. 30m).",
			},
			"redeploy_out_of_sync": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				Description: "If true, an environment with targets changed outside manent (reconciliation) is released again at the next apply.",
			},
			"state":       schema.StringAttribute{Computed: true, Description: "State of the deploy in service."},
			"out_of_sync": schema.BoolAttribute{Computed: true, Description: "Targets changed outside manent according to the last reconciliation."},
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
	inv, err := r.svc.Inventory(ctx, service.InventoryFilter{Tenant: m.Tenant.ValueString(), Application: m.Application.ValueString(), Environment: m.Environment.ValueString()})
	if err != nil {
		resp.Diagnostics.AddError("reading the inventory", err.Error())
		return
	}
	// Every artifact has its own entry: the resource's is the
	// descriptor's artifact.
	artifact := descriptorArtifact(&m)
	i := slices.IndexFunc(inv, func(e service.InventoryEntry) bool {
		return e.Current != nil && (e.Artifact == "" || e.Artifact == artifact)
	})
	if i < 0 {
		// Nothing in service (e.g. history deleted): the next apply deploys again.
		resp.State.RemoveResource(ctx)
		return
	}
	readInventory(&m, inv[i])
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

// descriptorArtifact returns artifact.name of the resource's descriptor ("" if unreadable).
func descriptorArtifact(m *deploymentModel) string {
	d, err := descriptor.ParseYAML([]byte(m.Descriptor.ValueString()))
	if err != nil {
		return ""
	}
	return d.Artifact.Name
}

// readInventory writes into the model the release in service.
func readInventory(m *deploymentModel, e service.InventoryEntry) {
	cur := e.Current
	m.ID, m.State, m.OutOfSync = types.StringValue(cur.ID), types.StringValue(cur.State), types.BoolValue(e.OutOfSync)
	m.Version = types.StringValue(cur.Version)
	if e.Tenant != "" {
		m.Tenant = types.StringValue(e.Tenant)
	}
	if e.OutOfSync && m.RedeployOutOfSync.ValueBool() {
		m.Version = types.StringValue(cur.Version + outOfSyncSuffix)
	}
}

// Delete implements resource.Resource: manent does not remove releases from the targets.
func (r *deploymentResource) Delete(_ context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	resp.Diagnostics.AddWarning("release left live",
		"manent does not remove releases from the targets: the resource leaves the Terraform state, the targets stay unchanged.")
}

// ImportState implements resource.ResourceWithImportState: the ID is
// <application>/<environment> or <tenant>/<application>/<environment>.
func (r *deploymentResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	id := req.ID
	if parts := strings.Split(id, "/"); len(parts) == 3 && parts[0] != "" {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("tenant"), parts[0])...)
		id = parts[1] + "/" + parts[2]
	}
	app, env, ok := cutPair(id)
	if !ok {
		resp.Diagnostics.AddError("invalid import ID", "use <application>/<environment> or <tenant>/<application>/<environment>")
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
		diags.AddError("deploy refused", err.Error())
		return diags
	}
	m.ID = types.StringValue(sub.ID)
	if m.Tenant.IsUnknown() {
		// Computed: known before saving, also when the deploy fails.
		m.Tenant = types.StringValue("")
	}
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, werr := r.svc.Wait(wctx, sub.ID)
	st, err := r.svc.Status(ctx, sub.ID)
	if err != nil {
		diags.AddError("state of deploy "+sub.ID, err.Error())
		return diags
	}
	m.State, m.OutOfSync = types.StringValue(st.State), types.BoolValue(false)
	if st.Tenant != "" {
		m.Tenant = types.StringValue(st.Tenant)
	}
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
	if v := m.Tenant.ValueString(); v != "" {
		d.Tenant = v
	}
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
