package engine

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/joeg-ita/manent/internal/deploylog"
)

// workflowFor restituisce l'ID del workflow che rappresenta il deploy: il deploy stesso o, se
// non è ancora partito, la sua approvazione.
func (e *Engine) workflowFor(id string) (dbos.WorkflowStatus, error) {
	ws, err := dbos.ListWorkflows(e.client, dbos.WithFilterWorkflowIDs(id, id+approvalSuffix),
		dbos.WithFilterName(workflowNames...), dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false))
	if err != nil {
		return dbos.WorkflowStatus{}, err
	}
	for _, w := range ws {
		if w.ID == id {
			return w, nil
		}
	}
	if len(ws) > 0 {
		return ws[0], nil
	}
	return dbos.WorkflowStatus{}, fmt.Errorf("%w: deploy %s", ErrNotFound, id)
}

// Steps implementa InsightService.
func (e *Engine) Steps(_ context.Context, id string) ([]StepRecord, error) {
	w, err := e.workflowFor(id)
	if err != nil {
		return nil, err
	}
	steps, err := dbos.GetWorkflowSteps(e.client, w.ID, dbos.WithStepsLoadOutput(true))
	if err != nil {
		return nil, err
	}
	out := make([]StepRecord, len(steps))
	for i, s := range steps {
		r := StepRecord{ID: s.StepID, Name: s.StepName, Output: s.Output}
		if !s.StartedAt.IsZero() {
			t := s.StartedAt.UTC()
			r.StartedAt = &t
		}
		if !s.CompletedAt.IsZero() {
			t := s.CompletedAt.UTC()
			r.CompletedAt = &t
			if r.StartedAt != nil {
				r.DurationMs = t.Sub(*r.StartedAt).Milliseconds()
			}
		}
		if s.Error != nil {
			r.Error = s.Error.Error()
		}
		out[i] = r
	}
	return out, nil
}

// Logs implementa InsightService.
func (e *Engine) Logs(_ context.Context, id string, offset int) (*deploylog.Page, error) {
	if offset < 0 {
		return nil, fmt.Errorf("%w: offset negativo", ErrInvalidRequest)
	}
	w, err := e.workflowFor(id)
	if err != nil {
		return nil, err
	}
	p := deploylog.Page{Lines: []deploylog.Line{}, Next: offset}
	if w.ID == id {
		if p, err = deploylog.Read(e.client, id, offset); err != nil {
			return nil, err
		}
	}
	p.Done = p.Done || Terminal(toStatus(w).State)
	return &p, nil
}

// doraAcc accumula i dati di un ambiente.
type doraAcc struct {
	m         DORAMetrics
	successes int
	restores  []time.Duration
	leads     []time.Duration
	// events sono i deploy conclusi del periodo, in ordine cronologico.
	events []Status
}

// DORA implementa InsightService. Esamina al più gli ultimi maxListScan deploy: con uno storico
// più lungo (o una retention più breve del periodo) le metriche sono parziali.
func (e *Engine) DORA(_ context.Context, f DORAFilter) (*DORAReport, error) {
	until := e.now().UTC()
	if f.Since.IsZero() {
		f.Since = until.AddDate(0, 0, -30)
	}
	firstSuccess := map[string]time.Time{}
	byEnv := map[string]*doraAcc{}
	err := e.scan(nil, func(s Status) bool {
		if s.DryRun || s.Application == "" {
			return true
		}
		if s.State == string(dbos.WorkflowStatusSuccess) {
			// Lo scan va dal più recente: l'ultimo valore visto è il primo rilascio.
			firstSuccess[s.Application+"\x00"+s.Artifact+"\x00"+s.Version] = s.UpdatedAt
		}
		if s.UpdatedAt.Before(f.Since) || (f.Application != "" && s.Application != f.Application) ||
			(f.Environment != "" && s.Environment != f.Environment) || (f.Match != nil && !f.Match(s)) {
			return true
		}
		switch s.State {
		case string(dbos.WorkflowStatusSuccess), string(dbos.WorkflowStatusError), string(dbos.WorkflowStatusMaxRecoveryAttemptsExceeded):
		default:
			return true
		}
		k := s.Application + "/" + s.Environment
		a := byEnv[k]
		if a == nil {
			a = &doraAcc{m: DORAMetrics{Application: s.Application, Environment: s.Environment}}
			byEnv[k] = a
		}
		a.events = append(a.events, s)
		return true
	})
	if err != nil {
		return nil, err
	}
	days := until.Sub(f.Since).Hours() / 24
	rep := &DORAReport{Since: f.Since, Until: until, Environments: []DORAMetrics{}}
	var total doraAcc
	for _, k := range slices.Sorted(maps.Keys(byEnv)) {
		a := byEnv[k]
		a.compute(firstSuccess)
		a.finish(days)
		rep.Environments = append(rep.Environments, a.m)
		total.m.Deployments += a.m.Deployments
		total.m.Failures += a.m.Failures
		total.successes += a.successes
		total.restores = append(total.restores, a.restores...)
		total.leads = append(total.leads, a.leads...)
	}
	total.finish(days)
	rep.Total = total.m
	return rep, nil
}

// compute ricostruisce la cronologia dell'ambiente: guasti, ripristini e lead time.
func (a *doraAcc) compute(firstSuccess map[string]time.Time) {
	slices.Reverse(a.events)
	var failedAt time.Time
	for _, s := range a.events {
		a.m.Deployments++
		if s.State != string(dbos.WorkflowStatusSuccess) {
			a.m.Failures++
			if failedAt.IsZero() {
				failedAt = s.UpdatedAt
			}
			continue
		}
		a.successes++
		if s.Kind == KindRollback {
			// Un rollback manuale segnala che la release precedente era difettosa: il guasto
			// conta dalla richiesta del rollback, se non era già aperto.
			a.m.Failures++
			if failedAt.IsZero() {
				failedAt = s.CreatedAt
			}
		}
		if !failedAt.IsZero() {
			a.restores = append(a.restores, s.UpdatedAt.Sub(failedAt))
			failedAt = time.Time{}
		}
		if first, ok := firstSuccess[s.Application+"\x00"+s.Artifact+"\x00"+s.Version]; ok && s.Kind != KindRollback && first.Before(s.UpdatedAt) {
			a.leads = append(a.leads, s.UpdatedAt.Sub(first))
		}
	}
}

// finish calcola i rapporti.
func (a *doraAcc) finish(days float64) {
	if days > 0 {
		a.m.DeploymentsPerDay = round2(float64(a.successes) / days)
	}
	if a.m.Deployments > 0 {
		a.m.ChangeFailureRate = round2(float64(a.m.Failures) / float64(a.m.Deployments))
	}
	if n := len(a.restores); n > 0 {
		var sum time.Duration
		for _, d := range a.restores {
			sum += d
		}
		a.m.MTTRSeconds = (sum / time.Duration(n)).Round(time.Second).Seconds()
	}
	if n := len(a.leads); n > 0 {
		sort.Slice(a.leads, func(i, j int) bool { return a.leads[i] < a.leads[j] })
		a.m.LeadTimeSeconds = a.leads[n/2].Round(time.Second).Seconds()
	}
}

func round2(v float64) float64 { return float64(int64(v*100+0.5)) / 100 }
