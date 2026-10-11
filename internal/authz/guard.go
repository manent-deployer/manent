package authz

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/joeg-ita/manent/internal/audit"
	"github.com/joeg-ita/manent/internal/auth"
	"github.com/joeg-ita/manent/internal/deploylog"
	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/engine"
	"github.com/joeg-ita/manent/internal/governance"
	"github.com/joeg-ita/manent/internal/pipeline"
)

// Guard avvolge un engine.Service: verifica i permessi dell'identità nel context e registra
// nell'audit le azioni che modificano lo stato e quelle negate. Un'azione negata restituisce
// engine.ErrForbidden.
type Guard struct {
	Next   engine.Service
	Policy Policy
	Audit  audit.Recorder
	Logger *slog.Logger
}

var _ engine.Service = (*Guard)(nil)

// target identifica l'oggetto di un'azione, per permessi e audit.
type target struct {
	application, environment, deployment, schedule string
	dryRun                                         bool
	// comment è il commento di un'approvazione o il motivo di una deroga al freeze.
	comment string
}

// check verifica il permesso; se negato registra la voce di audit e restituisce l'errore.
func (g *Guard) check(ctx context.Context, a Action, t target) error {
	p := auth.FromContext(ctx)
	if g.Policy.Allowed(p, a, t.application, t.environment) {
		return nil
	}
	err := fmt.Errorf("%w: %s non può eseguire %s su %s/%s", engine.ErrForbidden, actor(p), a, t.application, t.environment)
	g.record(ctx, a, t, audit.OutcomeDenied, err)
	return err
}

// record registra una voce di audit; un errore di registrazione viene solo segnalato a log.
func (g *Guard) record(ctx context.Context, a Action, t target, outcome string, err error) {
	if g.Audit == nil {
		return
	}
	e := audit.Entry{
		Actor: actor(auth.FromContext(ctx)), Action: string(a), Outcome: outcome,
		Application: t.application, Environment: t.environment,
		Deployment: t.deployment, Schedule: t.schedule, DryRun: t.dryRun, Comment: t.comment,
	}
	if err != nil {
		e.Reason = err.Error()
	}
	if rerr := g.Audit.Record(ctx, e); rerr != nil {
		g.Logger.Error("registrazione nell'audit fallita", "action", a, "error", rerr)
	}
}

// done registra l'esito di un'azione autorizzata.
func (g *Guard) done(ctx context.Context, a Action, t target, err error) {
	outcome := audit.OutcomeAllowed
	if err != nil {
		outcome = audit.OutcomeError
	}
	g.record(ctx, a, t, outcome, err)
}

func actor(p *auth.Principal) string {
	if p == nil {
		return "unauthenticated"
	}
	return p.String()
}

// Plan richiede deployment.read: rivela la pipeline configurata per l'applicazione/ambiente.
func (g *Guard) Plan(ctx context.Context, d *descriptor.Descriptor) (pipeline.Plan, error) {
	if err := g.check(ctx, DeploymentRead, target{application: d.Application, environment: d.Environment}); err != nil {
		return nil, err
	}
	return g.Next.Plan(ctx, d)
}

// submitAction è l'azione richiesta per un deploy reale (act) o per un dry-run.
func submitAction(act Action, dryRun bool) Action {
	if dryRun {
		return DeploymentDryRun
	}
	return act
}

// checkOverride verifica il permesso di deroga al freeze, se richiesta.
func (g *Guard) checkOverride(ctx context.Context, override string, t target) error {
	if override == "" {
		return nil
	}
	return g.check(ctx, FreezeOverride, t)
}

