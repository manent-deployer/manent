package engine

import (
	"cmp"
	"context"
	"slices"
	"strings"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/joeg-ita/manent/internal/customspec"
	"github.com/joeg-ita/manent/internal/pipeline"
)

// SpecsOptions selects the custom step specs to look for.
type SpecsOptions struct {
	// Hashes are the sha256 looked for (usually pipeline.custom.revoked_specs).
	Hashes []string
	// All lists all captured specs, not only those in Hashes.
	All bool
}

// SpecUse is a custom step captured in the plan of an unfinished workflow or of a recurring
// schedule.
type SpecUse struct {
	// Kind is workflow or schedule.
	Kind string `json:"kind"`
	// ID is the workflow ID or the schedule name.
	ID string `json:"id"`
	// Workflow is the DBOS workflow name (manent.deploy, manent.approval, ...).
	Workflow    string `json:"workflow,omitempty"`
	Status      string `json:"status"`
	Application string `json:"application,omitempty"`
	Environment string `json:"environment,omitempty"`
	Step        string `json:"step"`
	SpecHash    string `json:"spec_hash"`
	// Mismatch means the recorded spec_hash does not match the spec: the plan was tampered with,
	// and the worker will reject it if specs are signed.
	Mismatch bool `json:"mismatch,omitempty"`
}

// SpecsReport is the result of Specs.
type SpecsReport struct {
	Hashes []string  `json:"hashes,omitempty"`
	Uses   []SpecUse `json:"uses"`
	// Unreadable are the workflows and schedules whose input cannot be read.
	Unreadable []string `json:"unreadable,omitempty"`
}

// specWorkflows are the workflows whose input contains a plan: deploys and pass children, and
// the waits (approval, dependencies) that will enqueue a deploy with the recorded plan.
// Releases are not involved: they enqueue deploys in their first step, with the plan computed then.
var specWorkflows = []string{workflowName, approvalWorkflowName, afterWorkflowName, passWorkflowName}

// Specs lists the unfinished workflows and schedules containing the specs looked for.
// A spec revoked with pipeline.custom.revoked_specs no longer runs, but stays in the plans
// already recorded: this listing says which deploys will fail at the step and which schedules
// must be applied again with the corrected configuration.
func (a *Admin) Specs(_ context.Context, o SpecsOptions) (*SpecsReport, error) {
	rep := &SpecsReport{Hashes: o.Hashes, Uses: []SpecUse{}}
	wanted := func(h string) bool { return o.All || slices.Contains(o.Hashes, h) }
	if !o.All && len(o.Hashes) == 0 {
		return rep, nil
	}

	ws, err := dbos.ListWorkflows(a.client,
		dbos.WithFilterName(specWorkflows...),
		dbos.WithFilterStatus(dbos.WorkflowStatusPending, dbos.WorkflowStatusEnqueued, dbos.WorkflowStatusDelayed),
		dbos.WithFilterLoadInput(true), dbos.WithFilterLoadOutput(false))
	if err != nil {
		return nil, err
	}
	for _, w := range ws {
		app, env, plan, err := planOf(w)
		if err != nil {
			rep.Unreadable = append(rep.Unreadable, w.ID)
			continue
		}
		for _, u := range specUses(plan, wanted) {
			u.Kind, u.ID, u.Workflow, u.Status, u.Application, u.Environment = "workflow", w.ID, w.Name, string(w.Status), app, env
			rep.Uses = append(rep.Uses, u)
		}
	}

	ss, err := dbos.ListSchedules(a.client, dbos.WithScheduleWorkflowNames(scheduledWorkflowName))
	if err != nil {
		return nil, err
	}
	for _, s := range ss {
		sc, err := dbos.DecodeScheduleContext[scheduleContext](dbos.ScheduledWorkflowInput{Context: s.Context})
		if err != nil {
			rep.Unreadable = append(rep.Unreadable, "schedule:"+s.ScheduleName)
			continue
		}
		d := sc.Request.Descriptor
		for _, u := range specUses(sc.Request.Plan, wanted) {
			u.Kind, u.ID, u.Status, u.Application, u.Environment = "schedule", s.ScheduleName, string(s.Status), d.Application, d.Environment
			rep.Uses = append(rep.Uses, u)
		}
	}
	slices.SortFunc(rep.Uses, func(x, y SpecUse) int {
		return cmp.Or(strings.Compare(x.Kind, y.Kind), strings.Compare(x.ID, y.ID), strings.Compare(x.Step, y.Step))
	})
	a.logger.Info("specifiche degli step personalizzati cercate", "hashes", len(o.Hashes), "all", o.All, "uses", len(rep.Uses), "unreadable", len(rep.Unreadable))
	return rep, nil
}

// planOf reads application, environment and plan from a workflow input.
func planOf(w dbos.WorkflowStatus) (string, string, pipeline.Plan, error) {
	if w.Name == passWorkflowName {
		var in passInput
		if err := decodeInput(w.Input, &in); err != nil {
			return "", "", nil, err
		}
		return in.Descriptor.Application, in.Descriptor.Environment, in.Steps, nil
	}
	var req Request
	if w.Name == workflowName {
		if err := decodeInput(w.Input, &req); err != nil {
			return "", "", nil, err
		}
	} else {
		// Approvals and waits have the same shape: deploy ID and request.
		var in approvalInput
		if err := decodeInput(w.Input, &in); err != nil {
			return "", "", nil, err
		}
		req = in.Request
	}
	return req.Descriptor.Application, req.Descriptor.Environment, req.Plan, nil
}

// specUses returns the custom steps of the plan with a spec looked for. The hash is
// recomputed from the spec, not taken from the recorded one.
func specUses(plan pipeline.Plan, wanted func(string) bool) []SpecUse {
	var out []SpecUse
	for _, ps := range plan {
		if ps.Spec == "" {
			continue
		}
		h := customspec.Hash(ps.Spec)
		if !wanted(h) && !wanted(ps.SpecHash) {
			continue
		}
		out = append(out, SpecUse{Step: ps.Name, SpecHash: h, Mismatch: ps.SpecHash != "" && ps.SpecHash != h})
	}
	return out
}
