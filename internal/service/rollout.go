package service

// Stati di un rollout, di uno stadio e di un passo.
const (
	RolloutRunning        = "running"
	RolloutPausedApproval = "paused-approval"
	RolloutPausedFreeze   = "paused-freeze"
	RolloutSucceeded      = "succeeded"
	RolloutFailed         = "failed"

	StagePending    = "pending"
	StageRunning    = "running"
	StageBaking     = "baking"
	StageSucceeded  = "succeeded"
	StageFailed     = "failed"
	StageRolledBack = "rolled-back"
)

// RolloutState è lo stato di un rilascio a ondate, pubblicato dal workflow a ogni cambiamento.
type RolloutState struct {
	Status string `json:"status"`
	// Stage è lo stadio corrente (o quello fallito).
	Stage string `json:"stage,omitempty"`
	// Paused spiega la pausa: approvazione dopo un'ondata o freeze di una region.
	Paused string         `json:"paused,omitempty"`
	Stages []RolloutStage `json:"stages"`
	// Approval è l'approvazione richiesta tra due ondate, se in corso o conclusa.
	Approval *ApprovalState `json:"approval,omitempty"`
	// OnFailure è il comportamento previsto se un passo fallisce.
	OnFailure string `json:"on_failure"`
	// Rollback è l'esito del rollback dei passi, se eseguito.
	Rollback string `json:"rollback,omitempty"`
}

// RolloutStage è uno stadio: i passi eseguiti insieme, poi bake e health gate.
type RolloutStage struct {
	Name   string        `json:"name"`
	Wave   string        `json:"wave"`
	State  string        `json:"state"`
	Passes []RolloutPass `json:"passes"`
	// Gate descrive l'ultimo health gate rosso, o è vuoto.
	Gate  string `json:"gate,omitempty"`
	Error string `json:"error,omitempty"`
}

// RolloutPass è il lavoro su una region (o un'AZ) dentro uno stadio.
type RolloutPass struct {
	Name   string   `json:"name"`
	Region string   `json:"region,omitempty"`
	AZ     string   `json:"az,omitempty"`
	Hosts  []string `json:"hosts"`
}

// Completed restituisce gli stadi superati.
func (r *RolloutState) Completed() []string {
	var out []string
	for _, s := range r.Stages {
		if s.State == StageSucceeded {
			out = append(out, s.Name)
		}
	}
	return out
}
