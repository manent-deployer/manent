package authz

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/joeg-ita/manent/internal/audit"
	"github.com/joeg-ita/manent/internal/auth"
	"github.com/joeg-ita/manent/internal/engine"
	"github.com/joeg-ita/manent/internal/governance"
)

// Audit actions of the policy administration, besides those subject to permissions.
const (
	ActionPolicyPush     Action = "policy.push"
	ActionPolicyActivate Action = "policy.activate"
	ActionFreezeCreate   Action = "freeze.create"
	ActionFreezeDelete   Action = "freeze.delete"
)

// checkPlatform verifies an action that does not depend on application and environment.
func (g *Guard) checkPlatform(ctx context.Context, a Action, t target) error {
	p := auth.FromContext(ctx)
	if g.permittedAnywhere(ctx, p, a) {
		return nil
	}
	err := fmt.Errorf("%w: %s cannot perform %s", engine.ErrForbidden, actor(p), a)
	g.record(ctx, a, t, audit.OutcomeDenied, err)
	return err
}

// checkFreeze verifies freeze.write on every application/environment pattern of the window.
func (g *Guard) checkFreeze(ctx context.Context, applications, environments []string, t target) error {
	p := auth.FromContext(ctx)
	for _, a := range applications {
		for _, e := range environments {
			if !g.permitted(ctx, p, FreezeWrite, a, e, nil) {
				err := fmt.Errorf("%w: %s cannot perform %s on %s/%s", engine.ErrForbidden, actor(p), FreezeWrite, a, e)
				g.record(ctx, FreezeWrite, t, audit.OutcomeDenied, err)
				return err
			}
		}
	}
	return nil
}

// freezeTarget describes a window in the audit.
func freezeTarget(name string, applications, environments []string, reason string) target {
	return target{application: strings.Join(applications, ","), environment: strings.Join(environments, ","), comment: name + ": " + reason}
}

// CreateFreeze requires freeze.write on the window's applications and environments.
func (g *Guard) CreateFreeze(ctx context.Context, r engine.FreezeRequest) (*governance.WindowStatus, error) {
	t := freezeTarget(r.Name, r.Applications, r.Environments, r.Reason)
	if err := g.checkFreeze(ctx, r.Applications, r.Environments, t); err != nil {
		return nil, err
	}
	w, err := g.Next.CreateFreeze(ctx, r)
	g.done(ctx, ActionFreezeCreate, t, err)
	return w, err
}

// DeleteFreeze requires freeze.write on the applications and environments of the window.
func (g *Guard) DeleteFreeze(ctx context.Context, name string) error {
	ws, err := g.Next.Freezes(ctx, "", "")
	if err != nil {
		return err
	}
	var found *governance.WindowStatus
	for i := range ws {
		if ws[i].Name == name && ws[i].Dynamic {
			found = &ws[i]
		}
	}
	if found == nil {
		return fmt.Errorf("%w: freeze %s (only windows created at runtime can be deleted)", engine.ErrNotFound, name)
	}
	t := freezeTarget(name, found.Applications, found.Environments, "")
	if err := g.checkFreeze(ctx, found.Applications, found.Environments, t); err != nil {
		return err
	}
	err = g.Next.DeleteFreeze(ctx, name)
	g.done(ctx, ActionFreezeDelete, t, err)
	return err
}

// Policies requires policy.read.
func (g *Guard) Policies(ctx context.Context) (*engine.PolicyList, error) {
	if err := g.checkPlatform(ctx, PolicyRead, target{}); err != nil {
		return nil, err
	}
	return g.Next.Policies(ctx)
}

// PolicyRevision requires policy.read.
func (g *Guard) PolicyRevision(ctx context.Context, revision int64) (*engine.PolicyRevision, error) {
	if err := g.checkPlatform(ctx, PolicyRead, target{}); err != nil {
		return nil, err
	}
	return g.Next.PolicyRevision(ctx, revision)
}

// PushPolicy requires policy.write.
func (g *Guard) PushPolicy(ctx context.Context, r engine.PolicyPush) (*engine.PolicyRevision, error) {
	t := target{comment: r.Comment}
	if err := g.checkPlatform(ctx, PolicyWrite, t); err != nil {
		return nil, err
	}
	rev, err := g.Next.PushPolicy(ctx, r)
	if rev != nil {
		t.comment = "revision " + strconv.FormatInt(rev.Revision, 10) + " " + rev.SHA256 + ": " + r.Comment
	}
	g.done(ctx, ActionPolicyPush, t, err)
	return rev, err
}

// PolicyAccess requires policy.write: the simulation shows the permissions of any identity.
func (g *Guard) PolicyAccess(ctx context.Context, revision int64, r engine.PolicyAccessRequest) (*engine.PolicyAccess, error) {
	if err := g.checkPlatform(ctx, PolicyWrite, target{}); err != nil {
		return nil, err
	}
	return g.Next.PolicyAccess(ctx, revision, r)
}

// EvaluatePolicy requires policy.write: the evaluation shows the effective configuration of
// the deploy, so it also requires reading the application/environment.
func (g *Guard) EvaluatePolicy(ctx context.Context, revision int64, r engine.PolicyEvaluationRequest) (*engine.PolicyEvaluation, error) {
	if err := g.checkPlatform(ctx, PolicyWrite, target{}); err != nil {
		return nil, err
	}
	if r.Descriptor != nil {
		if err := g.check(ctx, DeploymentRead, target{application: r.Descriptor.Application, environment: r.Descriptor.Environment}); err != nil {
			return nil, err
		}
	}
	return g.Next.EvaluatePolicy(ctx, revision, r)
}

// PolicyImpact requires policy.write; only the deploys the identity can read are reported.
func (g *Guard) PolicyImpact(ctx context.Context, revision int64, since time.Duration) (*engine.PolicyImpact, error) {
	if err := g.checkPlatform(ctx, PolicyWrite, target{}); err != nil {
		return nil, err
	}
	imp, err := g.Next.PolicyImpact(ctx, revision, since)
	if err != nil {
		return nil, err
	}
	p := auth.FromContext(ctx)
	changes := imp.Changes[:0]
	for _, c := range imp.Changes {
		if g.permitted(ctx, p, DeploymentRead, c.Application, c.Environment, nil) {
			changes = append(changes, c)
		}
	}
	imp.Changes = changes
	return imp, nil
}

// ApprovePolicy requires policy.approve.
func (g *Guard) ApprovePolicy(ctx context.Context, revision int64) error {
	t := target{comment: "revision " + strconv.FormatInt(revision, 10)}
	if err := g.checkPlatform(ctx, PolicyApprove, t); err != nil {
		return err
	}
	err := g.Next.ApprovePolicy(ctx, revision)
	g.done(ctx, ActionPolicyActivate, t, err)
	return err
}

// ActivatePolicy requires policy.approve.
func (g *Guard) ActivatePolicy(ctx context.Context, revision int64) error {
	t := target{comment: "revision " + strconv.FormatInt(revision, 10)}
	if err := g.checkPlatform(ctx, PolicyApprove, t); err != nil {
		return err
	}
	err := g.Next.ActivatePolicy(ctx, revision)
	g.done(ctx, ActionPolicyActivate, t, err)
	return err
}
