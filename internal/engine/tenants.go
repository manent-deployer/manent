package engine

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/google/uuid"

	"github.com/joeg-ita/manent/internal/auth"
	"github.com/joeg-ita/manent/internal/callback"
	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/tenant"
)

// tenantRef returns the tenant with its tags from the registry; without the registry or if
// it cannot be read, the name alone (the config step reads and checks the tenant anyway).
func (e *Engine) tenantRef(ctx context.Context, name string) *tenant.Ref {
	if e.tenants != nil {
		if t, err := e.tenants.Tenant(ctx, name); err == nil {
			return t.Ref()
		}
	}
	return &tenant.Ref{Name: name}
}

// tenantSuffix describes the tenant in the error messages.
func tenantSuffix(name string) string {
	if name == "" {
		return ""
	}
	return " for tenant " + name
}

// Adoption is the release adopted by a deploy of kind adoption.
type Adoption struct {
	// Source is the deploy whose release is adopted; SHA256 its artifact.
	Source string `json:"source"`
	SHA256 string `json:"sha256,omitempty"`
}

// AdoptRequest asks to record as a deploy of tenant in environment To the release in service
// in From (an environment per customer, before tenants).
type AdoptRequest struct {
	Application string
	// Artifact chooses the artifact, required when From has more
	// than one.
	Artifact string
	From, To string
	Tenant   string
}

// Adopt records the adoption: a deploy of kind adoption that runs no step and leaves the
// targets as they are, so promotions and rollbacks of the tenant start from the release
// really in service. It is a platform operation (manent-server admin), outside permissions
// and governance.
func (e *Engine) Adopt(ctx context.Context, r AdoptRequest) (*Submission, error) {
	if r.Application == "" || r.From == "" || r.To == "" || r.Tenant == "" {
		return nil, fmt.Errorf("%w: application, from, to and tenant are required", ErrInvalidRequest)
	}
	if err := tenant.ValidName(r.Tenant); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	artifact, err := e.resolveArtifact(r.Application, r.From, "", r.Artifact)
	if err != nil {
		return nil, err
	}
	inv, err := e.Inventory(ctx, InventoryFilter{Application: r.Application, Environment: r.From, Artifact: artifact})
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(inv, func(ie InventoryEntry) bool { return ie.Tenant == "" && ie.Current != nil })
	if i < 0 {
		return nil, fmt.Errorf("%w: no release in service of %s in %s", ErrInvalidRequest, r.Application, r.From)
	}
	src := inv[i].Current
	req, err := e.loadRequest(src.ID)
	if err != nil {
		return nil, err
	}
	d := req.Descriptor
	d.Environment, d.Tenant, d.Tenants, d.ID, d.Schedule, d.After, d.DryRun = r.To, r.Tenant, nil, "", nil, nil, false
	d.Artifact.SHA256 = cmp.Or(src.SHA256, d.Artifact.SHA256)
	plan, err := e.planner.Plan(&d)
	if err != nil {
		return nil, err
	}
	areq := Request{Descriptor: d, Plan: plan, RequestedBy: auth.FromContext(ctx).String(), Kind: KindAdoption, Source: src.ID,
		Tenant: e.tenantRef(ctx, r.Tenant), Adopted: &Adoption{Source: src.ID, SHA256: src.SHA256}}
	h, err := dbos.Enqueue[Result](e.client, e.queueName, workflowName, areq, deployOptions(areq, uuid.NewString())...)
	if err != nil {
		return nil, err
	}
	e.logger.Warn("release adottata come deploy del tenant", "deployment", h.GetWorkflowID(), "application", r.Application,
		"from", r.From, "to", r.To, "tenant", r.Tenant, "version", d.Artifact.Version, "source", src.ID)
	return &Submission{ID: h.GetWorkflowID(), State: string(dbos.WorkflowStatusEnqueued)}, nil
}

// adoptWorkflow records an adoption: no step, attributes of a successful deploy with the
// sha256 of the adopted release, audit.
func (w *Worker) adoptWorkflow(ctx dbos.Context, id string, req Request) (Result, error) {
	res := Result{DeploymentID: id, Steps: []StepResult{
		{Name: stepArtifacts, Output: map[string]string{outSHA256: req.Adopted.SHA256}},
		{Name: "adopt", Output: map[string]string{"source": req.Adopted.Source}},
	}}
	final := callback.Event{Type: descriptor.CallbackEventDeployment, Status: callback.StatusSucceeded}
	w.recordOutcome(ctx, id, req.RequestedBy, &req.Descriptor, final)
	w.recordAttributes(ctx, id, req, res, final)
	w.logger.Info("adozione registrata", "deployment", id, "tenant", req.Descriptor.Tenant, "source", req.Adopted.Source)
	return res, nil
}
