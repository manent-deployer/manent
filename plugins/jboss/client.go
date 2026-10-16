package jboss

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Client talks to the HTTP management API of JBoss EAP / WildFly (DMR operations in JSON on
// /management, content upload on /management/add-content).
type Client interface {
	// Execute runs an operation and returns its result.
	Execute(ctx context.Context, op Operation) (json.RawMessage, error)
	// Upload uploads a file to the content repository and returns its hash.
	Upload(ctx context.Context, path string) (string, error)
}

// Connector opens a Client to an instance (or a domain controller).
type Connector interface {
	Connect(ctx context.Context, baseURL, username, password string) (Client, error)
}

// Operation is a DMR operation: name, address and parameters.
type Operation map[string]any

// Address is the DMR address of a resource, as type/name pairs.
type Address [][2]string

// MarshalJSON serializes the address as a list of single-key objects.
func (a Address) MarshalJSON() ([]byte, error) {
	out := make([]map[string]string, len(a))
	for i, p := range a {
		out[i] = map[string]string{p[0]: p[1]}
	}
	return json.Marshal(out)
}

// op builds an operation on the given address.
func op(name string, addr Address, params ...any) Operation {
	o := Operation{"operation": name, "address": addr}
	for i := 0; i+1 < len(params); i += 2 {
		o[params[i].(string)] = params[i+1]
	}
	return o
}

// OperationError is an operation that failed in the server.
type OperationError struct {
	Operation   string
	Description string
}

func (e *OperationError) Error() string {
	return fmt.Sprintf("operazione %s fallita: %s", e.Operation, e.Description)
}

// response is the DMR response.
type response struct {
	Outcome string          `json:"outcome"`
	Result  json.RawMessage `json:"result"`
	Failure json.RawMessage `json:"failure-description"`
}

func (r response) failure() string {
	var s string
	if json.Unmarshal(r.Failure, &s) == nil {
		return s
	}
	return string(r.Failure)
}

// HTTPConnector is the real Connector, with digest authentication (the management realms'
// default) or basic, depending on the server challenge.
type HTTPConnector struct {
	HTTP *http.Client
}

// Connect implements Connector; the connection is opened by the first operation.
func (c HTTPConnector) Connect(_ context.Context, baseURL, username, password string) (Client, error) {
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	return &httpClient{base: strings.TrimRight(baseURL, "/"), http: hc, auth: &digestAuth{user: username, pass: password}}, nil
}

type httpClient struct {
	base string
	http *http.Client
	auth *digestAuth
}

func (c *httpClient) Execute(ctx context.Context, o Operation) (json.RawMessage, error) {
	body, err := json.Marshal(o)
	if err != nil {
		return nil, err
	}
	newBody := func() (io.ReadCloser, string, error) {
		return io.NopCloser(bytes.NewReader(body)), "application/json", nil
	}
	name, _ := o["operation"].(string)
	return c.send(ctx, "/management", name, newBody)
}

func (c *httpClient) Upload(ctx context.Context, path string) (string, error) {
	// The multipart body is streamed from the file, reopened at every attempt: an EAR can weigh
	// hundreds of MB.
	newBody := func() (io.ReadCloser, string, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, "", err
		}
		pr, pw := io.Pipe()
		mw := multipart.NewWriter(pw)
		go func() {
			defer f.Close()
			part, err := mw.CreateFormFile("file", filepath.Base(path))
			if err == nil {
				_, err = io.Copy(part, f)
			}
			if err == nil {
				err = mw.Close()
			}
			pw.CloseWithError(err)
		}()
		return pr, mw.FormDataContentType(), nil
	}
	raw, err := c.send(ctx, "/management/add-content", "add-content", newBody)
	if err != nil {
		return "", err
	}
	var h struct {
		Bytes string `json:"BYTES_VALUE"`
	}
	if err := json.Unmarshal(raw, &h); err != nil || h.Bytes == "" {
		return "", fmt.Errorf("add-content: hash del contenuto non leggibile: %s", raw)
	}
	return h.Bytes, nil
}

