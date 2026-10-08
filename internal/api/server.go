// Package api espone le funzionalità di go-dart come API HTTP (Gin) con descrittori JSON.
package api

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
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
	v1.POST("/deployments/plan", s.plan)
	v1.GET("/deployments/:id", s.status)
	return r
}

// submit accoda un deploy e risponde 202 con l'ID assegnato.
func (s *Server) submit(c *gin.Context) {
	d, ok := s.readDescriptor(c)
	if !ok {
		return
	}
	id, err := s.svc.Submit(c.Request.Context(), d)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}
	c.Header("Location", "/api/v1/deployments/"+id)
	c.JSON(http.StatusAccepted, gin.H{"id": id})
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
	switch {
	case errors.Is(err, engine.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	case err != nil:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusOK, st)
	}
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
