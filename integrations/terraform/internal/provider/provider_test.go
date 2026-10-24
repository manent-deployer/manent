package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	fwprovider "github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/manent-deployer/manent/internal/service"
)

func TestSchemiValidi(t *testing.T) {
	ctx := context.Background()
	p := New("test")()
	var ps fwprovider.SchemaResponse
	p.Schema(ctx, fwprovider.SchemaRequest{}, &ps)
	if ps.Diagnostics.HasError() {
		t.Fatal(ps.Diagnostics)
	}
	for _, f := range p.Resources(ctx) {
		var rs resource.SchemaResponse
		f().Schema(ctx, resource.SchemaRequest{}, &rs)
		if diags := rs.Schema.ValidateImplementation(ctx); diags.HasError() {
			t.Errorf("schema della risorsa: %v", diags)
		}
	}
	for _, f := range p.DataSources(ctx) {
		var ds datasource.SchemaResponse
		f().Schema(ctx, datasource.SchemaRequest{}, &ds)
		if diags := ds.Schema.ValidateImplementation(ctx); diags.HasError() {
			t.Errorf("schema del data source: %v", diags)
		}
	}
}

const testDescriptor = `apiVersion: manent/v1
id: fisso
application: shop
environment: prod
artifact: {type: application, name: shop, version: "0.0.0", source: "file:///srv/shop.tgz"}
`

func TestBuildDescriptor(t *testing.T) {
	m := &deploymentModel{
		Application: types.StringValue("shop"), Environment: types.StringValue("prod"), Descriptor: types.StringValue(testDescriptor),
		Version: types.StringValue("1.4.0"), Source: types.StringValue("file:///srv/shop-1.4.0.tgz"),
	}
	d, err := buildDescriptor(m)
	if err != nil {
		t.Fatal(err)
	}
	// The ID is dropped: every apply is a new deploy.
	if d.ID != "" || d.Artifact.Version != "1.4.0" || d.Artifact.Source != "file:///srv/shop-1.4.0.tgz" {
		t.Errorf("descrittore: %+v", d)
	}
	m.Environment = types.StringValue("test")
	if _, err := buildDescriptor(m); err == nil || !strings.Contains(err.Error(), "shop/prod") {
		t.Errorf("ambiente diverso dal descrittore: %v", err)
	}
}

func TestReadInventory(t *testing.T) {
	e := service.InventoryEntry{Current: &service.Status{ID: "d2", State: "SUCCESS", Version: "1.3.0"}, OutOfSync: true}
	m := &deploymentModel{Version: types.StringValue("1.4.0"), RedeployOutOfSync: types.BoolValue(false)}
	readInventory(m, e)
	// A version released outside Terraform becomes a difference in the plan.
	if m.Version.ValueString() != "1.3.0" || m.ID.ValueString() != "d2" || !m.OutOfSync.ValueBool() {
		t.Errorf("modello: %+v", m)
	}
	m.RedeployOutOfSync = types.BoolValue(true)
	readInventory(m, e)
	if m.Version.ValueString() != "1.3.0"+outOfSyncSuffix {
		t.Errorf("redeploy_out_of_sync: %q", m.Version.ValueString())
	}
	if a, b, ok := cutPair("shop/prod"); !ok || a != "shop" || b != "prod" {
		t.Error("cutPair")
	}
	if _, _, ok := cutPair("shop"); ok {
		t.Error("cutPair senza /")
	}
}
