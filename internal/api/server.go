// Package api espone le funzionalità di manent come API HTTP (Gin) con descrittori JSON.
package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/joeg-ita/manent/internal/auth"
	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/engine"
	"github.com/joeg-ita/manent/internal/service"
	"github.com/joeg-ita/manent/internal/telemetry"
)

// maxBodySize limita la dimensione di un descrittore JSON.
const maxBodySize = 1 << 20

// Server contiene le dipendenze degli handler HTTP.
type Server struct {
	svc     engine.Service
	authn   auth.Authenticator
	logger  *slog.Logger
	metrics telemetry.Recorder
	// eventInterval è l'intervallo con cui lo stream SSE rilegge stato e log.
	eventInterval time.Duration
}

// Options sono le dipendenze facoltative del router.
type Options struct {
	// Metrics registra durata ed esito delle richieste.
	Metrics telemetry.Recorder
	// MetricsHandler, se presente, è servito su /metrics senza autenticazione.
	MetricsHandler http.Handler
	// Ready, se presente, è verificato da /readyz (es. raggiungibilità del database).
	Ready func(context.Context) error
	// UI, se presente, serve l'interfaccia web per i percorsi non gestiti dall'API.
	UI http.Handler
	// UIConfig, se presente, è servita su /ui/config.json senza autenticazione.
	UIConfig *UIConfig
	// EventInterval è l'intervallo di aggiornamento dello stream SSE (default 1s).
	EventInterval time.Duration
}

// NewRouter crea il router Gin con tutte le rotte. Con authn nil l'autenticazione è
// disattivata e ogni richiesta usa l'identità anonima; altrimenti le rotte /api/v1 richiedono
// un token bearer. /healthz, /readyz e /metrics restano sempre accessibili.
func NewRouter(svc engine.Service, authn auth.Authenticator, logger *slog.Logger) *gin.Engine {
	return NewRouterWith(svc, authn, logger, Options{})
}

// NewRouterWith crea il router con le opzioni indicate.
func NewRouterWith(svc engine.Service, authn auth.Authenticator, logger *slog.Logger, o Options) *gin.Engine {
	s := &Server{svc: svc, authn: authn, logger: logger.With("component", "api"), metrics: o.Metrics, eventInterval: o.EventInterval}
	if s.metrics == nil {
		s.metrics = telemetry.Noop{}
	}
	if s.eventInterval <= 0 {
		s.eventInterval = time.Second
	}
	r := gin.New()
	r.Use(gin.Recovery(), s.trace(), s.accessLog())

	AddProbes(r, o.Ready)
	if o.MetricsHandler != nil {
		r.GET("/metrics", gin.WrapH(o.MetricsHandler))
	}
	v1 := r.Group("/api/v1", s.authenticate())
	v1.POST("/deployments", s.submit)
	v1.GET("/deployments", s.list)
	v1.POST("/deployments/plan", s.plan)
	v1.POST("/deployments/validate", s.validate)
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
	v1.GET("/workers", s.workers)
	v1.GET("/activity", s.activity)

	v1.GET("/schedules", s.listSchedules)
	v1.GET("/schedules/:name", s.getSchedule)
	v1.POST("/schedules/:name/pause", s.scheduleOp(svc.PauseSchedule))
	v1.POST("/schedules/:name/resume", s.scheduleOp(svc.ResumeSchedule))
	v1.DELETE("/schedules/:name", s.scheduleOp(svc.DeleteSchedule))
	s.mountUI(r, o)
	return r
}

// submit accoda un deploy (202), anche pianificato con schedule.at,
// oppure crea/aggiorna una schedule ricorrente con schedule.cron (201).
// Con ?dry_run=true (o dryRun nel descrittore) accoda una simulazione del deploy.
func (s *Server) submit(c *gin.Context) {
	d, ok := s.readDescriptor(c)
	if !ok {
		return
	}
	if v := c.Query("dry_run"); v != "" {
		dry, err := strconv.ParseBool(v)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "dry_run deve essere un booleano"})
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

// writeSubmission risponde all'accodamento di un deploy: 202 (201 per una schedule ricorrente).
// Un errore che non sia di permessi è un descrittore non accettabile (422).
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

// list elenca i deploy più recenti (query: schedule, application, environment, state, limit).
func (s *Server) list(c *gin.Context) {
	f := engine.ListFilter{Schedule: c.Query("schedule"), Application: c.Query("application"), Environment: c.Query("environment"), State: c.Query("state"), Cursor: c.Query("cursor")}
	if l := c.Query("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "limit deve essere un intero positivo"})
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
	// Una pagina piena può avere un seguito: next_cursor è l'ID dell'ultimo deploy.
	if limit := cmp.Or(f.Limit, 50); len(deps) >= limit && len(deps) > 0 {
		out["next_cursor"] = deps[len(deps)-1].ID
	}
	c.JSON(http.StatusOK, out)
}

// cancel annulla un deploy in attesa o in esecuzione.
func (s *Server) cancel(c *gin.Context) {
	if err := s.svc.Cancel(c.Request.Context(), c.Param("id")); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// listSchedules elenca le schedule ricorrenti.
func (s *Server) listSchedules(c *gin.Context) {
	ss, err := s.svc.ListSchedules(c.Request.Context())
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"schedules": ss})
}

// getSchedule restituisce una schedule per nome.
func (s *Server) getSchedule(c *gin.Context) {
	sc, err := s.svc.GetSchedule(c.Request.Context(), c.Param("name"))
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, sc)
}

