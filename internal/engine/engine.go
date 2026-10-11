// Package engine orchestra la pipeline di deploy come workflow durevole DBOS.
package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/google/uuid"

	"github.com/joeg-ita/manent/internal/auth"
	"github.com/joeg-ita/manent/internal/callback"
	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/governance"
	"github.com/joeg-ita/manent/internal/pipeline"
	"github.com/joeg-ita/manent/internal/rollout"
	"github.com/joeg-ita/manent/internal/service"
	"github.com/joeg-ita/manent/internal/telemetry"
)

// Nomi stabili con cui i workflow sono registrati in DBOS.
// Non vanno cambiati: i workflow in corso e le schedule li riferiscono per nome.
const (
	workflowName          = "manent.deploy"
	scheduledWorkflowName = "manent.scheduled-deploy"
	approvalWorkflowName  = "manent.approval"
)

// Request è l'input del workflow, persistito da DBOS.
type Request struct {
	Descriptor descriptor.Descriptor `json:"descriptor"`
	Plan       pipeline.Plan         `json:"plan"`
	// RequestedBy è l'identità che ha richiesto il deploy (o creato la schedule), es. "oidc:alice".
	RequestedBy string `json:"requested_by,omitempty"`
	// Kind è deploy, promotion o rollback (vuoto = deploy).
	Kind string `json:"kind,omitempty"`
	// Source è il deploy da cui deriva una promozione o un rollback manuale.
	Source string `json:"source,omitempty"`
	// Approval, se presente, sono le approvazioni richieste prima dell'esecuzione: la regola
	// è quella valida al momento della richiesta (o della creazione della schedule).
	Approval *governance.Approvals `json:"approval,omitempty"`
	// TraceParent è la traccia (W3C traceparent) della richiesta: le span del worker ne
	// diventano figlie. Vuoto per le esecuzioni delle schedule ricorrenti.
	TraceParent string `json:"trace_parent,omitempty"`
}

// ServiceOptions configura il servizio.
type ServiceOptions struct {
	// Client è il client DBOS: il contesto di un Worker oppure un client standalone (dbos.NewClient).
	Client    dbos.Client
	QueueName string
	Planner   pipeline.Planner
	// CallbackPolicy è applicata agli URL dei callback già all'accodamento, per rifiutare subito
	// un descrittore che il worker non potrebbe notificare.
	CallbackPolicy callback.Policy
	// Sources, se presente, rifiuta già all'accodamento una sorgente di artefatti non ammessa.
	// Il worker la verifica di nuovo al download.
	Sources SourceChecker
	// Rollback, se presente, rifiuta già all'accodamento un rollback non ammesso dalla policy.
	// La decisione definitiva è presa dal worker, dopo la fusione con la configurazione di ambiente.
	// Decide anche se un artefatto ammette il rollback manuale.
	Rollback RollbackPolicy
	// Resolver, se presente, verifica già all'accodamento la policy sui descrittori e la
	// compatibilità con la configurazione di ambiente. Il worker la verifica di nuovo.
	Resolver ConfigResolver
	// Governance, se presente, applica promozione obbligatoria, approvazioni e freeze.
	Governance Governance
	// Health, se presente, verifica che i controlli di salute dei rollout siano eseguibili.
	Health rollout.Checker
	Logger *slog.Logger
	// Now restituisce l'ora corrente; sostituibile nei test.
	Now func() time.Time
}

// SourceChecker verifica che la sorgente di un artefatto sia ammessa.
type SourceChecker interface {
	Check(source string) error
}

// Governance fornisce le regole di rilascio e le finestre di freeze.
type Governance interface {
	Rule(application, environment string) governance.Rule
	Frozen(application, environment string, t time.Time) *governance.WindowStatus
	// FrozenIn considera anche le finestre limitate a una region (passi di un rollout).
	FrozenIn(application, environment, region string, t time.Time) *governance.WindowStatus
	Windows(application, environment string, t time.Time) []governance.WindowStatus
}

// Engine implementa Service usando solo le API client di DBOS:
// accoda e interroga i workflow ma non li esegue (lo fanno i Worker).
type Engine struct {
	client     dbos.Client
	queueName  string
	planner    pipeline.Planner
	callbacks  callback.Policy
	rollback   RollbackPolicy
	sources    SourceChecker
	resolver   ConfigResolver
	governance Governance
	health     rollout.Checker
	logger     *slog.Logger
	now        func() time.Time
}

