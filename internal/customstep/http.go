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

	"github.com/manent-deployer/manent/internal/customspec"
)

// maxResponse limits the response read.
const maxResponse = 1 << 20

// HeaderIdempotencyKey is sent with every request: after a recovery the step may repeat the
// request, and the receiver recognizes it by this key ("<deploy>:<step>").
const HeaderIdempotencyKey = "Idempotency-Key"

// http runs the request and, if configured, waits for the result by polling poll.url.
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
			return nil, permanent("JSON body: %w", err)
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
			return nil, permanent("output %q: %s missing or not scalar in the response", k, r.s.doc.Outputs[k].Path)
		}
		out[k] = v
	}
	return out, nil
}

// authorize adds authentication with the declared secret.
func (r *run) authorize(req *http.Request) error {
	au := r.action.Auth
	if au == nil {
		return nil
	}
	secret, ok := r.secrets[au.Secret]
	if !ok {
		return permanent("authentication secret %q not among those of the run", au.Secret)
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

// do sends the request and reads the response. Statuses 5xx, 408 and 429 are temporary, the
// others outside expect permanent. With parse the response must be JSON.
func (r *run) do(req *http.Request, expect []int, parse bool) (any, error) {
	// Only host and path in the logs: the query may contain tokens.
	target := req.URL.Host + req.URL.Path
	resp, err := r.s.b.HTTP.Do(req)
	if err != nil {
		return nil, temporary("%s %s: %w", req.Method, target, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return nil, temporary("%s %s: reading the response: %w", req.Method, target, err)
	}
	r.log.InfoContext(req.Context(), "HTTP request", "method", req.Method, "url", target, "status", resp.StatusCode)
	if !accepted(resp.StatusCode, expect) {
		switch c := resp.StatusCode; {
		case c >= 500 || c == http.StatusRequestTimeout || c == http.StatusTooManyRequests:
			return nil, temporary("%s %s: status %d", req.Method, target, c)
		default:
			return nil, permanent("%s %s: unexpected status %d", req.Method, target, c)
		}
	}
	if len(data) > maxResponse {
		return nil, permanent("%s %s: response over %d bytes", req.Method, target, maxResponse)
	}
	if !parse {
		return nil, nil
	}
	var doc any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, permanent("%s %s: the response is not JSON", req.Method, target)
	}
	return doc, nil
}

// poll polls poll.url periodically until until or fail are true, or until the timeout.
// The wait happens inside the step: after a crash the step restarts and repeats the initial
// request with the same Idempotency-Key.
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
			return nil, temporary("wait interrupted: %w", ctx.Err())
		case <-deadline.C:
			return nil, permanent("wait for the outcome expired after %s", p.Timeout)
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
			// A temporary error during the wait does not stop it.
			r.log.WarnContext(ctx, "state read failed, retrying", "error", r.red.apply(err.Error()))
			continue
		}
		if f := p.Fail; f != nil {
			if v, ok := customspec.Lookup(doc, f.Path); ok && slices.Contains(f.In, v) {
				return nil, permanent("the operation failed: %s = %q", f.Path, v)
			}
		}
		if v, ok := customspec.Lookup(doc, p.Until.Path); ok && slices.Contains(p.Until.In, v) {
			return doc, nil
		}
	}
}

// accepted checks the status: with an empty expect any 2xx is fine.
func accepted(code int, expect []int) bool {
	if len(expect) == 0 {
		return code >= 200 && code <= 299
	}
	return slices.Contains(expect, code)
}
