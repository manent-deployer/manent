package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/joeg-ita/manent/internal/engine"
)

// tenants lists the tenants (query: application, environment, selector).
func (s *Server) tenants(c *gin.Context) {
	ts, err := s.svc.Tenants(c.Request.Context(), engine.TenantFilter{
		Application: c.Query("application"), Environment: c.Query("environment"), Selector: c.Query("selector"),
	})
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"tenants": ts})
}

// tenant returns a tenant.
func (s *Server) tenant(c *gin.Context) {
	t, err := s.svc.Tenant(c.Request.Context(), c.Param("name"))
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, t)
}
