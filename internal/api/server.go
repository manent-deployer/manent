// Package api exposes manent as an HTTP API (Gin) with JSON descriptors.
package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/manent-deployer/manent/internal/auth"
	"github.com/manent-deployer/manent/internal/descriptor"
	"github.com/manent-deployer/manent/internal/engine"
	"github.com/manent-deployer/manent/internal/service"
	"github.com/manent-deployer/manent/internal/telemetry"
)

// maxBodySize limits the size of a JSON descriptor.
const maxBodySize = 1 << 20

// Server holds the dependencies of the HTTP handlers.
type Server struct {
	svc     engine.Service
	authn   auth.Authenticator
	logger  *slog.Logger
	metrics telemetry.Recorder
	// eventInterval is how often the SSE stream reads status and logs again.
	eventInterval time.Duration
}

// Options are the optional dependencies of the router.
type Options struct {
	// Metrics records duration and outcome of requests.
	Metrics telemetry.Recorder
	// MetricsHandler, if set, is served on /metrics without authentication.
	MetricsHandler http.Handler
	// Ready, if set, is checked by /readyz (e.g. database reachability).
	Ready func(context.Context) error
	// UI, if set, serves the web interface for paths not handled by the API.
	UI http.Handler
	// UIConfig, if set, is served on /ui/config.json without authentication.
	UIConfig *UIConfig
	// EventInterval is the refresh interval of the SSE stream (default 1s).
	EventInterval time.Duration
	// SlackInteractions, if set, receives Slack message button clicks on
	// /integrations/slack/interactions: authentication is Slack's signature, not a bearer.
	SlackInteractions http.Handler
	// TrustedProxies are the proxies whose X-Forwarded-For is believed (addresses or CIDRs);
	// none by default, so the client address cannot be forged with a header.
	TrustedProxies []string
}

// NewRouter creates the Gin router with all routes. With a nil authn authentication is
// disabled and every request uses the anonymous identity; otherwise the /api/v1 routes
// require a bearer token. /healthz, /readyz and /metrics are always reachable.
func NewRouter(svc engine.Service, authn auth.Authenticator, logger *slog.Logger) *gin.Engine {
	return NewRouterWith(svc, authn, logger, Options{})
}