// Submit richiede deployment.submit (deployment.dry-run per un dry-run) e, per una schedule
// ricorrente, anche schedule.write. Una schedule esistente con lo stesso nome ma di un'altra
// applicazione o ambiente richiede schedule.write anche su quella, perché verrebbe sovrascritta.
// Una deroga al freeze richiede deployment.freeze-override.
func (g *Guard) Submit(ctx context.Context, d *descriptor.Descriptor) (*engine.Submission, error) {
	t := target{application: d.Application, environment: d.Environment, dryRun: d.DryRun, comment: d.FreezeOverride}
	act := submitAction(DeploymentSubmit, d.DryRun)
	if err := g.check(ctx, act, t); err != nil {
		return nil, err
	}
	if err := g.checkOverride(ctx, d.FreezeOverride, t); err != nil {
		return nil, err
	}
	if d.Schedule != nil && d.Schedule.Cron != "" {
		t.schedule = d.ScheduleName()
		if err := g.check(ctx, ScheduleWrite, t); err != nil {
			return nil, err
		}
		if cur, err := g.Next.GetSchedule(ctx, t.schedule); err == nil && (cur.Application != d.Application || cur.Environment != d.Environment) {
			if err := g.check(ctx, ScheduleWrite, target{application: cur.Application, environment: cur.Environment, schedule: cur.Name}); err != nil {
				return nil, err
			}
		}
	}
	sub, err := g.Next.Submit(ctx, d)
	if sub != nil {
		t.deployment = sub.ID
	}
	g.done(ctx, act, t, err)
	return sub, err
}