// send sends the request; it answers a 401 challenge once with the new credentials.
func (c *httpClient) send(ctx context.Context, path, name string, newBody func() (io.ReadCloser, string, error)) (json.RawMessage, error) {
	for attempt := 0; ; attempt++ {
		body, ctype, err := newBody()
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, body)
		if err != nil {
			body.Close()
			return nil, err
		}
		req.Header.Set("Content-Type", ctype)
		req.Header.Set("Accept", "application/json")
		if h := c.auth.header(http.MethodPost, req.URL.RequestURI()); h != "" {
			req.Header.Set("Authorization", h)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized {
			if attempt > 0 || !c.auth.challenge(resp.Header.Values("WWW-Authenticate")) {
				return nil, errors.New("credenziali di management rifiutate (401)")
			}
			continue
		}
		var r response
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, fmt.Errorf("%s: risposta HTTP %d non valida: %.200s", name, resp.StatusCode, data)
		}
		if r.Outcome != "success" {
			return nil, &OperationError{Operation: name, Description: r.failure()}
		}
		return r.Result, nil
	}
}

// digestAuth computes the Authorization header (RFC 7616: MD5 or SHA-256, qop auth), or basic
// if the server asks for it. The challenge is reused with the nc counter until the server
// sends a new one.
type digestAuth struct {
	user, pass string

	mu                              sync.Mutex
	basic                           bool
	realm, nonce, opaque, algo, qop string
	nc                              int
}

// challenge reads the challenges of a 401 response; false if none is supported.
func (d *digestAuth) challenge(values []string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, v := range values {
		scheme, rest, _ := strings.Cut(strings.TrimSpace(v), " ")
		switch strings.ToLower(scheme) {
		case "digest":
			p := parseParams(rest)
			algo := strings.ToUpper(p["algorithm"])
			if algo == "" {
				algo = "MD5"
			}
			if algo != "MD5" && algo != "SHA-256" {
				continue
			}
			qop := ""
			for _, q := range strings.Split(p["qop"], ",") {
				if strings.TrimSpace(q) == "auth" {
					qop = "auth"
				}
			}
			d.basic, d.realm, d.nonce, d.opaque, d.algo, d.qop, d.nc = false, p["realm"], p["nonce"], p["opaque"], algo, qop, 0
			return true
		case "basic":
			d.basic = true
			return true
		}
	}
	return false
}

func (d *digestAuth) header(method, uri string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.basic {
		req, _ := http.NewRequest(method, "http://x", nil)
		req.SetBasicAuth(d.user, d.pass)
		return req.Header.Get("Authorization")
	}
	if d.nonce == "" {
		return ""
	}
	h := func(s string) string {
		var hf hash.Hash = md5.New()
		if d.algo == "SHA-256" {
			hf = sha256.New()
		}
		hf.Write([]byte(s))
		return hex.EncodeToString(hf.Sum(nil))
	}
	ha1 := h(d.user + ":" + d.realm + ":" + d.pass)
	ha2 := h(method + ":" + uri)
	d.nc++
	nc := fmt.Sprintf("%08x", d.nc)
	var resp, extra string
	if d.qop == "auth" {
		b := make([]byte, 12)
		_, _ = rand.Read(b)
		cnonce := hex.EncodeToString(b)
		resp = h(ha1 + ":" + d.nonce + ":" + nc + ":" + cnonce + ":auth:" + ha2)
		extra = fmt.Sprintf(`, qop=auth, nc=%s, cnonce="%s"`, nc, cnonce)
	} else {
		resp = h(ha1 + ":" + d.nonce + ":" + ha2)
	}
	out := fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="%s", algorithm=%s, response="%s"%s`,
		d.user, d.realm, d.nonce, uri, d.algo, resp, extra)
	if d.opaque != "" {
		out += fmt.Sprintf(`, opaque="%s"`, d.opaque)
	}
	return out
}

// parseParams reads the key=value parameters (quoted too) of a challenge.
func parseParams(s string) map[string]string {
	out := map[string]string{}
	for s = strings.TrimSpace(s); s != ""; s = strings.TrimSpace(s) {
		k, rest, ok := strings.Cut(s, "=")
		if !ok {
			break
		}
		k = strings.ToLower(strings.TrimSpace(strings.TrimLeft(k, ", ")))
		rest = strings.TrimSpace(rest)
		var v string
		if strings.HasPrefix(rest, `"`) {
			end := strings.Index(rest[1:], `"`)
			if end < 0 {
				v, rest = rest[1:], ""
			} else {
				v, rest = rest[1:end+1], rest[end+2:]
			}
		} else {
			v, rest, _ = strings.Cut(rest, ",")
			v = strings.TrimSpace(v)
		}
		out[k] = v
		s = strings.TrimPrefix(strings.TrimSpace(rest), ",")
	}
	return out
}
