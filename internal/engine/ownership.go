package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/glob"
	"github.com/joeg-ita/manent/internal/pipeline"
	"github.com/joeg-ita/manent/internal/tenant"
)

// TenantLimits are the restrictions of the tenant that owns the application, recorded in the
// request: the config step checks them again on the effective descriptor, so the decision
// does not depend on the registry read during a recovery.
type TenantLimits struct {
	Tenant          string   `json:"tenant"`
	SecretRefs      []string `json:"secret_refs,omitempty"`
	ArtifactSources []string `json:"artifact_sources,omitempty"`
	CallbackHosts   []string `json:"callback_hosts,omitempty"`
}

// limitsOf returns the restrictions of the tenant, nil if it has none.
func limitsOf(t *tenant.Tenant) *TenantLimits {
	if t == nil || (len(t.SecretRefs) == 0 && len(t.ArtifactSources) == 0 && len(t.CallbackHosts) == 0) {
		return nil
	}
	return &TenantLimits{Tenant: t.Name, SecretRefs: t.SecretRefs, ArtifactSources: t.ArtifactSources, CallbackHosts: t.CallbackHosts}
}

// check verifies the descriptor against the restrictions: secret references, artifact source
// and callback hosts. Every error is permanent.
func (l *TenantLimits) check(d *descriptor.Descriptor) error {
	if l == nil {
		return nil
	}
	var errs []error
	if len(l.SecretRefs) > 0 {
		for _, s := range d.Secrets {
			if !glob.Any(l.SecretRefs, s.Ref) {
				errs = append(errs, fmt.Errorf("secrets.%s: reference not allowed for tenant %s (secret_refs)", s.Name, l.Tenant))
			}
		}
	}
	if len(l.ArtifactSources) > 0 && !slices.ContainsFunc(l.ArtifactSources, func(p string) bool { return strings.HasPrefix(d.Artifact.Source, p) }) {
		errs = append(errs, fmt.Errorf("artifact.source %s is not allowed for tenant %s (artifact_sources)", d.Artifact.Source, l.Tenant))
	}
	if len(l.CallbackHosts) > 0 {
		for i, cb := range d.Callbacks {
			u, err := url.Parse(cb.URL)
			if err != nil || !glob.Any(l.CallbackHosts, u.Hostname()) {
				errs = append(errs, fmt.Errorf("callbacks[%d]: host not allowed for tenant %s (callback_hosts)", i, l.Tenant))
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return pipeline.Permanent(err)
	}
	return nil
}

// checkLimits verifies the effective descriptor against the restrictions of the application
// owner, at submission and in validations.
func (e *Engine) checkLimits(ctx context.Context, d *descriptor.Descriptor) error {
	owner, err := e.owner(ctx, d.Application)
	if errors.Is(err, pipeline.ErrPermanent) {
		return fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	if err != nil {
		// The registry is unreadable: the worker checks again with the recorded limits.
		return nil
	}
	if err := limitsOf(owner).check(d); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	return nil
}

// checkOwnership refuses a deploy ID already used by another tenant: a team cannot take or,
// by resubmitting it, read the deploy of another one. The error does not say whose it is.
func (e *Engine) checkOwnership(ctx context.Context, d *descriptor.Descriptor, owner *tenant.Ref) error {
	if d.ID == "" || e.tenants == nil {
		return nil
	}
	st, err := e.Status(ctx, workflowID(d))
	if err != nil {
		return nil
	}
	name := ""
	if owner != nil {
		name = owner.Name
	}
	if st.Tenant != name {
		return fmt.Errorf("%w: deployment id %s is already in use", ErrInvalidRequest, d.ID)
	}
	return nil
}

// deployQueue is the queue a deploy request runs on: the owner's, or def.
func deployQueue(req Request, def string) string { return cmp.Or(req.Queue, def) }