// NewService crea il servizio sopra un client DBOS.
func NewService(opts ServiceOptions) *Engine {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Engine{
		client:     opts.Client,
		queueName:  opts.QueueName,
		planner:    opts.Planner,
		callbacks:  opts.CallbackPolicy,
		rollback:   opts.Rollback,
		sources:    opts.Sources,
		resolver:   opts.Resolver,
		governance: opts.Governance,
		health:     opts.Health,
		logger:     opts.Logger.With("component", "engine"),
		now:        opts.Now,
	}
}

// Plan delega al planner configurato.
func (e *Engine) Plan(_ context.Context, d *descriptor.Descriptor) (pipeline.Plan, error) {
	return e.planner.Plan(d)
}

// submitMeta descrive l'origine di una richiesta di deploy.
type submitMeta struct {
	kind, source string
}

// Submit calcola il piano e, in base a schedule nel descrittore:
//   - nessuna schedule: accoda subito il deploy;
//   - schedule.at: accoda il deploy in stato DELAYED fino all'orario indicato;
//   - schedule.cron: crea o aggiorna la schedule ricorrente.
//
// Se le regole di governance richiedono approvazioni, il deploy resta in attesa
// (AWAITING_APPROVAL) e viene accodato solo dopo l'approvazione.
// Se il descrittore ha un ID, questo è usato come ID del workflow e rende la richiesta idempotente.
func (e *Engine) Submit(ctx context.Context, d *descriptor.Descriptor) (*Submission, error) {
	return e.submit(ctx, d, submitMeta{kind: KindDeploy})
}

func (e *Engine) submit(ctx context.Context, d *descriptor.Descriptor, meta submitMeta) (*Submission, error) {
	if sub, ok := e.alreadyScheduled(ctx, d); ok {
		return sub, nil
	}
	plan, err := e.planner.Plan(d)
	if err != nil {
		return nil, err
	}
	if err := e.check(ctx, d); err != nil {
		return nil, err
	}
	req := Request{
		Descriptor: *d, Plan: plan, RequestedBy: auth.FromContext(ctx).String(), Kind: meta.kind, Source: meta.source,
		TraceParent: telemetry.TraceParent(ctx),
	}
	if req.Approval, err = e.govern(ctx, d); err != nil {
		return nil, err
	}
	if d.Schedule != nil && d.Schedule.Cron != "" {
		return e.applySchedule(req)
	}

	sub := &Submission{State: string(dbos.WorkflowStatusEnqueued), DryRun: d.DryRun}
	var delay time.Duration
	if d.Schedule != nil && d.Schedule.At != nil {
		if delay = d.Schedule.At.Sub(e.now()); delay <= 0 {
			return nil, fmt.Errorf("%w: schedule.at %s è nel passato", ErrInvalidRequest, d.Schedule.At.Format(time.RFC3339))
		}
		sub.State = string(dbos.WorkflowStatusDelayed)
		sub.ScheduledAt = d.Schedule.At
	}
	var id string
	if d.ID != "" {
		id = workflowID(d)
	}
	if req.Approval != nil {
		// Il deploy non esiste ancora: l'ID va scelto subito, perché è quello da approvare.
		if id == "" {
			id = uuid.NewString()
		}
		if err := enqueueApproval(e.client, e.queueName, req, id); err != nil {
			return nil, err
		}
		sub.ID, sub.State = id, StateAwaitingApproval
		e.logger.Info("deploy in attesa di approvazione", "deployment", id, "application", d.Application, "environment", d.Environment, "required", req.Approval.Required, "requested_by", req.RequestedBy)
		return sub, nil
	}
	opts := deployOptions(req, id)
	if delay > 0 {
		opts = append(opts, dbos.WithEnqueueDelay(delay))
	}
	h, err := dbos.Enqueue[Result](e.client, e.queueName, workflowName, req, opts...)
	if err != nil {
		return nil, err
	}
	sub.ID = h.GetWorkflowID()
	e.logger.Info("deploy accodato", "deployment", sub.ID, "application", d.Application, "environment", d.Environment, "state", sub.State, "dry_run", d.DryRun, "kind", meta.kind, "requested_by", req.RequestedBy)
	return sub, nil
}