// NewRouterWith creates the router with the given options.
func NewRouterWith(svc engine.Service, authn auth.Authenticator, logger *slog.Logger, o Options) *gin.Engine {
	s := &Server{svc: svc, authn: authn, logger: logger.With("component", "api"), metrics: o.Metrics, eventInterval: o.EventInterval}
	if s.metrics == nil {
		s.metrics = telemetry.Noop{}
	}
	if s.eventInterval <= 0 {
		s.eventInterval = time.Second
	}
	r := gin.New()
	// Gin trusts every proxy by default: anyone could set the client address with
	// X-Forwarded-For. The configuration is validated, so the error is not expected.
	if err := r.SetTrustedProxies(o.TrustedProxies); err != nil {
		s.logger.Error("invalid trusted proxies: X-Forwarded-For ignored", "error", err)
		_ = r.SetTrustedProxies(nil)
	}
	r.Use(gin.Recovery(), s.trace(), s.accessLog())

	AddProbes(r, o.Ready, s.logger)
	if o.MetricsHandler != nil {
		r.GET("/metrics", gin.WrapH(o.MetricsHandler))
	}
	if o.SlackInteractions != nil {
		r.POST("/integrations/slack/interactions", gin.WrapH(o.SlackInteractions))
	}
	v1 := r.Group("/api/v1", s.authenticate())
	v1.POST("/deployments", s.submit)
	v1.GET("/deployments", s.list)
	v1.POST("/deployments/plan", s.plan)
	v1.POST("/deployments/validate", s.validate)
	v1.POST("/environments/validate", s.validateEnvironment)
	v1.GET("/deployments/:id", s.status)
	v1.GET("/deployments/:id/steps", s.steps)
	v1.GET("/deployments/:id/logs", s.logs)
	v1.GET("/deployments/:id/events", s.events)
	v1.GET("/approvals", s.approvals)
	v1.GET("/me", s.me)
	v1.GET("/metrics/dora", s.dora)
	v1.POST("/deployments/:id/cancel", s.cancel)
	v1.POST("/deployments/:id/approve", s.decide(true))
	v1.POST("/deployments/:id/reject", s.decide(false))
	v1.POST("/promotions", s.promote)
	v1.POST("/rollbacks", s.rollback)
	v1.GET("/inventory", s.inventory)
	v1.GET("/freezes", s.freezes)
	v1.POST("/freezes", s.createFreeze)
	v1.DELETE("/freezes/:name", s.deleteFreeze)
	v1.GET("/tenants", s.tenants)
	v1.GET("/tenants/:name", s.tenant)
	v1.GET("/policies", s.policies)
	v1.POST("/policies", s.pushPolicy)
	v1.GET("/policies/:revision", s.policy)
	v1.POST("/policies/:revision/evaluate", s.evaluatePolicy)
	v1.POST("/policies/:revision/access", s.policyAccess)
	v1.GET("/policies/:revision/impact", s.policyImpact)
	v1.POST("/policies/:revision/approve", s.activatePolicy(true))
	v1.POST("/policies/:revision/activate", s.activatePolicy(false))
	v1.GET("/workers", s.workers)
	v1.GET("/queues", s.queues)
	v1.POST("/releases", s.submitRelease)
	v1.GET("/releases", s.listReleases)
	v1.GET("/releases/:id", s.releaseStatus)
	v1.POST("/releases/:id/cancel", s.cancelRelease)
	v1.GET("/activity", s.activity)

	v1.GET("/schedules", s.listSchedules)
	v1.GET("/schedules/:name", s.getSchedule)
	v1.POST("/schedules/:name/pause", s.scheduleOp(svc.PauseSchedule))
	v1.POST("/schedules/:name/resume", s.scheduleOp(svc.ResumeSchedule))
	v1.DELETE("/schedules/:name", s.scheduleOp(svc.DeleteSchedule))
	s.mountUI(r, o)
	return r
}

// submit enqueues a deploy (202), possibly scheduled with schedule.at,
// or creates/updates a recurring schedule with schedule.cron (201).
// With ?dry_run=true (or dryRun in the descriptor) it enqueues a simulation of the deploy.
func (s *Server) submit(c *gin.Context) {
	d, ok := s.readDescriptor(c)
	if !ok {
		return
	}
	if v := c.Query("dry_run"); v != "" {
		dry, err := strconv.ParseBool(v)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "dry_run must be a boolean"})
			return
		}
		d.DryRun = d.DryRun || dry
		if err := d.Validate(); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}
	sub, err := s.svc.Submit(c.Request.Context(), d)
	writeSubmission(c, sub, err)
}

// writeSubmission answers the enqueuing of a deploy: 202 (201 for a recurring schedule).
// An error other than a permission error is an unacceptable descriptor (422).
func writeSubmission(c *gin.Context, sub *engine.Submission, err error) {
	if err != nil {
		if errors.Is(err, engine.ErrForbidden) || errors.Is(err, engine.ErrNotFound) {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}
	if sub.Schedule != "" {
		c.Header("Location", "/api/v1/schedules/"+sub.Schedule)
		c.JSON(http.StatusCreated, sub)
		return
	}
	c.Header("Location", "/api/v1/deployments/"+sub.ID)
	c.JSON(http.StatusAccepted, sub)
}

// list lists the most recent deploys (query: schedule, application, environment, state, limit).
func (s *Server) list(c *gin.Context) {
	f := engine.ListFilter{Schedule: c.Query("schedule"), Application: c.Query("application"), Environment: c.Query("environment"),
		Tenant: c.Query("tenant"), Artifact: c.Query("artifact"), Selector: c.Query("selector"), State: c.Query("state"), Cursor: c.Query("cursor")}
	if l := c.Query("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be a positive integer"})
			return
		}
		f.Limit = n
	}
	deps, err := s.svc.List(c.Request.Context(), f)
	if err != nil {
		writeError(c, err)
		return
	}
	out := gin.H{"deployments": deps}
	// A full page may continue: next_cursor is the ID of the last deploy.
	if limit := cmp.Or(f.Limit, 50); len(deps) >= limit && len(deps) > 0 {
		out["next_cursor"] = deps[len(deps)-1].ID
	}
	c.JSON(http.StatusOK, out)
}

