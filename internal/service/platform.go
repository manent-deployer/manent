package service

import (
	"context"
	"time"
)

// PlatformService espone lo stato della piattaforma: i worker registrati con il lavoro che
// stanno svolgendo e i deploy attivi (in corso, in coda, pianificati, in attesa di approvazione).
type PlatformService interface {
	// Workers elenca i worker registrati e quelli con workflow in sospeso.
	Workers(ctx context.Context) ([]WorkerInfo, error)
	// Activity restituisce i deploy non ancora conclusi, raggruppati per stato.
	Activity(ctx context.Context, f ActivityFilter) (*Activity, error)
}

// Stati di un worker.
const (
	// WorkerAlive indica un worker che invia heartbeat.
	WorkerAlive = "alive"
	// WorkerStale indica un worker che non invia heartbeat: è caduto o irraggiungibile.
	WorkerStale = "stale"
	// WorkerStopped indica un worker arrestato in modo ordinato.
	WorkerStopped = "stopped"
	// WorkerUnregistered indica un executor con workflow in sospeso ma assente dal registro.
	WorkerUnregistered = "unregistered"
)

// WorkerInfo è un worker (executor DBOS) con il lavoro assegnato.
type WorkerInfo struct {
	ID string `json:"id"`
	// State è alive, stale, stopped o unregistered.
	State string `json:"state"`
	// Version è la versione dei workflow (app.version): un worker riprende solo i propri.
	Version  string `json:"version,omitempty"`
	Build    string `json:"build,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	// Region è la region del worker (dbos.region), se configurata.
	Region      string     `json:"region,omitempty"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	HeartbeatAt *time.Time `json:"heartbeat_at,omitempty"`
	StoppedAt   *time.Time `json:"stopped_at,omitempty"`
	// RecoveredBy è chi ha ripreso i workflow del worker dopo la sua perdita.
	RecoveredBy string     `json:"recovered_by,omitempty"`
	RecoveredAt *time.Time `json:"recovered_at,omitempty"`
	// Pending conta i workflow in sospeso del worker per versione dei workflow.
	Pending map[string]int64 `json:"pending,omitempty"`
	// Tasks sono i workflow in esecuzione sul worker che l'identità può leggere.
	Tasks []WorkerTask `json:"tasks"`
	// HiddenTasks conta i workflow in esecuzione di applicazioni/ambienti non leggibili.
	HiddenTasks int `json:"hidden_tasks,omitempty"`
}

// Tipi di lavoro di un worker.
const (
	TaskDeploy   = "deploy"
	TaskPass     = "pass"
	TaskGate     = "gate"
	TaskApproval = "approval"
	TaskSchedule = "schedule"
)

// WorkerTask è un workflow in esecuzione su un worker.
type WorkerTask struct {
	// ID è l'ID del workflow; DeploymentID il deploy a cui appartiene.
	ID           string `json:"id"`
	DeploymentID string `json:"deployment_id"`
	// Kind è deploy, pass (passo di un rollout in una region), gate, approval o schedule.
	Kind        string     `json:"kind"`
	Application string     `json:"application,omitempty"`
	Environment string     `json:"environment,omitempty"`
	Region      string     `json:"region,omitempty"`
	Version     string     `json:"version,omitempty"`
	Queue       string     `json:"queue,omitempty"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	// Attempts è il numero di esecuzioni del workflow: più di 1 dopo una recovery.
	Attempts int `json:"attempts,omitempty"`
}

// ActivityFilter filtra i deploy attivi.
type ActivityFilter struct {
	Application string `json:"application,omitempty"`
	Environment string `json:"environment,omitempty"`
	// Match, se presente, è un filtro aggiuntivo (es. i permessi), applicato prima di leggere
	// i dettagli dei deploy in esecuzione.
	Match func(Status) bool `json:"-"`
}

// Activity sono i deploy non conclusi, dal più recente.
type Activity struct {
	// Running sono i deploy in esecuzione, con worker e ultimo step registrato.
	Running []Status `json:"running"`
	// AwaitingApproval sono i deploy in attesa di una decisione, prima del deploy o tra due ondate.
	AwaitingApproval []Status `json:"awaiting_approval"`
	// Queued sono i deploy in coda, in attesa di uno slot libero.
	Queued []Status `json:"queued"`
	// Delayed sono i deploy pianificati con schedule.at.
	Delayed []Status `json:"delayed"`
	// Truncated indica che i deploy attivi erano più di quelli esaminati.
	Truncated bool `json:"truncated,omitempty"`
}