// alreadyScheduled rende idempotente il reinvio di un deploy con id e schedule.at dopo
// l'orario indicato (es. una CI che riprova): se il deploy esiste già per la stessa
// applicazione e lo stesso ambiente lo restituisce, invece di rifiutare l'orario come passato.
// Prima dell'orario ci pensa l'accodamento con lo stesso ID del workflow.
func (e *Engine) alreadyScheduled(ctx context.Context, d *descriptor.Descriptor) (*Submission, bool) {
	if d.ID == "" || d.DryRun || d.Schedule == nil || d.Schedule.At == nil || d.Schedule.At.After(e.now()) {
		return nil, false
	}
	st, err := e.Status(ctx, workflowID(d))
	if err != nil || st.Application != d.Application || st.Environment != d.Environment {
		return nil, false
	}
	return &Submission{ID: st.ID, State: st.State, ScheduledAt: d.Schedule.At}, true
}

// check rifiuta subito ciò che il worker non potrebbe eseguire: callback e sorgenti non
// ammessi, descrittore in violazione della policy o incompatibile con la configurazione di
// ambiente, rollback non ammesso. Se la configurazione di ambiente non è raggiungibile la
// verifica è lasciata al worker.
func (e *Engine) check(ctx context.Context, d *descriptor.Descriptor) error {
	v, err := e.inspect(ctx, d)
	if err != nil {
		return err
	}
	for _, w := range v.Warnings {
		e.logger.Warn(w, "application", d.Application, "environment", d.Environment)
	}
	return nil
}

// Validate verifica il descrittore come all'accodamento, senza accodarlo, e restituisce il
// piano e il deploy effettivo: configurazione di ambiente, rollback e regole di governance.
// Il client lo usa per "manent validate", perché non ha accesso alla configurazione di ambiente.
func (e *Engine) Validate(ctx context.Context, d *descriptor.Descriptor) (*Validation, error) {
	plan, err := e.planner.Plan(d)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	v, err := e.inspect(ctx, d)
	if err != nil {
		return nil, err
	}
	v.Plan = plan
	if e.governance != nil {
		rule := e.governance.Rule(d.Application, d.Environment)
		v.Governance = &GovernanceValidation{Rule: rule.Name, Requires: rule.Requires, Approvals: rule.Approvals.Required}
		at := e.now()
		if d.Schedule != nil && d.Schedule.At != nil {
			at = *d.Schedule.At
		}
		v.Governance.Freeze = e.governance.Frozen(d.Application, d.Environment, at)
	}
	return v, nil
}

// inspect esegue le verifiche di check e descrive il deploy effettivo. Un errore permanente
// rende la richiesta non valida; una configurazione di ambiente irraggiungibile produce solo
// un avviso.
func (e *Engine) inspect(ctx context.Context, d *descriptor.Descriptor) (*Validation, error) {
	for _, cb := range d.Callbacks {
		if err := e.callbacks.Check(cb.URL); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
		}
	}
	if e.sources != nil {
		if err := e.sources.Check(d.Artifact.Source); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
		}
	}
	v := &Validation{Valid: true}
	eff := *d
	if e.resolver != nil {
		r, meta, err := e.resolver.Resolve(ctx, *d)
		switch {
		case errors.Is(err, pipeline.ErrPermanent):
			return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
		case err != nil:
			v.Warnings = append(v.Warnings, "configurazione di ambiente non verificabile all'accodamento: la verifica resta al worker ("+err.Error()+")")
		default:
			eff, v.Config = r, meta
		}
	}
	if e.rollback != nil {
		r, err := e.rollback.Apply(eff)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
		}
		eff = r
	}
	if eff.Rollout != nil && v.Config != nil {
		// Con la configurazione di ambiente letta il piano a ondate è quello definitivo.
		p, err := checkRollout(eff, e.health)
		if err != nil {
			return nil, fmt.Errorf("%w: rollout: %w", ErrInvalidRequest, err)
		}
		v.Rollout = newRolloutState(p, eff.Rollout)
		v.Rollout.Status = "planned"
	}
	v.Deploy, v.Rollback = &eff.Deploy, eff.Rollback
	for _, sec := range eff.Secrets {
		v.Secrets = append(v.Secrets, sec.Name)
	}
	return v, nil
}

