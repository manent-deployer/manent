package apiclient

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/joeg-ita/manent/internal/governance"
	"github.com/joeg-ita/manent/internal/service"
)

// revisionPath is the path of a policy revision (0 = the one in force).
func revisionPath(revision int64) string {
	if revision == 0 {
		return "/policies/current"
	}
	return "/policies/" + strconv.FormatInt(revision, 10)
}

// CreateFreeze implements service.Service.
func (c *Client) CreateFreeze(ctx context.Context, r service.FreezeRequest) (*governance.WindowStatus, error) {
	var w governance.WindowStatus
	if err := c.do(ctx, http.MethodPost, "/freezes", nil, r, &w); err != nil {
		return nil, err
	}
	return &w, nil
}

// DeleteFreeze implements service.Service.
func (c *Client) DeleteFreeze(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/freezes/"+url.PathEscape(name), nil, nil, nil)
}

// Policies implements service.Service.
func (c *Client) Policies(ctx context.Context) (*service.PolicyList, error) {
	var l service.PolicyList
	if err := c.do(ctx, http.MethodGet, "/policies", nil, nil, &l); err != nil {
		return nil, err
	}
	return &l, nil
}

// PolicyRevision implements service.Service.
func (c *Client) PolicyRevision(ctx context.Context, revision int64) (*service.PolicyRevision, error) {
	var p service.PolicyRevision
	if err := c.do(ctx, http.MethodGet, revisionPath(revision), nil, nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// PushPolicy implements service.Service.
func (c *Client) PushPolicy(ctx context.Context, r service.PolicyPush) (*service.PolicyRevision, error) {
	var p service.PolicyRevision
	if err := c.do(ctx, http.MethodPost, "/policies", nil, r, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// EvaluatePolicy implements service.Service.
func (c *Client) EvaluatePolicy(ctx context.Context, revision int64, r service.PolicyEvaluationRequest) (*service.PolicyEvaluation, error) {
	var ev service.PolicyEvaluation
	if err := c.do(ctx, http.MethodPost, revisionPath(revision)+"/evaluate", nil, r, &ev); err != nil {
		return nil, err
	}
	return &ev, nil
}

// PolicyAccess implements service.Service.
func (c *Client) PolicyAccess(ctx context.Context, revision int64, r service.PolicyAccessRequest) (*service.PolicyAccess, error) {
	var acc service.PolicyAccess
	if err := c.do(ctx, http.MethodPost, revisionPath(revision)+"/access", nil, r, &acc); err != nil {
		return nil, err
	}
	return &acc, nil
}

// PolicyImpact implements service.Service.
func (c *Client) PolicyImpact(ctx context.Context, revision int64, since time.Duration) (*service.PolicyImpact, error) {
	var imp service.PolicyImpact
	q := url.Values{}
	if since > 0 {
		q.Set("since", since.String())
	}
	if err := c.do(ctx, http.MethodGet, revisionPath(revision)+"/impact", q, nil, &imp); err != nil {
		return nil, err
	}
	return &imp, nil
}

// ApprovePolicy implements service.Service.
func (c *Client) ApprovePolicy(ctx context.Context, revision int64) error {
	return c.do(ctx, http.MethodPost, revisionPath(revision)+"/approve", nil, nil, nil)
}

// ActivatePolicy implements service.Service.
func (c *Client) ActivatePolicy(ctx context.Context, revision int64) error {
	return c.do(ctx, http.MethodPost, revisionPath(revision)+"/activate", nil, nil, nil)
}
