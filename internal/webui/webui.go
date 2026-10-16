// Package webui serves the manent web interface, built from web/ (make web) and embedded in
// the binary with go:embed. Without a build (clean checkout, go build without Node) it serves
// a page that explains how to build it.
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

// dist holds the build of web/ (vite build writes here); .gitkeep keeps the directory in the
// repository even without a build.
//
//go:embed all:dist
var dist embed.FS

// Options configures the handler.
type Options struct {
	// ConnectOrigins are the extra origins the page can contact (e.g. the OIDC issuer), besides its own.
	ConnectOrigins []string
}

// Handler serves the static files of the UI. Paths that match no file return index.html,
// because routing is client-side. Files in assets/ have the hash in their name and are cached
// for a year; index.html is never cached, so an update arrives at once.
type Handler struct {
	files  fs.FS
	index  []byte
	csp    string
	loaded time.Time
}

// New creates the handler with the embedded build.
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
		// Inline styles allowed: dialog components insert a <style> to lock the page scroll. Scripts
		// stay only those of the build.
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

// Built reports whether the UI build is embedded.
func (h *Handler) Built() bool { return h.index != nil }

// ServeHTTP implements http.Handler.
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
		// A file with an extension that does not exist is a real 404, not an app page.
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