// Status richiede deployment.read sull'applicazione/ambiente del deploy.
func (g *Guard) Status(ctx context.Context, id string) (*engine.Status, error) {
	st, err := g.Next.Status(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := g.check(ctx, DeploymentRead, target{application: st.Application, environment: st.Environment, deployment: id}); err != nil {
		return nil, err
	}
	return st, nil
}

// List restituisce solo i deploy che l'identità può leggere. Il filtro è applicato
// dall'engine durante la scansione, quindi l'elenco arriva comunque fino al limite richiesto.
func (g *Guard) List(ctx context.Context, f engine.ListFilter) ([]engine.Status, error) {
	p := auth.FromContext(ctx)
	match := f.Match
	f.Match = func(st engine.Status) bool {
		return g.Policy.Allowed(p, DeploymentRead, st.Application, st.Environment) && (match == nil || match(st))
	}
	all, err := g.Next.List(ctx, f)
	if err != nil {
		return nil, err
	}
	// Un servizio remoto ignora Match: il filtro viene riapplicato.
	out := all[:0]
	for _, st := range all {
		if f.Match(st) {
			out = append(out, st)
		}
	}
	return out, nil
}

// Wait richiede deployment.read.
func (g *Guard) Wait(ctx context.Context, id string) (*engine.Result, error) {
	if _, err := g.Status(ctx, id); err != nil {
		return nil, err
	}
	return g.Next.Wait(ctx, id)
}

// Cancel richiede deployment.cancel.
func (g *Guard) Cancel(ctx context.Context, id string) error {
	st, err := g.Next.Status(ctx, id)
	if err != nil {
		return err
	}
	t := target{application: st.Application, environment: st.Environment, deployment: id}
	if err := g.check(ctx, DeploymentCancel, t); err != nil {
		return err
	}
	err = g.Next.Cancel(ctx, id)
	g.done(ctx, DeploymentCancel, t, err)
	return err
}

// ListSchedules restituisce solo le schedule che l'identità può leggere.
func (g *Guard) ListSchedules(ctx context.Context) ([]engine.ScheduleInfo, error) {
	all, err := g.Next.ListSchedules(ctx)
	if err != nil {
		return nil, err
	}
	p := auth.FromContext(ctx)
	out := all[:0]
	for _, s := range all {
		if g.Policy.Allowed(p, ScheduleRead, s.Application, s.Environment) {
			out = append(out, s)
		}
	}
	return out, nil
}

// GetSchedule richiede schedule.read.
func (g *Guard) GetSchedule(ctx context.Context, name string) (*engine.ScheduleInfo, error) {
	s, err := g.Next.GetSchedule(ctx, name)
	if err != nil {
		return nil, err
	}
	if err := g.check(ctx, ScheduleRead, target{application: s.Application, environment: s.Environment, schedule: name}); err != nil {
		return nil, err
	}
	return s, nil
}

// PauseSchedule richiede schedule.write.
func (g *Guard) PauseSchedule(ctx context.Context, name string) error {
	return g.scheduleOp(ctx, name, "schedule.pause", g.Next.PauseSchedule)
}

// ResumeSchedule richiede schedule.write.
func (g *Guard) ResumeSchedule(ctx context.Context, name string) error {
	return g.scheduleOp(ctx, name, "schedule.resume", g.Next.ResumeSchedule)
}

// DeleteSchedule richiede schedule.write.
func (g *Guard) DeleteSchedule(ctx context.Context, name string) error {
	return g.scheduleOp(ctx, name, "schedule.delete", g.Next.DeleteSchedule)
}

// scheduleOp verifica schedule.write sulla schedule ed esegue l'operazione. Nell'audit
// l'azione riporta l'operazione specifica (es. schedule.pause).
func (g *Guard) scheduleOp(ctx context.Context, name string, auditAction Action, op func(context.Context, string) error) error {
	s, err := g.Next.GetSchedule(ctx, name)
	if err != nil {
		return err
	}
	t := target{application: s.Application, environment: s.Environment, schedule: name}
	if err := g.check(ctx, ScheduleWrite, t); err != nil {
		return err
	}
	err = op(ctx, name)
	g.done(ctx, auditAction, t, err)
	return err
}

// Inventory restituisce solo le applicazioni/ambienti che l'identità può leggere.
func (g *Guard) Inventory(ctx context.Context, f engine.InventoryFilter) ([]engine.InventoryEntry, error) {
	all, err := g.Next.Inventory(ctx, f)
	if err != nil {
		return nil, err
	}
	p := auth.FromContext(ctx)
	out := all[:0]
	for _, ie := range all {
		if g.Policy.Allowed(p, DeploymentRead, ie.Application, ie.Environment) {
			out = append(out, ie)
		}
	}
	return out, nil
}

// Promote richiede deployment.read sull'ambiente di origine e deployment.submit (o
// deployment.dry-run) su quello di destinazione.
func (g *Guard) Promote(ctx context.Context, r engine.PromoteRequest) (*engine.Submission, error) {
	if err := g.check(ctx, DeploymentRead, target{application: r.Application, environment: r.From}); err != nil {
		return nil, err
	}
	t := target{application: r.Application, environment: r.To, dryRun: r.DryRun, comment: r.FreezeOverride}
	if err := g.check(ctx, submitAction(DeploymentSubmit, r.DryRun), t); err != nil {
		return nil, err
	}
	if err := g.checkOverride(ctx, r.FreezeOverride, t); err != nil {
		return nil, err
	}
	sub, err := g.Next.Promote(ctx, r)
	if sub != nil {
		t.deployment = sub.ID
	}
	g.done(ctx, ActionPromote, t, err)
	return sub, err
}

// Rollback richiede deployment.rollback (o deployment.dry-run).
func (g *Guard) Rollback(ctx context.Context, r engine.RollbackRequest) (*engine.Submission, error) {
	t := target{application: r.Application, environment: r.Environment, dryRun: r.DryRun, comment: r.FreezeOverride}
	if err := g.check(ctx, submitAction(DeploymentRollback, r.DryRun), t); err != nil {
		return nil, err
	}
	if err := g.checkOverride(ctx, r.FreezeOverride, t); err != nil {
		return nil, err
	}
	sub, err := g.Next.Rollback(ctx, r)
	if sub != nil {
		t.deployment = sub.ID
	}
	g.done(ctx, DeploymentRollback, t, err)
	return sub, err
}

// Decide richiede deployment.approve sull'applicazione/ambiente del deploy. Chi ha richiesto
// il deploy non può approvarlo, salvo regola con allow_self.
func (g *Guard) Decide(ctx context.Context, id string, r engine.DecisionRequest) error {
	st, err := g.Next.Status(ctx, id)
	if err != nil {
		return err
	}
	act := ActionApprove
	if !r.Approve {
		act = ActionReject
	}
	t := target{application: st.Application, environment: st.Environment, deployment: id, comment: r.Comment}
	if err := g.check(ctx, DeploymentApprove, t); err != nil {
		return err
	}
	p := auth.FromContext(ctx)
	if r.Approve && st.RequestedBy == p.String() && (st.Approval == nil || !st.Approval.AllowSelf) {
		err := fmt.Errorf("%w: %s ha richiesto il deploy %s e non può approvarlo", engine.ErrForbidden, actor(p), id)
		g.record(ctx, act, t, audit.OutcomeDenied, err)
		return err
	}
	err = g.Next.Decide(ctx, id, r)
	g.done(ctx, act, t, err)
	return err
}

// Freezes richiede solo l'autenticazione: le finestre di freeze non sono riservate.
func (g *Guard) Freezes(ctx context.Context, application, environment string) ([]governance.WindowStatus, error) {
	return g.Next.Freezes(ctx, application, environment)
}

// Azioni di audit specifiche, oltre a quelle soggette a permesso.
const (
	ActionPromote Action = "deployment.promote"
	ActionApprove Action = "deployment.approve"
	ActionReject  Action = "deployment.reject"
)

// Steps richiede la lettura dell'applicazione/ambiente del deploy.
func (g *Guard) Steps(ctx context.Context, id string) ([]engine.StepRecord, error) {
	if _, err := g.Status(ctx, id); err != nil {
		return nil, err
	}
	return g.Next.Steps(ctx, id)
}

// Logs richiede la lettura dell'applicazione/ambiente del deploy.
func (g *Guard) Logs(ctx context.Context, id string, offset int) (*deploylog.Page, error) {
	if _, err := g.Status(ctx, id); err != nil {
		return nil, err
	}
	return g.Next.Logs(ctx, id, offset)
}

// DORA calcola le metriche solo sui deploy che l'identità può leggere.
func (g *Guard) DORA(ctx context.Context, f engine.DORAFilter) (*engine.DORAReport, error) {
	p := auth.FromContext(ctx)
	match := f.Match
	f.Match = func(st engine.Status) bool {
		return g.Policy.Allowed(p, DeploymentRead, st.Application, st.Environment) && (match == nil || match(st))
	}
	rep, err := g.Next.DORA(ctx, f)
	if err != nil {
		return nil, err
	}
	// Un servizio remoto ignora Match: gli ambienti non leggibili vengono rimossi.
	envs := rep.Environments[:0]
	for _, m := range rep.Environments {
		if g.Policy.Allowed(p, DeploymentRead, m.Application, m.Environment) {
			envs = append(envs, m)
		}
	}
	rep.Environments = envs
	return rep, nil
}

// Validate richiede la lettura dell'applicazione e dell'ambiente: mostra la configurazione
// effettiva, che può includere target e parametri dell'ambiente.
func (g *Guard) Validate(ctx context.Context, d *descriptor.Descriptor) (*engine.Validation, error) {
	if err := g.check(ctx, DeploymentRead, target{application: d.Application, environment: d.Environment}); err != nil {
		return nil, err
	}
	return g.Next.Validate(ctx, d)
}

// Me restituisce l'identità e i ruoli che le si applicano (binding che la riguardano), senza
// verificare permessi: ognuno può leggere i propri.
func (g *Guard) Me(ctx context.Context) (*engine.Identity, error) {
	id, err := g.Next.Me(ctx)
	if err != nil {
		return nil, err
	}
	id.AuthEnabled = !g.Policy.Disabled
	if g.Policy.Disabled {
		return id, nil
	}
	id.Grants = []engine.Grant{}
	p := auth.FromContext(ctx)
	if p == nil {
		return id, nil
	}
	for _, b := range g.Policy.Bindings {
		if !b.matches(p) {
			continue
		}
		acts := make([]string, len(g.Policy.Roles[b.Role]))
		for i, a := range g.Policy.Roles[b.Role] {
			acts[i] = string(a)
		}
		id.Grants = append(id.Grants, engine.Grant{Role: b.Role, Actions: acts, Applications: b.Applications, Environments: b.Environments})
	}
	return id, nil
}

// Approvals restituisce solo i deploy che l'identità può decidere: deployment.approve
// sull'applicazione/ambiente e, salvo regola con allow_self, non richiesti da lei.
func (g *Guard) Approvals(ctx context.Context) ([]engine.Status, error) {
	all, err := g.Next.Approvals(ctx)
	if err != nil {
		return nil, err
	}
	p := auth.FromContext(ctx)
	out := all[:0]
	for _, st := range all {
		if !g.Policy.Allowed(p, DeploymentApprove, st.Application, st.Environment) {
			continue
		}
		if !g.Policy.Disabled && st.RequestedBy == p.String() && (st.Approval == nil || !st.Approval.AllowSelf) {
			continue
		}
		out = append(out, st)
	}
	return out, nil
}
