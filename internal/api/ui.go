package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/joeg-ita/manent/internal/authz"
	"github.com/joeg-ita/manent/internal/service"
)

// UIConfig is the configuration the web interface reads from /ui/config.json before login.
// It holds no secrets: the UI OIDC client is public (PKCE).
type UIConfig struct {
	// AuthMode is "none" (no login), "oidc" (login with the identity provider) or
	// "unavailable" (authentication enabled but no OIDC client for the UI).
	AuthMode     string   `json:"auth_mode"`
	Issuer       string   `json:"issuer,omitempty"`
	ClientID     string   `json:"client_id,omitempty"`
	Scopes       []string `json:"scopes,omitempty"`
	RedirectPath string   `json:"redirect_path,omitempty"`
	Version      string   `json:"version"`
}

// Authentication modes of the web interface.
const (
	UIAuthNone        = "none"
	UIAuthOIDC        = "oidc"
	UIAuthUnavailable = "unavailable"
)

// mountUI registers /ui/config.json and serves the web interface for paths not handled by
// the API. Unknown /api/v1 routes stay a JSON 404.
func (s *Server) mountUI(r *gin.Engine, o Options) {
	if o.UIConfig != nil {
		cfg := *o.UIConfig
		r.GET("/ui/config.json", func(c *gin.Context) {
			c.Header("Cache-Control", "no-store")
			c.JSON(http.StatusOK, cfg)
		})
	}
	r.NoRoute(func(c *gin.Context) {
		if o.UI == nil || strings.HasPrefix(c.Request.URL.Path, "/api/") ||
			(c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead) {
			c.JSON(http.StatusNotFound, gin.H{"error": "risorsa non trovata"})
			return
		}
		o.UI.ServeHTTP(c.Writer, c.Request)
	})
}

// me returns the identity of the request and its permissions.
func (s *Server) me(c *gin.Context) {
	id, err := s.svc.Me(c.Request.Context())
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, id)
}

// approvals lists the waiting deploys the identity can approve.
func (s *Server) approvals(c *gin.Context) {
	deps, err := s.svc.Approvals(c.Request.Context())
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"approvals": deps})
}

// heartbeatEvery is the interval of the comments that keep the SSE connection open through
// proxies and load balancers.
const heartbeatEvery = 15 * time.Second

// events sends as Server-Sent Events the status of a deploy (event "status", when it changes)
// and the new log lines (event "log", with id = next offset, so a client reconnecting with
// Last-Event-ID resumes from there). When the deploy ends, after the last lines, it sends
// "end" and closes. Status is read from the service periodically, so the same permissions as
// Status and Logs apply.
func (s *Server) events(c *gin.Context) {
	// The status read at every round also authorizes reading the logs: the Guard does not read it again.
	ctx := authz.WithDeploymentScope(c.Request.Context())
	id := c.Param("id")
	offset := 0
	for _, v := range []string{c.GetHeader("Last-Event-ID"), c.Query("offset")} {
		if v == "" {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "offset deve essere un intero >= 0"})
			return
		}
		offset = n
		break
	}
	st, err := s.svc.Status(ctx, id)
	if err != nil {
		writeError(c, err)
		return
	}
	h := c.Writer.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)

	var last []byte
	lastWrite := time.Now()
	send := func(event, id string, data []byte) {
		writeEvent(c, event, id, data)
		lastWrite = time.Now()
	}
	tick := time.NewTicker(s.eventInterval)
	defer tick.Stop()
	for {
		if b, err := json.Marshal(st); err == nil && !bytes.Equal(b, last) {
			send("status", "", b)
			last = b
		}
		page, err := s.svc.Logs(ctx, id, offset)
		if err != nil {
			send("error", "", mustJSON(gin.H{"error": err.Error()}))
			return
		}
		if len(page.Lines) > 0 {
			offset = page.Next
			send("log", strconv.Itoa(offset), mustJSON(page.Lines))
		}
		if service.Terminal(st.State) && len(page.Lines) == 0 {
			send("end", "", mustJSON(gin.H{"state": st.State}))
			return
		}
		if time.Since(lastWrite) >= heartbeatEvery {
			_, _ = fmt.Fprint(c.Writer, ": keep-alive\n\n")
			c.Writer.Flush()
			lastWrite = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if st, err = s.svc.Status(ctx, id); err != nil {
			send("error", "", mustJSON(gin.H{"error": err.Error()}))
			return
		}
	}
}

func writeEvent(c *gin.Context, event, id string, data []byte) {
	if id != "" {
		_, _ = fmt.Fprintf(c.Writer, "id: %s\n", id)
	}
	_, _ = fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", event, data)
	c.Writer.Flush()
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
