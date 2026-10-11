// Package webui serve l'interfaccia web di manent, compilata da web/ (make web) e incorporata
// nel binario con go:embed. Senza build (checkout pulito, go build senza Node) serve una
// pagina che spiega come costruirla.
package webui

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// dist contiene la build di web/ (vite build scrive qui); .gitkeep tiene la directory nel
// repository anche senza build.
//
//go:embed all:dist
var dist embed.FS

// Options configura l'handler.
type Options struct {
	// ConnectOrigins sono le origini aggiuntive che la pagina può contattare (es. l'issuer OIDC),
	// oltre alla propria.
	ConnectOrigins []string
}

// Handler serve i file statici della UI. I percorsi che non corrispondono a un file restituiscono
// index.html, perché il routing è lato client. I file in assets/ hanno l'hash nel nome e sono
// in cache per un anno; index.html non è mai in cache, così un aggiornamento arriva subito.
type Handler struct {
	files  fs.FS
	index  []byte
	csp    string
	loaded time.Time
}

// New crea l'handler con la build incorporata.
func New(o Options) *Handler {
	sub, _ := fs.Sub(dist, "dist")
	return newHandler(sub, o)
}

func newHandler(files fs.FS, o Options) *Handler {
	h := &Handler{files: files, loaded: time.Now()}
	if b, err := fs.ReadFile(files, "index.html"); err == nil {
		h.index = b
	}
	connect := []string{"'self'"}
	for _, origin := range o.ConnectOrigins {
		if u, err := url.Parse(origin); err == nil && u.Scheme != "" && u.Host != "" {
			connect = append(connect, u.Scheme+"://"+u.Host)
		}
	}
	h.csp = strings.Join([]string{
		"default-src 'self'",
		"script-src 'self'",
		// Stili inline ammessi: i componenti dei dialoghi inseriscono un <style> per bloccare lo
		// scroll della pagina. Gli script restano solo quelli della build.
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data:",
		"font-src 'self'",
		"connect-src " + strings.Join(connect, " "),
		"frame-src " + strings.Join(connect, " "),
		"object-src 'none'",
		"base-uri 'self'",
		"form-action 'self'",
		"frame-ancestors 'none'",
	}, "; ")
	return h
}

// Built indica se la build della UI è incorporata.
func (h *Handler) Built() bool { return h.index != nil }

// ServeHTTP implementa http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hd := w.Header()
	hd.Set("Content-Security-Policy", h.csp)
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("X-Frame-Options", "DENY")
	hd.Set("Referrer-Policy", "no-referrer")
	hd.Set("Cross-Origin-Opener-Policy", "same-origin")
	hd.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "metodo non consentito", http.StatusMethodNotAllowed)
		return
	}
	if h.index == nil {
		hd.Set("Cache-Control", "no-store")
		hd.Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(notBuilt))
		return
	}
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name != "" && name != "index.html" {
		if st, err := fs.Stat(h.files, name); err == nil && !st.IsDir() {
			if strings.HasPrefix(name, "assets/") {
				hd.Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				hd.Set("Cache-Control", "no-cache")
			}
			http.ServeFileFS(w, r, h.files, name)
			return
		}
		// Un file con estensione che non esiste è un 404 vero, non una pagina dell'app.
		if path.Ext(name) != "" {
			http.NotFound(w, r)
			return
		}
	}
	hd.Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "index.html", h.loaded, bytes.NewReader(h.index))
}

const notBuilt = `<!doctype html><html lang="it"><head><meta charset="utf-8"><title>manent</title></head>
<body><main><h1>Interfaccia web non compilata</h1>
<p>Questo binario è stato compilato senza la UI. Eseguire <code>make web</code> e poi <code>make build</code>,
oppure usare un binario o un'immagine di rilascio. L'API è disponibile su <code>/api/v1</code>.</p></main></body></html>`