// cancel cancels a waiting or running deploy.
func (s *Server) cancel(c *gin.Context) {
	if err := s.svc.Cancel(c.Request.Context(), c.Param("id")); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// listSchedules lists the recurring schedules.
func (s *Server) listSchedules(c *gin.Context) {
	ss, err := s.svc.ListSchedules(c.Request.Context())
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"schedules": ss})
}

// getSchedule returns a schedule by name.
func (s *Server) getSchedule(c *gin.Context) {
	sc, err := s.svc.GetSchedule(c.Request.Context(), c.Param("name"))
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, sc)
}

// scheduleOp creates a handler for schedule operations (pause, resume, delete).
func (s *Server) scheduleOp(op func(context.Context, string) error) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := op(c.Request.Context(), c.Param("name")); err != nil {
			writeError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

// plan returns the steps that would run, without starting the deploy.
func (s *Server) plan(c *gin.Context) {
	d, ok := s.readDescriptor(c)
	if !ok {
		return
	}
	p, err := s.svc.Plan(c.Request.Context(), d)
	if err != nil {
		if errors.Is(err, engine.ErrForbidden) {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"plan": p})
}

// status returns the status of a deploy.
func (s *Server) status(c *gin.Context) {
	st, err := s.svc.Status(c.Request.Context(), c.Param("id"))
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, st)
}

