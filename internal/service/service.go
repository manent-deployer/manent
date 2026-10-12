// Package service è il contratto tra i client e il server di manent: interfacce del servizio,
// richieste, risposte ed errori. Non dipende da DBOS né dal database, così il client
// (cmd/manent, internal/apiclient) resta leggero: lo implementano engine.Engine sul server e
// apiclient.Client sopra l'API HTTP.
package service

import (
	"context"
	"errors"
	"time"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/governance"
	"github.com/joeg-ita/manent/internal/pipeline"
)

// Stati di una Submission e di un deploy oltre a quelli dei workflow.
const (
	// StateScheduled indica una schedule ricorrente creata o aggiornata.
	StateScheduled = "SCHEDULED"
	// StateAwaitingApproval indica un deploy in attesa delle approvazioni richieste.
	StateAwaitingApproval = "AWAITING_APPROVAL"
	// StateRejected indica un deploy respinto da un approvatore.
	StateRejected = "REJECTED"
	// StateExpired indica un deploy non approvato entro il timeout.
	StateExpired = "EXPIRED"
)

// Stati dei workflow esposti dal servizio (gli stessi di DBOS).
const (
	StatePending   = "PENDING"
	StateEnqueued  = "ENQUEUED"
	StateDelayed   = "DELAYED"
	StateSuccess   = "SUCCESS"
	StateError     = "ERROR"
	StateCancelled = "CANCELLED"
	// StateMaxRecoveryAttempts indica un workflow non più ripreso dopo troppi tentativi.
	StateMaxRecoveryAttempts = "MAX_RECOVERY_ATTEMPTS_EXCEEDED"
)

// Terminal indica gli stati finali di un deploy.
func Terminal(state string) bool {
	switch state {
	case StateSuccess, StateError, StateCancelled, StateMaxRecoveryAttempts, StateRejected, StateExpired:
		return true
	}
	return false
}

// Tipi di deploy, registrati nello stato e nell'inventario.
const (
	KindDeploy    = "deploy"
	KindPromotion = "promotion"
	KindRollback  = "rollback"
)

var (
	// ErrNotFound indica un deploy o una schedule inesistente.
	ErrNotFound = errors.New("non trovato")
	// ErrInvalidRequest indica una richiesta non accettabile (es. orario nel passato).
	ErrInvalidRequest = errors.New("richiesta non valida")
	// ErrForbidden indica un'azione non consentita dai permessi dell'utente.
	ErrForbidden = errors.New("permesso negato")
)

// Service è il punto di ingresso usato da CLI e API.
type Service interface {
	DeploymentService
	ScheduleService
	GovernanceService
	InsightService
	AccessService
	PlatformService
}

// DeploymentService gestisce i deploy, immediati o pianificati.
type DeploymentService interface {
	// Plan calcola gli step che verrebbero eseguiti, senza avviare il deploy.
	Plan(ctx context.Context, d *descriptor.Descriptor) (pipeline.Plan, error)
	// Validate verifica il descrittore come all'accodamento e restituisce il deploy effettivo,
	// senza avviarlo.
	Validate(ctx context.Context, d *descriptor.Descriptor) (*Validation, error)
	// Submit accoda un deploy, lo pianifica a un orario o crea una schedule ricorrente.
	Submit(ctx context.Context, d *descriptor.Descriptor) (*Submission, error)
	// Status restituisce lo stato corrente di un deploy.
	Status(ctx context.Context, id string) (*Status, error)
	// List elenca i deploy più recenti, opzionalmente solo quelli di una schedule.
	List(ctx context.Context, f ListFilter) ([]Status, error)
	// Wait attende la conclusione di un deploy e ne restituisce il risultato.
	Wait(ctx context.Context, id string) (*Result, error)
	// Cancel annulla un deploy in attesa o in esecuzione.
	Cancel(ctx context.Context, id string) error
}

