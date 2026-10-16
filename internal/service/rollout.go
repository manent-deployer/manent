package service

// States of a rollout, a stage and a pass.
const (
	RolloutRunning        = "running"
	RolloutPausedApproval = "paused-approval"
	RolloutPausedFreeze   = "paused-freeze"
	// RolloutPausedDrift means router weights differ from those set by manent.
	RolloutPausedDrift = "paused-drift"
	RolloutSucceeded   = "succeeded"
	RolloutFailed      = "failed"

	StagePending    = "pending"
	StageRunning    = "running"
	StageBaking     = "baking"
	StageSucceeded  = "succeeded"
	StageFailed     = "failed"
	StageRolledBack = "rolled-back"
)

// RolloutState is the state of a wave rollout, published by the workflow at every change.
type RolloutState struct {
	// Strategy is waves (empty) or traffic.
	Strategy string `json:"strategy,omitempty"`
	Status   string `json:"status"`
	// Stage is the current stage (or the failed one).
	Stage string `json:"stage,omitempty"`
	// Paused explains the pause: approval after a wave or freeze of a region.
	Paused string         `json:"paused,omitempty"`
	Stages []RolloutStage `json:"stages"`
	// Approval is the approval requested between two waves, if running or ended.
	Approval *ApprovalState `json:"approval,omitempty"`
	// OnFailure is the behavior configured for a failed pass.
	OnFailure string `json:"on_failure"`
	// Rollback is the outcome of the passes' rollback, if run.
	Rollback string `json:"rollback,omitempty"`
	// Traffic is the state of the percentage rollout (strategy: traffic).
	Traffic *TrafficState `json:"traffic,omitempty"`
}

// TrafficState is the state of a percentage rollout: stages are the steps.
type TrafficState struct {
	// Pass is the running pass in percentage rollouts in waves.
	Pass string `json:"pass,omitempty"`
	// Router is the router (environment provider or deploy plugin).
	Router string `json:"router,omitempty"`
	// Stable and Candidate are the slots of the stable and candidate versions, if known.
	Stable    string `json:"stable,omitempty"`
	Candidate string `json:"candidate,omitempty"`
	// Weight is the candidate's current traffic share.
	Weight int   `json:"weight"`
	Steps  []int `json:"steps"`
	// Testers means the testers' requests are already routed to the candidate.
	Testers bool `json:"testers,omitempty"`
	// Promoted means the candidate has become the version in service.
	Promoted bool `json:"promoted,omitempty"`
}

// RolloutStage is a stage: the passes run together, then bake and health gate.
type RolloutStage struct {
	Name   string        `json:"name"`
	Wave   string        `json:"wave"`
	State  string        `json:"state"`
	Passes []RolloutPass `json:"passes"`
	// Gate describes the last red health gate, or is empty.
	Gate string `json:"gate,omitempty"`
	// Score is the canary analysis score of the last gate read (rollout.health.analysis).
	Score *float64 `json:"score,omitempty"`
	Error string   `json:"error,omitempty"`
}

// RolloutPass is the work on a region (or an AZ) within a stage.
type RolloutPass struct {
	Name   string   `json:"name"`
	Region string   `json:"region,omitempty"`
	AZ     string   `json:"az,omitempty"`
	Hosts  []string `json:"hosts"`
	// Drained are the pool members out of the load balancer right now.
	Drained []string `json:"drained,omitempty"`
}

// Completed returns the stages passed.
func (r *RolloutState) Completed() []string {
	var out []string
	for _, s := range r.Stages {
		if s.State == StageSucceeded {
			out = append(out, s.Name)
		}
	}
	return out
}