// scheduleOp crea un handler per le operazioni sulle schedule (pausa, ripresa, eliminazione).
func (s *Server) scheduleOp(op func(context.Context, string) error) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := op(c.Request.Context(), c.Param("name")); err != nil {
			writeError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

// plan restituisce gli step che verrebbero eseguiti, senza avviare il deploy.
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

// status restituisce lo stato di un deploy.
func (s *Server) status(c *gin.Context) {
	st, err := s.svc.Status(c.Request.Context(), c.Param("id"))
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, st)
}

// decide approva o respinge un deploy in attesa di approvazione (body opzionale: {"comment": ...}).
func (s *Server) decide(approve bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body struct {
			Comment string `json:"comment"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, maxBodySize))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "body JSON non valido: " + err.Error()})
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

// promote rilascia in un ambiente l'artefatto già rilasciato in un altro.
func (s *Server) promote(c *gin.Context) {
	var r engine.PromoteRequest
	if !readJSON(c, &r) {
		return
	}
	sub, err := s.svc.Promote(c.Request.Context(), r)
	writeSubmission(c, sub, err)
}

// rollback rilascia di nuovo una versione precedente.
func (s *Server) rollback(c *gin.Context) {
	var r engine.RollbackRequest
	if !readJSON(c, &r) {
		return
	}
	sub, err := s.svc.Rollback(c.Request.Context(), r)
	writeSubmission(c, sub, err)
}

// inventory restituisce la release in esercizio per applicazione e ambiente.
func (s *Server) inventory(c *gin.Context) {
	inv, err := s.svc.Inventory(c.Request.Context(), engine.InventoryFilter{Application: c.Query("application"), Environment: c.Query("environment")})
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"inventory": inv})
}

// freezes elenca le finestre di freeze attive e future.
func (s *Server) freezes(c *gin.Context) {
	ws, err := s.svc.Freezes(c.Request.Context(), c.Query("application"), c.Query("environment"))
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"freezes": ws})
}

// readJSON decodifica il body JSON, rifiutando campi sconosciuti.
func (s *Server) workers(c *gin.Context) {
	ws, err := s.svc.Workers(c.Request.Context())
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"workers": ws})
}

func (s *Server) activity(c *gin.Context) {
	a, err := s.svc.Activity(c.Request.Context(), engine.ActivityFilter{Application: c.Query("application"), Environment: c.Query("environment")})
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
		c.JSON(http.StatusBadRequest, gin.H{"error": "body JSON non valido: " + err.Error()})
		return false
	}
	return true
}

// validate verifica un descrittore come all'accodamento e restituisce il deploy effettivo.
func (s *Server) validate(c *gin.Context) {
	d, ok := s.readDescriptor(c)
	if !ok {
		return
	}
	v, err := s.svc.Validate(c.Request.Context(), d)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, v)
}

// steps restituisce la timeline degli step di un deploy.
func (s *Server) steps(c *gin.Context) {
	steps, err := s.svc.Steps(c.Request.Context(), c.Param("id"))
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"steps": steps})
}

// logs restituisce il log di un deploy da ?offset= (default 0).
func (s *Server) logs(c *gin.Context) {
	offset := 0
	if v := c.Query("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "offset deve essere un intero >= 0"})
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

// dora calcola le metriche DORA; ?since= accetta una data RFC 3339 o una durata (es. 720h).
func (s *Server) dora(c *gin.Context) {
	f := engine.DORAFilter{Application: c.Query("application"), Environment: c.Query("environment")}
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

// writeError traduce gli errori dell'engine nel codice HTTP corrispondente.
func writeError(c *gin.Context, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, engine.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, engine.ErrInvalidRequest):
		code = http.StatusUnprocessableEntity
	case errors.Is(err, engine.ErrForbidden):
		code = http.StatusForbidden
	}
	c.JSON(code, gin.H{"error": err.Error()})
}

// readDescriptor legge e valida il descrittore JSON dal body.
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

// authenticate verifica il token bearer e mette l'identità nel context della richiesta.
// I dettagli di un errore di verifica finiscono solo nel log, non nella risposta.
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
			s.logger.Warn("autenticazione fallita", "path", c.FullPath(), "client_ip", c.ClientIP(), "error", err)
			c.Header("WWW-Authenticate", `Bearer realm="manent"`)
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": auth.ErrUnauthenticated.Error()})
			return
		}
		c.Request = c.Request.WithContext(auth.WithPrincipal(c.Request.Context(), p))
	}
}

// accessLog registra ogni richiesta sul logger applicativo (e quindi anche sul SIEM).
// AddProbes aggiunge /healthz (processo attivo) e /readyz (pronto a servire: ready, se
// presente, non restituisce errori).
func AddProbes(r gin.IRoutes, ready func(context.Context) error) {
	r.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	r.GET("/readyz", func(c *gin.Context) {
		if ready != nil {
			ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
			defer cancel()
			if err := ready(ctx); err != nil {
				c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unavailable", "error": err.Error()})
				return
			}
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
}

// trace apre una span per ogni richiesta, figlia di quella indicata dal client (traceparent).
// La span arriva al worker tramite la richiesta di deploy (engine.Request.TraceParent).
func (s *Server) trace() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := telemetry.Extract(c.Request.Context(), propagation.HeaderCarrier(c.Request.Header))
		route := c.FullPath()
		if route == "" {
			route = "non trovato"
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
		s.logger.Info("richiesta HTTP",
			"method", c.Request.Method,
			"path", c.FullPath(),
			"status", c.Writer.Status(),
			"duration_ms", time.Since(start).Milliseconds(),
			"client_ip", c.ClientIP(),
			"actor", auth.FromContext(c.Request.Context()).String())
	}
}