// govern applica le regole di governance a un deploy reale: freeze, promozione obbligatoria e
// approvazioni. Restituisce le approvazioni richieste, o nil. Il dry-run non è soggetto alle
// regole perché non modifica gli ambienti.
func (e *Engine) govern(ctx context.Context, d *descriptor.Descriptor) (*governance.Approvals, error) {
	if e.governance == nil || d.DryRun {
		return nil, nil
	}
	if d.FreezeOverride == "" && (d.Schedule == nil || d.Schedule.Cron == "") {
		at := e.now()
		if d.Schedule != nil && d.Schedule.At != nil {
			at = *d.Schedule.At
		}
		if w := e.governance.Frozen(d.Application, d.Environment, at); w != nil {
			return nil, fmt.Errorf("%w: %s", ErrInvalidRequest, freezeMessage(w))
		}
	}
	rule := e.governance.Rule(d.Application, d.Environment)
	for _, env := range rule.Requires {
		rel, err := e.findRelease(ctx, d.Application, env, func(s Status) bool { return sameArtifact(s, d) })
		if err != nil {
			return nil, err
		}
		if rel == nil {
			return nil, fmt.Errorf("%w: %s %s non risulta rilasciato con successo in %s: va promosso da lì (governance)", ErrInvalidRequest, d.Artifact.Name, d.Artifact.Version, env)
		}
	}
	if rule.Approvals.Required > 0 {
		a := rule.Approvals
		return &a, nil
	}
	return nil, nil
}

// freezeMessage descrive il freeze che blocca un deploy.
func freezeMessage(w *governance.WindowStatus) string {
	msg := fmt.Sprintf("deploy bloccato dal freeze %q fino a %s", w.Name, w.End.Format(time.RFC3339))
	if w.Reason != "" {
		msg += " (" + w.Reason + ")"
	}
	return msg + "; per un'emergenza indicare freezeOverride con il motivo"
}

// sameArtifact indica se il deploy registrato ha rilasciato lo stesso artefatto del descrittore:
// stesso nome e versione e, se entrambi noti, stesso sha256.
func sameArtifact(s Status, d *descriptor.Descriptor) bool {
	if s.Artifact != d.Artifact.Name || s.Version != d.Artifact.Version {
		return false
	}
	return d.Artifact.SHA256 == "" || s.SHA256 == "" || strings.EqualFold(s.SHA256, d.Artifact.SHA256)
}

// deployOptions sono le opzioni di accodamento di un deploy: partizione della coda, utente,
// attributi per elenchi e inventario e, se indicato, ID del workflow.
func deployOptions(req Request, id string) []dbos.EnqueueOption {
	opts := []dbos.EnqueueOption{
		dbos.WithEnqueueQueuePartitionKey(partitionKey(&req.Descriptor)),
		dbos.WithEnqueueAttributes(attributes(req)),
	}
	if req.RequestedBy != "" {
		opts = append(opts, dbos.WithEnqueueAuthenticatedUser(req.RequestedBy))
	}
	if id != "" {
		opts = append(opts, dbos.WithEnqueueWorkflowID(id))
	}
	return opts
}

// Chiavi degli attributi dei workflow. Sono registrati alla creazione e aggiornati al termine
// del deploy (esito, sha256, rollback): permettono elenchi e inventario senza caricare l'input.
const (
	attrApplication = "application"
	attrEnvironment = "environment"
	attrArtifact    = "artifact"
	attrVersion     = "version"
	attrKind        = "kind"
	attrSource      = "source"
	attrDryRun      = "dry_run"
	attrApproval    = "approval"
	attrOutcome     = "outcome"
	attrSHA256      = "sha256"
	attrRollback    = "rollback"
	// attrRolloutProgress riassume un rollout a ondate: "stadi superati/stadi totali".
	attrRolloutProgress = "rollout_progress"
)

// attributes restituisce gli attributi iniziali di un workflow.
func attributes(req Request) map[string]any {
	d := req.Descriptor
	a := map[string]any{
		attrApplication: d.Application,
		attrEnvironment: d.Environment,
		attrArtifact:    d.Artifact.Name,
		attrVersion:     d.Artifact.Version,
		attrKind:        cmp.Or(req.Kind, KindDeploy),
	}
	if req.Source != "" {
		a[attrSource] = req.Source
	}
	if d.DryRun {
		a[attrDryRun] = "true"
	}
	if req.Approval != nil {
		a[attrApproval] = "required"
	}
	return a
}

