package service

import (
	"context"
	"time"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/governance"
)

// PolicyService manages the governance policy revisions and the freeze windows created at
// runtime, without restarting manent.
type PolicyService interface {
	// CreateFreeze creates a freeze window (freeze.write on its applications and environments).
	CreateFreeze(ctx context.Context, r FreezeRequest) (*governance.WindowStatus, error)
	// DeleteFreeze deletes a freeze window created at runtime.
	DeleteFreeze(ctx context.Context, name string) error
	// Policies lists the policy revisions, newest first, without their files.
	Policies(ctx context.Context) (*PolicyList, error)
	// PolicyRevision returns a revision with its files.
	PolicyRevision(ctx context.Context, revision int64) (*PolicyRevision, error)
	// PushPolicy checks (compilation, tests, sample evaluations) and stores a new draft revision.
	PushPolicy(ctx context.Context, r PolicyPush) (*PolicyRevision, error)
	// EvaluatePolicy evaluates a revision (0 = the active one) on a descriptor.
	EvaluatePolicy(ctx context.Context, revision int64, r PolicyEvaluationRequest) (*PolicyEvaluation, error)
	// PolicyImpact recomputes with a revision the decisions of the real deploys of a period.
	PolicyImpact(ctx context.Context, revision int64, since time.Duration) (*PolicyImpact, error)
	// ApprovePolicy activates a draft revision (by an identity other than its author when
	// governance.opa.store.review is set).
	ApprovePolicy(ctx context.Context, revision int64) error
	// ActivatePolicy goes back to a revision already active in the past.
	ActivatePolicy(ctx context.Context, revision int64) error
}

// FreezeRequest asks to create a freeze window at runtime: absolute (from-to).
type FreezeRequest struct {
	Name         string    `json:"name"`
	Reason       string    `json:"reason,omitempty"`
	Applications []string  `json:"applications"`
	Environments []string  `json:"environments"`
	Regions      []string  `json:"regions,omitempty"`
	From         time.Time `json:"from,omitzero"`
	To           time.Time `json:"to"`
	// Hard windows cannot be exempted by the policy nor overridden with freezeOverride.
	Hard bool `json:"hard,omitempty"`
}

// Policy revision states.
const (
	PolicyDraft      = "draft"
	PolicyActive     = "active"
	PolicySuperseded = "superseded"
)

// PolicyRevision is an immutable set of Rego modules and data.
type PolicyRevision struct {
	Revision    int64      `json:"revision"`
	SHA256      string     `json:"sha256"`
	Status      string     `json:"status"`
	Author      string     `json:"author"`
	Comment     string     `json:"comment,omitempty"`
	ReviewedBy  string     `json:"reviewed_by,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	ActivatedAt *time.Time `json:"activated_at,omitempty"`
	// Tests is the outcome of the revision's tests (*_test.rego) run at push.
	Tests []PolicyTest `json:"tests,omitempty"`
	// Files maps the path of every file (modules *.rego, data.json, data.yaml) to its content.
	Files map[string]string `json:"files,omitempty"`
}

// PolicyTest is the outcome of a policy test.
type PolicyTest struct {
	Name  string `json:"name"`
	Pass  bool   `json:"pass"`
	Error string `json:"error,omitempty"`
}

// PolicyList lists the revisions and tells the policy in force in this process.
type PolicyList struct {
	// Engine is builtin or opa; Source the policy source (store, file, bundle, consul).
	Engine string `json:"engine"`
	Source string `json:"source,omitempty"`
	// Current is the revision in force in the process that answered.
	Current   string           `json:"current"`
	LoadedAt  *time.Time       `json:"loaded_at,omitempty"`
	Revisions []PolicyRevision `json:"revisions"`
}

// PolicyPush is a new revision to store as a draft.
type PolicyPush struct {
	Files   map[string]string `json:"files"`
	Comment string            `json:"comment,omitempty"`
}

// PolicyEvaluationRequest is the deploy to evaluate: a descriptor and optionally phase and
// kind (default submit and deploy).
type PolicyEvaluationRequest struct {
	Descriptor *descriptor.Descriptor `json:"descriptor"`
	Phase      string                 `json:"phase,omitempty"`
	Kind       string                 `json:"kind,omitempty"`
}

// PolicyEvaluation is the decision of a revision on an input.
type PolicyEvaluation struct {
	Input    governance.Input    `json:"input"`
	Decision governance.Decision `json:"decision"`
}

// PolicyImpact compares the decisions of a revision with those recorded for the real deploys
// of a period.
type PolicyImpact struct {
	Revision  string    `json:"revision"`
	Since     time.Time `json:"since"`
	Evaluated int       `json:"evaluated"`
	// Changes are the deploys whose decision would change.
	Changes []PolicyChange `json:"changes,omitempty"`
}

// PolicyChange is a deploy whose decision would change with the evaluated revision.
type PolicyChange struct {
	Deployment  string    `json:"deployment"`
	Application string    `json:"application"`
	Environment string    `json:"environment"`
	Version     string    `json:"version"`
	CreatedAt   time.Time `json:"created_at"`
	Before      string    `json:"before"`
	After       string    `json:"after"`
}
