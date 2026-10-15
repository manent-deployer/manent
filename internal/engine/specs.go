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

// SpecsOptions seleziona le specifiche degli step personalizzati da cercare.
type SpecsOptions struct {
	// Hashes sono gli sha256 cercati (di solito pipeline.custom.revoked_specs).
	Hashes []string
	// All elenca tutte le specifiche fotografate, non solo quelle in Hashes.
	All bool
}

// SpecUse è uno step personalizzato fotografato nel piano di un workflow non concluso o di
// una schedule ricorrente.
type SpecUse struct {
	// Kind è workflow o schedule.
	Kind string `json:"kind"`
	// ID è l'ID del workflow o il nome della schedule.
	ID string `json:"id"`
	// Workflow è il nome del workflow DBOS (manent.deploy, manent.approval, ...).
	Workflow    string `json:"workflow,omitempty"`
	Status      string `json:"status"`
	Application string `json:"application,omitempty"`
	Environment string `json:"environment,omitempty"`
	Step        string `json:"step"`
	SpecHash    string `json:"spec_hash"`
	// Mismatch indica che lo spec_hash registrato non corrisponde alla specifica: il piano è
	// stato alterato, e il worker lo rifiuterà se le specifiche sono firmate.
	Mismatch bool `json:"mismatch,omitempty"`
}

// SpecsReport è il risultato di Specs.
type SpecsReport struct {
	Hashes []string  `json:"hashes,omitempty"`
	Uses   []SpecUse `json:"uses"`
	// Unreadable sono i workflow e le schedule il cui input non è leggibile.
	Unreadable []string `json:"unreadable,omitempty"`
}

// specWorkflows sono i workflow il cui input contiene un piano: deploy e figli dei passi, e le
// attese (approvazione, dipendenze) che accoderanno un deploy con il piano registrato. Le
// release non c'entrano: accodano i deploy nel primo step, con il piano calcolato allora.
var specWorkflows = []string{workflowName, approvalWorkflowName, afterWorkflowName, passWorkflowName}

// Specs elenca i workflow non conclusi e le schedule che contengono le specifiche cercate.
// Una specifica revocata con pipeline.custom.revoked_specs non viene più eseguita, ma resta
// nei piani già registrati: questo elenco dice quali deploy falliranno allo step e quali
// schedule vanno riapplicate con la configurazione corretta.
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

// planOf legge applicazione, ambiente e piano dall'input di un workflow.
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
		// Approvazioni e attese hanno la stessa forma: ID del deploy e richiesta.
		var in approvalInput
		if err := decodeInput(w.Input, &in); err != nil {
			return "", "", nil, err
		}
		req = in.Request
	}
	return req.Descriptor.Application, req.Descriptor.Environment, req.Plan, nil
}

// specUses restituisce gli step personalizzati del piano con una specifica cercata. L'hash è
// ricalcolato dalla specifica, non preso da quello registrato.
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