// Status legge lo stato del workflow dal database di sistema DBOS. Un deploy in attesa di
// approvazione non ha ancora un workflow: lo stato è quello dell'approvazione.
func (e *Engine) Status(_ context.Context, id string) (*Status, error) {
	h, err := dbos.RetrieveWorkflow[Result](e.client, id)
	if err != nil {
		return e.approvalStatus(id)
	}
	st, err := h.GetStatus()
	if err != nil {
		return nil, err
	}
	s := toStatus(st)
	if rs, err := dbos.GetEvent[RolloutState](e.client, id, rolloutEvent, 0); err == nil && rs.Status != "" {
		s.Rollout = &rs
		if rs.Status == service.RolloutPausedApproval && st.Status == dbos.WorkflowStatusPending {
			// Approvazione tra due ondate: il deploy è in corso ma attende una decisione.
			s.State, s.Approval = StateAwaitingApproval, rs.Approval
		}
	}
	if st.Attributes[attrApproval] != nil && s.Approval == nil {
		if a, err := e.approval(id + approvalSuffix); err == nil {
			s.Approval = a
		}
	}
	switch st.Status {
	case dbos.WorkflowStatusSuccess:
		res, err := h.GetResult()
		if err != nil {
			return nil, err
		}
		s.Result = &res
	case dbos.WorkflowStatusError:
		// Con un client DBOS lo stato non include l'errore, registrato a parte con l'esito.
		if s.Error == "" {
			if _, err := h.GetResult(); err != nil {
				s.Error = err.Error()
			}
		}
	}
	return &s, nil
}

// maxListScan limita i deploy esaminati da un elenco filtrato o dall'inventario.
const maxListScan = 10000

// List elenca i deploy dal più recente, compresi quelli in attesa di approvazione o respinti.
// Con f.Schedule restituisce solo le esecuzioni di quella schedule. I filtri sono applicati
// pagina per pagina, finché non si raggiunge il limite.
func (e *Engine) List(_ context.Context, f ListFilter) ([]Status, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	var prefix []string
	if f.Schedule != "" {
		prefix = []string{scheduledIDPrefix(f.Schedule)}
	}
	out := []Status{}
	// Con un cursore l'elenco riparte dal deploy successivo a quello indicato.
	skipping := f.Cursor != ""
	err := e.scan(prefix, func(s Status) bool {
		if skipping {
			skipping = s.ID != f.Cursor
			return true
		}
		if f.Matches(s) {
			out = append(out, s)
		}
		return len(out) < f.Limit
	})
	return out, err
}

// scan scorre deploy e approvazioni dal più recente, a pagine, finché fn restituisce true.
// Le approvazioni concluse con un deploy non sono riportate: il deploy le rappresenta.
func (e *Engine) scan(prefix []string, fn func(Status) bool) error {
	const page = 200
	for offset := 0; offset < maxListScan; offset += page {
		opts := []dbos.ListWorkflowsOption{
			dbos.WithFilterName(workflowName, approvalWorkflowName),
			dbos.WithFilterSortDesc(),
			dbos.WithFilterLimit(page),
			dbos.WithFilterOffset(offset),
			dbos.WithFilterLoadInput(false),
			dbos.WithFilterLoadOutput(false),
		}
		if len(prefix) > 0 {
			opts = append(opts, dbos.WithFilterWorkflowIDPrefix(prefix...))
		}
		wfs, err := dbos.ListWorkflows(e.client, opts...)
		if err != nil {
			return err
		}
		for _, w := range wfs {
			if w.Name == approvalWorkflowName && attr(w, attrOutcome) == ApprovalApproved {
				continue
			}
			if !fn(toStatus(w)) {
				return nil
			}
		}
		if len(wfs) < page {
			return nil
		}
	}
	return nil
}

