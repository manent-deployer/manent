package api

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/joeg-ita/manent/internal/engine"
	"github.com/joeg-ita/manent/internal/service"
)

// maxPolicyBodySize limits a policy revision upload (files in JSON).
const maxPolicyBodySize = 8 << 20

// createFreeze creates a freeze window at runtime: 201 with the window.
func (s *Server) createFreeze(c *gin.Context) {
	var r engine.FreezeRequest
	if !readJSON(c, &r) {
		return
	}
	w, err := s.svc.CreateFreeze(c.Request.Context(), r)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusCreated, w)
}

// deleteFreeze deletes a freeze window created at runtime.
func (s *Server) deleteFreeze(c *gin.Context) {
	if err := s.svc.DeleteFreeze(c.Request.Context(), c.Param("name")); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// policies lists the policy revisions.
func (s *Server) policies(c *gin.Context) {
	l, err := s.svc.Policies(c.Request.Context())
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, l)
}

// policyRevision parses :revision; "current" (or 0) is the revision in force.
func policyRevision(c *gin.Context) (int64, bool) {
	v := c.Param("revision")
	if v == "current" {
		return 0, true
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		writeError(c, fmt.Errorf("%w: invalid policy revision %q", engine.ErrInvalidRequest, v))
		return 0, false
	}
	return n, true
}

// policy returns a revision with its files.
func (s *Server) policy(c *gin.Context) {
	rev, ok := policyRevision(c)
	if !ok {
		return
	}
	p, err := s.svc.PolicyRevision(c.Request.Context(), rev)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, p)
}

// pushPolicy stores a draft revision after compilation and tests: 201 with the revision.
func (s *Server) pushPolicy(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxPolicyBodySize)
	var r engine.PolicyPush
	if !readJSON(c, &r) {
		return
	}
	p, err := s.svc.PushPolicy(c.Request.Context(), r)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusCreated, p)
}

// evaluatePolicy evaluates a revision on a descriptor.
func (s *Server) evaluatePolicy(c *gin.Context) {
	rev, ok := policyRevision(c)
	if !ok {
		return
	}
	var r engine.PolicyEvaluationRequest
	if !readJSON(c, &r) {
		return
	}
	if r.Descriptor != nil {
		if err := r.Descriptor.Validate(); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}
	ev, err := s.svc.EvaluatePolicy(c.Request.Context(), rev, r)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, ev)
}

// policyAccess simulates the permissions of an identity with a revision.
func (s *Server) policyAccess(c *gin.Context) {
	rev, ok := policyRevision(c)
	if !ok {
		return
	}
	var r engine.PolicyAccessRequest
	if !readJSON(c, &r) {
		return
	}
	if r.Check != nil && r.Check.Descriptor != nil {
		if err := r.Check.Descriptor.Validate(); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}
	acc, err := s.svc.PolicyAccess(c.Request.Context(), rev, r)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, acc)
}

// policyImpact recomputes with a revision the decisions of the deploys since ?since= (default 7d).
func (s *Server) policyImpact(c *gin.Context) {
	rev, ok := policyRevision(c)
	if !ok {
		return
	}
	since := 7 * 24 * time.Hour
	if v := c.Query("since"); v != "" {
		now := time.Now()
		t, err := service.ParseSince(v, now)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		since = now.Sub(t)
	}
	imp, err := s.svc.PolicyImpact(c.Request.Context(), rev, since)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, imp)
}

// activatePolicy approves a draft (approve) or goes back to a previous revision (activate).
func (s *Server) activatePolicy(draft bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		rev, ok := policyRevision(c)
		if !ok {
			return
		}
		op := s.svc.ActivatePolicy
		if draft {
			op = s.svc.ApprovePolicy
		}
		if err := op(c.Request.Context(), rev); err != nil {
			writeError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}
