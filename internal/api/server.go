// Package api espone le funzionalità di go-dart come API HTTP (Gin) con descrittori JSON.
package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/joeg-ita/go-dart/internal/descriptor"
	"github.com/joeg-ita/go-dart/internal/engine"
)

// maxBodySize limita la dimensione di un descrittore JSON.
const maxBodySize = 1 << 20

// Server contiene le dipendenze degli handler HTTP.
type Server struct {
	svc    engine.Service
	logger *slog.Logger
}

// NewRouter crea il router Gin con tutte le rotte.
func NewRouter(svc engine.Service, logger *slog.Logger) *gin.Engine {
	s := &Server{svc: svc, logger: logger.With("component", "api")}
	r := gin.New()
	r.Use(gin.Recovery(), s.accessLog())

	r.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	v1 := r.Group("/api/v1")
	v1.POST("/deployments", s.submit)
	v1.GET("/deployments", s.list)
	v1.POST("/deployments/plan", s.plan)
	v1.GET("/deployments/:id", s.status)
	v1.POST("/deployments/:id/cancel", s.cancel)

	v1.GET("/schedules", s.listSchedules)
	v1.GET("/schedules/:name", s.getSchedule)
	v1.POST("/schedules/:name/pause", s.scheduleOp(svc.PauseSchedule))
	v1.POST("/schedules/:name/resume", s.scheduleOp(svc.ResumeSchedule))
	v1.DELETE("/schedules/:name", s.scheduleOp(svc.DeleteSchedule))
	return r
}

// submit accoda un deploy (202), anche pianificato con schedule.at,
// oppure crea/aggiorna una schedule ricorrente con schedule.cron (201).
func (s *Server) submit(c *gin.Context) {
	d, ok := s.readDescriptor(c)
	if !ok {
		return
	}
	sub, err := s.svc.Submit(c.Request.Context(), d)
	if err != nil {
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

// list elenca i deploy più recenti (query: schedule, limit).
func (s *Server) list(c *gin.Context) {
	f := engine.ListFilter{Schedule: c.Query("schedule")}
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
	c.JSON(http.StatusOK, gin.H{"deployments": deps})
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
	p, err := s.svc.Plan(d)
	if err != nil {
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

// writeError traduce gli errori dell'engine nel codice HTTP corrispondente.
func writeError(c *gin.Context, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, engine.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, engine.ErrInvalidRequest):
		code = http.StatusUnprocessableEntity
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

// accessLog registra ogni richiesta sul logger applicativo (e quindi anche sul SIEM).
func (s *Server) accessLog() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		s.logger.Info("richiesta HTTP",
			"method", c.Request.Method,
			"path", c.FullPath(),
			"status", c.Writer.Status(),
			"duration_ms", time.Since(start).Milliseconds(),
			"client_ip", c.ClientIP())
	}
}
