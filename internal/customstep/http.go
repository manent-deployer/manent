package customstep

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/joeg-ita/manent/internal/customspec"
)

// maxResponse limita la risposta letta.
const maxResponse = 1 << 20

// HeaderIdempotencyKey è inviato con ogni richiesta: dopo una recovery lo step può ripetere
// la richiesta, e il destinatario la riconosce da questa chiave ("<deploy>:<step>").
const HeaderIdempotencyKey = "Idempotency-Key"

// http esegue la richiesta e, se previsto, attende l'esito interrogando poll.url.
func (r *run) http(ctx context.Context) (map[string]string, error) {
	a := r.action
	u, err := renderURL("url", a.URL, r.data, r.s.b.AllowHTTP)
	if err != nil {
		return nil, permanent("%w", err)
	}
	r.targets = []string{u.Host}
	var body []byte
	if a.JSON != nil {
		v, err := renderJSON(a.JSON, "json", r.data)
		if err != nil {
			return nil, permanent("%w", err)
		}
		if body, err = json.Marshal(v); err != nil {
			return nil, permanent("corpo JSON: %w", err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, a.Method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, permanent("%w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "manent")
	req.Header.Set(HeaderIdempotencyKey, r.in.DeploymentID+":"+r.s.ps.Name)
	for _, k := range slices.Sorted(maps.Keys(a.Headers)) {
		v, err := renderHeader("headers."+k, a.Headers[k], r.data)
		if err != nil {
			return nil, permanent("%w", err)
		}
		req.Header.Set(k, v)
	}
	if err := r.authorize(req); err != nil {
		return nil, err
	}
	doc, err := r.do(req, a.Expect, len(r.s.doc.Outputs) > 0 || a.Poll != nil)
	if err != nil {
		return nil, err
	}
	if a.Poll != nil {
		if doc, err = r.poll(ctx, a.Poll, doc); err != nil {
			return nil, err
		}
	}
	out := map[string]string{}
	for _, k := range slices.Sorted(maps.Keys(r.s.doc.Outputs)) {
		v, ok := customspec.Lookup(doc, r.s.doc.Outputs[k].Path)
		if !ok {
			return nil, permanent("output %q: %s assente o non scalare nella risposta", k, r.s.doc.Outputs[k].Path)
		}
		out[k] = v
	}
	return out, nil
}

// authorize aggiunge l'autenticazione con il segreto dichiarato.
func (r *run) authorize(req *http.Request) error {
	au := r.action.Auth
	if au == nil {
		return nil
	}
	secret, ok := r.secrets[au.Secret]
	if !ok {
		return permanent("segreto %q dell'autenticazione non tra quelli dell'esecuzione", au.Secret)
	}
	secret = strings.TrimSpace(secret)
	switch au.Type {
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+secret)
	case "basic":
		req.SetBasicAuth(au.Username, secret)
	case "header":
		req.Header.Set(au.Header, secret)
	}
	return nil
}

// do invia la richiesta e legge la risposta. Gli status 5xx, 408 e 429 sono temporanei, gli
// altri fuori da expect permanenti. Con parse la risposta deve essere JSON.
func (r *run) do(req *http.Request, expect []int, parse bool) (any, error) {
	// Nei log solo host e percorso: la query può contenere token.
	target := req.URL.Host + req.URL.Path
	resp, err := r.s.b.HTTP.Do(req)
	if err != nil {
		return nil, temporary("%s %s: %w", req.Method, target, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return nil, temporary("%s %s: lettura della risposta: %w", req.Method, target, err)
	}
	r.log.InfoContext(req.Context(), "richiesta HTTP", "method", req.Method, "url", target, "status", resp.StatusCode)
	if !accepted(resp.StatusCode, expect) {
		switch c := resp.StatusCode; {
		case c >= 500 || c == http.StatusRequestTimeout || c == http.StatusTooManyRequests:
			return nil, temporary("%s %s: status %d", req.Method, target, c)
		default:
			return nil, permanent("%s %s: status %d non atteso", req.Method, target, c)
		}
	}
	if len(data) > maxResponse {
		return nil, permanent("%s %s: risposta oltre %d byte", req.Method, target, maxResponse)
	}
	if !parse {
		return nil, nil
	}
	var doc any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, permanent("%s %s: la risposta non è JSON", req.Method, target)
	}
	return doc, nil
}

// poll interroga poll.url a intervalli finché until o fail sono veri, o fino al timeout.
// L'attesa avviene dentro lo step: dopo un crash lo step riparte e ripete la richiesta
// iniziale con la stessa Idempotency-Key.
func (r *run) poll(ctx context.Context, p *customspec.Poll, first any) (any, error) {
	data := r.data
	data.Response = first
	u, err := renderURL("poll.url", p.URL, data, r.s.b.AllowHTTP)
	if err != nil {
		return nil, permanent("%w", err)
	}
	deadline := time.NewTimer(p.Timeout)
	defer deadline.Stop()
	tick := time.NewTicker(p.Interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, temporary("attesa interrotta: %w", ctx.Err())
		case <-deadline.C:
			return nil, permanent("attesa dell'esito scaduta dopo %s", p.Timeout)
		case <-tick.C:
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, permanent("%w", err)
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "manent")
		if err := r.authorize(req); err != nil {
			return nil, err
		}
		doc, err := r.do(req, nil, true)
		switch {
		case isPermanent(err):
			return nil, err
		case err != nil:
			// Un errore temporaneo durante l'attesa non la interrompe.
			r.log.WarnContext(ctx, "lettura dello stato fallita, nuovo tentativo", "error", r.red.apply(err.Error()))
			continue
		}
		if f := p.Fail; f != nil {
			if v, ok := customspec.Lookup(doc, f.Path); ok && slices.Contains(f.In, v) {
				return nil, permanent("l'operazione è fallita: %s = %q", f.Path, v)
			}
		}
		if v, ok := customspec.Lookup(doc, p.Until.Path); ok && slices.Contains(p.Until.In, v) {
			return doc, nil
		}
	}
}

// accepted verifica lo status: con expect vuoto vale qualunque 2xx.
func accepted(code int, expect []int) bool {
	if len(expect) == 0 {
		return code >= 200 && code <= 299
	}
	return slices.Contains(expect, code)
}
