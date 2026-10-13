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

// UIConfig è la configurazione che l'interfaccia web legge da /ui/config.json prima del login.
// Non contiene segreti: il client OIDC della UI è pubblico (PKCE).
type UIConfig struct {
	// AuthMode è "none" (nessun login), "oidc" (login con l'identity provider) oppure
	// "unavailable" (autenticazione attiva ma senza un client OIDC per la UI).
	AuthMode     string   `json:"auth_mode"`
	Issuer       string   `json:"issuer,omitempty"`
	ClientID     string   `json:"client_id,omitempty"`
	Scopes       []string `json:"scopes,omitempty"`
	RedirectPath string   `json:"redirect_path,omitempty"`
	Version      string   `json:"version"`
}

// Modalità di autenticazione dell'interfaccia web.
const (
	UIAuthNone        = "none"
	UIAuthOIDC        = "oidc"
	UIAuthUnavailable = "unavailable"
)

// mountUI registra /ui/config.json e serve l'interfaccia web per i percorsi non gestiti
// dall'API. Le rotte /api/v1 sconosciute restano un 404 JSON.
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

// me restituisce l'identità della richiesta e i suoi permessi.
func (s *Server) me(c *gin.Context) {
	id, err := s.svc.Me(c.Request.Context())
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, id)
}

// approvals elenca i deploy in attesa che l'identità può approvare.
func (s *Server) approvals(c *gin.Context) {
	deps, err := s.svc.Approvals(c.Request.Context())
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"approvals": deps})
}

// heartbeatEvery è l'intervallo dei commenti che tengono aperta la connessione SSE attraverso
// proxy e bilanciatori.
const heartbeatEvery = 15 * time.Second

// events invia come Server-Sent Events lo stato di un deploy (evento "status", quando cambia)
// e le nuove righe di log (evento "log", con id = offset successivo, così un client che si
// riconnette con Last-Event-ID riprende da lì). Alla conclusione del deploy, dopo le ultime
// righe, invia "end" e chiude. Lo stato è letto dal servizio a intervalli, quindi valgono gli
// stessi permessi di Status e Logs.
func (s *Server) events(c *gin.Context) {
	// Lo stato letto a ogni giro autorizza anche la lettura dei log: il Guard non lo rilegge.
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