// Wait blocca fino alla conclusione del workflow, o fino alla scadenza di ctx se ne ha una.
// Il deploy viene eseguito da un Worker: senza worker attivi resta in coda. Per un deploy in
// attesa di approvazione attende prima la decisione.
func (e *Engine) Wait(ctx context.Context, id string) (*Result, error) {
	var opts []dbos.GetResultOption
	if deadline, ok := ctx.Deadline(); ok {
		opts = append(opts, dbos.WithHandleTimeout(time.Until(deadline)))
	}
	h, err := dbos.RetrieveWorkflow[Result](e.client, id)
	if err != nil {
		ah, aerr := dbos.RetrieveWorkflow[ApprovalState](e.client, id+approvalSuffix)
		if aerr != nil {
			return nil, fmt.Errorf("%w: deploy %s", ErrNotFound, id)
		}
		a, err := ah.GetResult(opts...)
		if err != nil {
			return nil, err
		}
		if a.Status != ApprovalApproved {
			return nil, fmt.Errorf("deploy %s non eseguito: approvazione %s", id, a.Status)
		}
		if h, err = dbos.RetrieveWorkflow[Result](e.client, id); err != nil {
			return nil, fmt.Errorf("%w: deploy %s", ErrNotFound, id)
		}
	}
	res, err := h.GetResult(opts...)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// Cancel annulla il deploy: se è in attesa viene rimosso dalla coda, se è in esecuzione si
// interrompe all'inizio dello step successivo, se attende un'approvazione non verrà eseguito.
func (e *Engine) Cancel(ctx context.Context, id string) error {
	st, err := e.Status(ctx, id)
	if err != nil {
		return err
	}
	target := id
	if st.State == StateAwaitingApproval {
		target = id + approvalSuffix
	}
	if err := dbos.CancelWorkflow(e.client, target); err != nil {
		return err
	}
	e.logger.Info("deploy annullato", "deployment", id)
	return nil
}

// toStatus converte lo stato DBOS nel formato esposto da manent.
// Gli attributi permettono di applicare i permessi e di costruire l'inventario senza caricare
// l'input del workflow.
func toStatus(st dbos.WorkflowStatus) Status {
	s := Status{
		ID: st.ID, State: string(st.Status), CreatedAt: st.CreatedAt, UpdatedAt: st.UpdatedAt, RequestedBy: st.AuthenticatedUser,
		Application: attr(st, attrApplication), Environment: attr(st, attrEnvironment),
		Artifact: attr(st, attrArtifact), Version: attr(st, attrVersion), SHA256: attr(st, attrSHA256),
		Kind: attr(st, attrKind), Source: attr(st, attrSource), DryRun: attr(st, attrDryRun) == "true",
		Rollback: attr(st, attrRollback), RolloutProgress: attr(st, attrRolloutProgress),
	}
	if s.Application == "" {
		// Deploy accodati prima degli attributi: la chiave di partizione è "<application>/<environment>".
		s.Application, s.Environment, _ = strings.Cut(st.QueuePartitionKey, "/")
	}
	if st.Name == approvalWorkflowName {
		s.ID = strings.TrimSuffix(st.ID, approvalSuffix)
		s.State = approvalWorkflowState(st.Status, attr(st, attrOutcome))
		s.Error = ""
		return s
	}
	if !st.DelayUntil.IsZero() {
		at := st.DelayUntil
		s.ScheduledAt = &at
	}
	if st.Status == dbos.WorkflowStatusError && st.Error != nil {
		s.Error = st.Error.Error()
	}
	return s
}

// attr restituisce un attributo stringa del workflow.
func attr(st dbos.WorkflowStatus, key string) string {
	v, _ := st.Attributes[key].(string)
	return v
}

// dryRunSuffix distingue l'ID del workflow di un dry-run da quello del deploy reale.
const dryRunSuffix = ".dry-run"

// workflowID restituisce l'ID del workflow per un descrittore con ID. Un dry-run usa un ID
// distinto: altrimenti, per l'idempotenza, il deploy reale con lo stesso ID non verrebbe mai eseguito.
func workflowID(d *descriptor.Descriptor) string {
	if d.DryRun {
		return d.ID + dryRunSuffix
	}
	return d.ID
}

// partitionKey è la chiave di partizione della coda: i deploy della stessa
// application/environment rispettano il limite environment_concurrency su tutti i worker.
func partitionKey(d *descriptor.Descriptor) string {
	return d.Application + "/" + d.Environment
}