// ScheduleService gestisce le schedule dei deploy ricorrenti.
type ScheduleService interface {
	ListSchedules(ctx context.Context) ([]ScheduleInfo, error)
	GetSchedule(ctx context.Context, name string) (*ScheduleInfo, error)
	PauseSchedule(ctx context.Context, name string) error
	ResumeSchedule(ctx context.Context, name string) error
	DeleteSchedule(ctx context.Context, name string) error
}

// GovernanceService gestisce inventario, promozioni, rollback manuali, approvazioni e freeze.
type GovernanceService interface {
	// Inventory restituisce la release in esercizio in ogni applicazione/ambiente.
	Inventory(ctx context.Context, f InventoryFilter) ([]InventoryEntry, error)
	// Promote rilascia in un ambiente lo stesso artefatto già rilasciato in un altro.
	Promote(ctx context.Context, r PromoteRequest) (*Submission, error)
	// Rollback rilascia di nuovo una versione precedente.
	Rollback(ctx context.Context, r RollbackRequest) (*Submission, error)
	// Decide approva o respinge un deploy in attesa di approvazione.
	Decide(ctx context.Context, id string, r DecisionRequest) error
	// Freezes elenca le finestre di freeze attive e future.
	Freezes(ctx context.Context, application, environment string) ([]governance.WindowStatus, error)
	// Approvals elenca i deploy in attesa di una decisione: prima del deploy o tra due ondate.
	// Attraverso i permessi, solo quelli che l'identità può approvare.
	Approvals(ctx context.Context) ([]Status, error)
}

// InsightService espone timeline, log e metriche DORA dei deploy.
type InsightService interface {
	// Steps restituisce gli step registrati di un deploy (o della sua approvazione, se il
	// deploy non è ancora partito), con orari, output ed errori.
	Steps(ctx context.Context, id string) ([]StepRecord, error)
	// Logs restituisce le righe di log del deploy a partire da offset.
	Logs(ctx context.Context, id string, offset int) (*LogPage, error)
	// DORA calcola le metriche DORA dallo storico dei deploy.
	DORA(ctx context.Context, f DORAFilter) (*DORAReport, error)
}

// Validation è l'esito di Validate: il piano e il deploy effettivo, dopo la fusione con la
// configurazione di ambiente e l'applicazione delle policy.
type Validation struct {
	Valid    bool                 `json:"valid"`
	Plan     pipeline.Plan        `json:"plan"`
	Rollback *descriptor.Rollback `json:"rollback,omitempty"`
	Deploy   *descriptor.Deploy   `json:"deploy,omitempty"`
	// Secrets sono i nomi dei segreti dichiarati, mai i riferimenti né i valori.
	Secrets []string `json:"secrets,omitempty"`
	// Config descrive la configurazione di ambiente usata (sorgente e revisione).
	Config     map[string]string     `json:"config,omitempty"`
	Governance *GovernanceValidation `json:"governance,omitempty"`
	// Rollout è il piano a ondate (stadi e passi) che verrebbe eseguito.
	Rollout *RolloutState `json:"rollout,omitempty"`
	// Warnings segnala le verifiche non eseguite, ad esempio con Consul irraggiungibile.
	Warnings []string `json:"warnings,omitempty"`
}

// GovernanceValidation sono le regole di governance che si applicherebbero al deploy.
type GovernanceValidation struct {
	Rule      string                   `json:"rule,omitempty"`
	Requires  []string                 `json:"requires,omitempty"`
	Approvals int                      `json:"approvals"`
	Freeze    *governance.WindowStatus `json:"freeze,omitempty"`
}

// StepResult è l'esito di uno step eseguito.
type StepResult struct {
	Name   string          `json:"name"`
	Output pipeline.Output `json:"output"`
}

// Result è l'output del workflow di deploy.
type Result struct {
	DeploymentID string `json:"deployment_id"`
	// DryRun indica una simulazione: nessuna modifica agli ambienti di esercizio.
	DryRun bool         `json:"dry_run,omitempty"`
	Steps  []StepResult `json:"steps"`
	// Rollout è lo stato finale del rilascio a ondate, se il deploy ne ha uno.
	Rollout *RolloutState `json:"rollout,omitempty"`
}

