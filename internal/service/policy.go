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
	// PolicyAccess simulates with a revision (0 = the active one) the permissions of an identity
	// given by hand and, optionally, the outcome of an action (rbac.engine: opa).
	PolicyAccess(ctx context.Context, revision int64, r PolicyAccessRequest) (*PolicyAccess, error)
	// ApprovePolicy activates a draft revision (by an identity other than its author when
	// governance.opa.store.review is set).
	ApprovePolicy(ctx context.Context, revision int64) error
	// ActivatePolicy goes back to a revision already active in the past.
	ActivatePolicy(ctx context.Context, revision int64) error
}

// FreezeRequest asks to create a freeze window at runtime: absolute (from-to).
type FreezeRequest struct {
	Name         string   `json:"name"`
	Reason       string   `json:"reason,omitempty"`
	Applications []string `json:"applications"`
	Environments []string `json:"environments"`
	Regions      []string `json:"regions,omitempty"`
	// Tenants limits the window to the deploys of the matching tenants (selector).
	Tenants string    `json:"tenants,omitempty"`
	From    time.Time `json:"from,omitzero"`
	To      time.Time `json:"to"`
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

// PolicyAccessRequest is the identity whose permissions are simulated: nothing is read from the
// identity provider, subject, groups and claims are those given.
type PolicyAccessRequest struct {
	// Subject is "<method>:<name>" (e.g. "oidc:alice"); a name without method is oidc.
	Subject string            `json:"subject"`
	Groups  []string          `json:"groups,omitempty"`
	Claims  map[string]string `json:"claims,omitempty"`
	// Compare also computes the grants gained and lost against the active revision.
	Compare bool `json:"compare,omitempty"`
	// Check, if set, is an action whose outcome is simulated.
	Check *AccessCheck `json:"check,omitempty"`
}

// AccessCheck is an action on an application, environment and optionally tenant; Descriptor
// gives the artifact to the denials (deny) of the changes.
type AccessCheck struct {
	Action      string                 `json:"action"`
	Application string                 `json:"application"`
	Environment string                 `json:"environment"`
	Tenant      string                 `json:"tenant,omitempty"`
	Descriptor  *descriptor.Descriptor `json:"descriptor,omitempty"`
}

// PolicyAccess is the outcome of the simulation of an identity's permissions.
type PolicyAccess struct {
	Revision string  `json:"revision"`
	Subject  string  `json:"subject"`
	Grants   []Grant `json:"grants"`
	// Warnings are the grants of the revision discarded as invalid or with policy.* actions.
	Warnings []string `json:"warnings,omitempty"`
	// Compared is the active revision the grants were compared with (Compare).
	Compared string         `json:"compared,omitempty"`
	Gained   []Grant        `json:"gained,omitempty"`
	Lost     []Grant        `json:"lost,omitempty"`
	Check    *AccessOutcome `json:"check,omitempty"`
}

// AccessOutcome is the simulated outcome of an action.
type AccessOutcome struct {
	Allowed bool `json:"allowed"`
	// GrantedBy is the name of the grant that allows the action.
	GrantedBy string `json:"granted_by,omitempty"`
	// Denied are the reasons of the denials (deny) of the revision.
	Denied []string `json:"denied,omitempty"`
	// Reason explains a refusal without denials: no grant, or a static action.
	Reason string `json:"reason,omitempty"`
}