// decide approves or rejects a deploy awaiting approval (optional body: {"comment": ...}).
func (s *Server) decide(approve bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body struct {
			Comment string `json:"comment"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, maxBodySize))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body: " + err.Error()})
			return
		}
		r := engine.DecisionRequest{Approve: approve, Comment: body.Comment}
		if err := s.svc.Decide(c.Request.Context(), c.Param("id"), r); err != nil {
			writeError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

// promote releases to an environment the artifact already released to another one.
func (s *Server) promote(c *gin.Context) {
	var r engine.PromoteRequest
	if !readJSON(c, &r) {
		return
	}
	sub, err := s.svc.Promote(c.Request.Context(), r)
	writeSubmission(c, sub, err)
}

// rollback releases a previous version again.
func (s *Server) rollback(c *gin.Context) {
	var r engine.RollbackRequest
	if !readJSON(c, &r) {
		return
	}
	sub, err := s.svc.Rollback(c.Request.Context(), r)
	writeSubmission(c, sub, err)
}

// inventory returns the release in service per application and environment.
func (s *Server) inventory(c *gin.Context) {
	inv, err := s.svc.Inventory(c.Request.Context(), engine.InventoryFilter{Application: c.Query("application"), Environment: c.Query("environment"), Tenant: c.Query("tenant"), Artifact: c.Query("artifact"), Selector: c.Query("selector")})
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"inventory": inv})
}

// freezes lists the active and future freeze windows.
func (s *Server) freezes(c *gin.Context) {
	ws, err := s.svc.Freezes(c.Request.Context(), c.Query("application"), c.Query("environment"))
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"freezes": ws})
}

// readJSON decodes the JSON body, rejecting unknown fields.
// submitRelease enqueues a composite release: 202 with the initial state, which reports the
// IDs of the deploys.
func (s *Server) submitRelease(c *gin.Context) {
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxReleaseBodySize))
	if err != nil {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": err.Error()})
		return
	}
	r, err := descriptor.ParseReleaseJSON(body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	rs, err := s.svc.SubmitRelease(c.Request.Context(), r)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, rs)
}

// maxReleaseBodySize is the body limit of a release: it holds several descriptors.
const maxReleaseBodySize = 16 * maxBodySize

func (s *Server) listReleases(c *gin.Context) {
	limit := 0
	if l := c.Query("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be a positive integer"})
			return
		}
		limit = n
	}
	rs, err := s.svc.ListReleases(c.Request.Context(), limit)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"releases": rs})
}

func (s *Server) releaseStatus(c *gin.Context) {
	rs, err := s.svc.ReleaseStatus(c.Request.Context(), c.Param("id"))
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, rs)
}

func (s *Server) cancelRelease(c *gin.Context) {
	if err := s.svc.CancelRelease(c.Request.Context(), c.Param("id")); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (s *Server) workers(c *gin.Context) {
	ws, err := s.svc.Workers(c.Request.Context())
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"workers": ws})
}

func (s *Server) queues(c *gin.Context) {
	qs, err := s.svc.Queues(c.Request.Context())
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"queues": qs})
}

func (s *Server) activity(c *gin.Context) {
	a, err := s.svc.Activity(c.Request.Context(), engine.ActivityFilter{Application: c.Query("application"), Environment: c.Query("environment"), Tenant: c.Query("tenant")})
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, a)
}

func readJSON(c *gin.Context, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, maxBodySize))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body: " + err.Error()})
		return false
	}
	return true
}

// validate checks a descriptor as at submission and returns the effective deploy.
func (s *Server) validate(c *gin.Context) {
	d, ok := s.readDescriptor(c)
	if !ok {
		return
	}
	v, err := s.svc.Validate(c.Request.Context(), d, engine.ValidateOptions{CheckArtifact: c.Query("check_artifact") == "true"})
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, v)
}

// validateEnvironment checks a candidate environment configuration before it is applied.
func (s *Server) validateEnvironment(c *gin.Context) {
	var req engine.EnvironmentCheck
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, fmt.Errorf("%w: %w", engine.ErrInvalidRequest, err))
		return
	}
	if req.Application == "" || req.Environment == "" {
		writeError(c, fmt.Errorf("%w: application and environment are required", engine.ErrInvalidRequest))
		return
	}
	if req.Descriptor != nil {
		if err := req.Descriptor.Validate(); err != nil {
			writeError(c, fmt.Errorf("%w: %w", engine.ErrInvalidRequest, err))
			return
		}
	}
	v, err := s.svc.ValidateEnvironment(c.Request.Context(), req)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, v)
}

// steps returns the step timeline of a deploy.
func (s *Server) steps(c *gin.Context) {
	steps, err := s.svc.Steps(c.Request.Context(), c.Param("id"))
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"steps": steps})
}

// logs returns the log of a deploy from ?offset= (default 0).
func (s *Server) logs(c *gin.Context) {
	offset := 0
	if v := c.Query("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "offset must be an integer >= 0"})
			return
		}
		offset = n
	}
	p, err := s.svc.Logs(c.Request.Context(), c.Param("id"), offset)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, p)
}

// dora computes the DORA metrics; ?since= takes an RFC 3339 date or a duration (e.g. 720h).
func (s *Server) dora(c *gin.Context) {
	f := engine.DORAFilter{Application: c.Query("application"), Environment: c.Query("environment"), Tenant: c.Query("tenant"), Artifact: c.Query("artifact"), ByTenant: c.Query("by") == "tenant"}
	if v := c.Query("since"); v != "" {
		t, err := service.ParseSince(v, time.Now())
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		f.Since = t
	}
	rep, err := s.svc.DORA(c.Request.Context(), f)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, rep)
}

// writeError maps engine errors to the matching HTTP status.
func writeError(c *gin.Context, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, engine.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, engine.ErrInvalidRequest):
		code = http.StatusUnprocessableEntity
	case errors.Is(err, engine.ErrForbidden):
		code = http.StatusForbidden
	case errors.Is(err, engine.ErrUnavailable):
		code = http.StatusServiceUnavailable
	}
	c.JSON(code, gin.H{"error": err.Error()})
}

// readDescriptor reads and validates the JSON descriptor from the body.
func (s *Server) readDescriptor(c *gin.Context) (*descriptor.Descriptor, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxBodySize))
	if err != nil {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": err.Error()})
		return nil, false
	}
	d, err := descriptor.ParseJSON(body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return nil, false
	}
	return d, true
}

// authenticate verifies the bearer token and puts the identity in the request context.
// The details of a verification error go only to the log, never to the response.
func (s *Server) authenticate() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.authn == nil {
			c.Request = c.Request.WithContext(auth.WithPrincipal(c.Request.Context(), auth.Anonymous))
			return
		}
		token, ok := auth.BearerToken(c.Request)
		var p *auth.Principal
		err := auth.ErrUnauthenticated
		if ok {
			p, err = s.authn.Authenticate(c.Request.Context(), token)
		}
		if err != nil {
			s.logger.Warn("authentication failed", "path", c.FullPath(), "client_ip", c.ClientIP(), "error", err)
			c.Header("WWW-Authenticate", `Bearer realm="manent"`)
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": auth.ErrUnauthenticated.Error()})
			return
		}
		c.Request = c.Request.WithContext(auth.WithPrincipal(c.Request.Context(), p))
	}
}

// accessLog logs every request on the application logger (and therefore to the SIEM).
// AddProbes adds /healthz (process alive) and /readyz (ready to serve: ready, if set,
// returns no error). The probes require no authentication, so the reason of a failure goes
// only to the log, once each time it changes: it may name databases, files or hosts.
func AddProbes(r gin.IRoutes, ready func(context.Context) error, logger *slog.Logger) {
	var last atomic.Value // string: the last reason logged
	r.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	r.GET("/readyz", func(c *gin.Context) {
		if ready != nil {
			ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
			defer cancel()
			if err := ready(ctx); err != nil {
				if prev, _ := last.Swap(err.Error()).(string); prev != err.Error() {
					logger.Warn("not ready", "error", err)
				}
				c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unavailable"})
				return
			}
		}
		if prev, _ := last.Swap("").(string); prev != "" {
			logger.Info("ready again")
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
}

// trace opens a span for every request, child of the one sent by the client (traceparent).
// The span reaches the worker through the deploy request (engine.Request.TraceParent).
func (s *Server) trace() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := telemetry.Extract(c.Request.Context(), propagation.HeaderCarrier(c.Request.Header))
		route := c.FullPath()
		if route == "" {
			route = "not found"
		}
		ctx, span := telemetry.Tracer().Start(ctx, c.Request.Method+" "+route, trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(attribute.String("http.request.method", c.Request.Method), attribute.String("http.route", route)))
		defer span.End()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
		span.SetAttributes(attribute.Int("http.response.status_code", c.Writer.Status()))
		if c.Writer.Status() >= 500 {
			span.SetStatus(codes.Error, http.StatusText(c.Writer.Status()))
		}
	}
}

func (s *Server) accessLog() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		if route := c.FullPath(); route != "" {
			s.metrics.HTTPRequest(c.Request.Method, route, c.Writer.Status(), time.Since(start))
		}
		s.logger.Info("HTTP request",
			"method", c.Request.Method,
			"path", c.FullPath(),
			"status", c.Writer.Status(),
			"duration_ms", time.Since(start).Milliseconds(),
			"client_ip", c.ClientIP(),
			"actor", auth.FromContext(c.Request.Context()).String())
	}
}