// Submission descrive l'esito di Submit.
type Submission struct {
	// ID del deploy; vuoto per le schedule ricorrenti.
	ID string `json:"id,omitempty"`
	// State è ENQUEUED, DELAYED (deploy singolo pianificato) o SCHEDULED (ricorrente).
	State       string     `json:"state"`
	ScheduledAt *time.Time `json:"scheduled_at,omitempty"`
	Schedule    string     `json:"schedule,omitempty"`
	DryRun      bool       `json:"dry_run,omitempty"`
}

// Status descrive lo stato di un deploy.
type Status struct {
	ID          string `json:"id"`
	State       string `json:"state"`
	Application string `json:"application,omitempty"`
	Environment string `json:"environment,omitempty"`
	Artifact    string `json:"artifact,omitempty"`
	Version     string `json:"version,omitempty"`
	// SHA256 è l'hash dell'artefatto rilasciato, registrato al termine del deploy.
	SHA256 string `json:"sha256,omitempty"`
	Kind   string `json:"kind,omitempty"`
	Source string `json:"source,omitempty"`
	DryRun bool   `json:"dry_run,omitempty"`
	// Rollback è l'esito del rollback automatico, se eseguito.
	Rollback    string     `json:"rollback,omitempty"`
	RequestedBy string     `json:"requested_by,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	ScheduledAt *time.Time `json:"scheduled_at,omitempty"`
	// StartedAt è l'inizio dell'esecuzione sul worker (assente finché il deploy è in coda).
	StartedAt *time.Time `json:"started_at,omitempty"`
	// Executor è il worker che esegue o ha eseguito il deploy; Queue la coda.
	Executor string `json:"executor,omitempty"`
	Queue    string `json:"queue,omitempty"`
	// LastStep è l'ultimo step registrato di un deploy in corso (solo nell'attività).
	LastStep string         `json:"last_step,omitempty"`
	Approval *ApprovalState `json:"approval,omitempty"`
	// Rollout è lo stato del rilascio a ondate, se il deploy ne ha uno.
	Rollout *RolloutState `json:"rollout,omitempty"`
	// RolloutProgress riassume il rollout negli elenchi: "stadi superati/stadi totali".
	RolloutProgress string  `json:"rollout_progress,omitempty"`
	Result          *Result `json:"result,omitempty"`
	Error           string  `json:"error,omitempty"`
}

// ListFilter filtra l'elenco dei deploy.
type ListFilter struct {
	Schedule    string `json:"schedule,omitempty"`
	Application string `json:"application,omitempty"`
	Environment string `json:"environment,omitempty"`
	// State filtra per stato (es. SUCCESS, ERROR, AWAITING_APPROVAL).
	State string `json:"state,omitempty"`
	Limit int    `json:"limit,omitempty"`
	// Cursor è l'ID dell'ultimo deploy della pagina precedente: l'elenco riparte dal successivo.
	Cursor string `json:"cursor,omitempty"`
	// Match, se presente, è un filtro aggiuntivo (es. i permessi): l'elenco prosegue sui deploy
	// più vecchi finché non raggiunge Limit, quindi il filtro non riduce il numero di risultati.
	Match func(Status) bool `json:"-"`
}

// Matches applica i filtri a uno stato.
func (f ListFilter) Matches(s Status) bool {
	return (f.Application == "" || s.Application == f.Application) &&
		(f.Environment == "" || s.Environment == f.Environment) &&
		(f.State == "" || s.State == f.State) &&
		(f.Match == nil || f.Match(s))
}

// ScheduleInfo descrive una schedule ricorrente.
type ScheduleInfo struct {
	Name        string `json:"name"`
	Cron        string `json:"cron"`
	Timezone    string `json:"timezone,omitempty"`
	Status      string `json:"status"`
	Application string `json:"application"`
	Environment string `json:"environment"`
	// RequestedBy è chi ha creato o aggiornato per ultimo la schedule.
	RequestedBy string     `json:"requested_by,omitempty"`
	LastFiredAt *time.Time `json:"last_fired_at,omitempty"`
}
