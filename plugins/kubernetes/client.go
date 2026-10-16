package kubernetes

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FieldManager is the field manager manent applies objects with (server-side apply).
const FieldManager = "manent"

// ErrNotFound means an object or resource that does not exist in the cluster.
var ErrNotFound = errors.New("non trovato")

// Credentials are the cluster access credentials, resolved at use time.
type Credentials struct {
	Token      string
	ClientCert string // PEM
	ClientKey  string // PEM
	CA         string // PEM; empty = system CAs or the plugin's ca_file
}

// API is the access to a cluster's API; abstract so tests can replace it.
type API interface {
	// Resource resolves apiVersion and kind into the resource name and says whether it is namespaced.
	Resource(ctx context.Context, apiVersion, kind string) (resource string, namespaced bool, err error)
	// Apply applies the object with server-side apply; with dryRun nothing is saved.
	Apply(ctx context.Context, obj Object, dryRun bool) (Object, error)
	Get(ctx context.Context, apiVersion, kind, namespace, name string) (Object, error)
	// Patch applies a JSON merge patch: with metadata.resourceVersion in the patch the server
	// rejects it if the object changed meanwhile.
	Patch(ctx context.Context, apiVersion, kind, namespace, name string, patch map[string]any) (Object, error)
	Delete(ctx context.Context, apiVersion, kind, namespace, name string) error
	// List lists the objects of a kind in a namespace with the label selector.
	List(ctx context.Context, apiVersion, kind, namespace, selector string) ([]Object, error)
	// Version returns the cluster version (connection and credentials check).
	Version(ctx context.Context) (string, error)
	// GroupVersions returns the APIs served by the cluster (e.g. "v1", "apps/v1").
	GroupVersions(ctx context.Context) ([]string, error)
}

// Connector opens access to a target's cluster.
type Connector interface {
	Connect(ctx context.Context, server string, creds Credentials) (API, error)
}

// HTTPConnector connects to the Kubernetes REST API without depending on client-go.
type HTTPConnector struct {
	// CAFile is a bundle of cluster CAs (e.g. /var/run/secrets/kubernetes.io/serviceaccount/ca.crt).
	CAFile string
	// InsecureSkipTLSVerify disables certificate verification (development only).
	InsecureSkipTLSVerify bool
	Timeout               time.Duration
}

// Connect implements Connector.
func (c HTTPConnector) Connect(_ context.Context, server string, creds Credentials) (API, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: c.InsecureSkipTLSVerify} //nolint:gosec // opzione esplicita per lo sviluppo
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if c.CAFile != "" {
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, fmt.Errorf("lettura ca_file: %w", err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("ca_file non contiene certificati PEM")
		}
	}
	if creds.CA != "" {
		pool = x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(creds.CA)) {
			return nil, errors.New("la CA del cluster non contiene certificati PEM")
		}
	}
	tc.RootCAs = pool
	if creds.ClientCert != "" || creds.ClientKey != "" {
		cert, err := tls.X509KeyPair([]byte(creds.ClientCert), []byte(creds.ClientKey))
		if err != nil {
			return nil, fmt.Errorf("certificato client non valido: %w", err)
		}
		tc.Certificates = []tls.Certificate{cert}
	}
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &httpAPI{
		server: strings.TrimSuffix(server, "/"),
		token:  creds.Token,
		client: &http.Client{Timeout: timeout, Transport: &http.Transport{TLSClientConfig: tc, Proxy: http.ProxyFromEnvironment}},
		cache:  map[string]apiResource{},
	}, nil
}

// serverURL builds the API address from the target (default port 6443).
func serverURL(host string, port int) string {
	if strings.HasPrefix(host, "https://") || strings.HasPrefix(host, "http://") {
		return host
	}
	if port == 0 {
		port = 6443
	}
	return "https://" + net.JoinHostPort(host, strconv.Itoa(port))
}

type apiResource struct {
	name       string
	namespaced bool
}

type httpAPI struct {
	server string
	token  string
	client *http.Client
	mu     sync.Mutex
	// cache of resources per group/version (discovery).
	cache  map[string]apiResource
	loaded map[string]bool
}

// groupPath returns the API path of an apiVersion ("v1" → /api/v1).
func groupPath(apiVersion string) string {
	if !strings.Contains(apiVersion, "/") {
		return "/api/" + apiVersion
	}
	return "/apis/" + apiVersion
}

func (a *httpAPI) Resource(ctx context.Context, apiVersion, kind string) (string, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := apiVersion + "/" + kind
	if r, ok := a.cache[key]; ok {
		return r.name, r.namespaced, nil
	}
	if a.loaded == nil {
		a.loaded = map[string]bool{}
	}
	if !a.loaded[apiVersion] {
		var list struct {
			Resources []struct {
				Name       string `json:"name"`
				Kind       string `json:"kind"`
				Namespaced bool   `json:"namespaced"`
			} `json:"resources"`
		}
		if err := a.do(ctx, http.MethodGet, groupPath(apiVersion), "", nil, &list); err != nil {
			if errors.Is(err, ErrNotFound) {
				return "", false, fmt.Errorf("apiVersion %s non disponibile nel cluster", apiVersion)
			}
			return "", false, fmt.Errorf("discovery di %s: %w", apiVersion, err)
		}
		for _, r := range list.Resources {
			if strings.Contains(r.Name, "/") { // subresources (status, scale)
				continue
			}
			a.cache[apiVersion+"/"+r.Kind] = apiResource{name: r.Name, namespaced: r.Namespaced}
		}
		a.loaded[apiVersion] = true
	}
	r, ok := a.cache[key]
	if !ok {
		return "", false, fmt.Errorf("kind %s non disponibile in %s", kind, apiVersion)
	}
	return r.name, r.namespaced, nil
}

// path builds the path of an object (empty name = collection).
func (a *httpAPI) path(ctx context.Context, apiVersion, kind, namespace, name string) (string, error) {
	res, namespaced, err := a.Resource(ctx, apiVersion, kind)
	if err != nil {
		return "", err
	}
	p := groupPath(apiVersion)
	if namespaced {
		if namespace == "" {
			return "", fmt.Errorf("%s %s: namespace mancante", kind, name)
		}
		p += "/namespaces/" + url.PathEscape(namespace)
	}
	p += "/" + res
	if name != "" {
		p += "/" + url.PathEscape(name)
	}
	return p, nil
}

func (a *httpAPI) Apply(ctx context.Context, obj Object, dryRun bool) (Object, error) {
	p, err := a.path(ctx, obj.APIVersion(), obj.Kind(), obj.Namespace(), obj.Name())
	if err != nil {
		return nil, err
	}
	q := url.Values{"fieldManager": {FieldManager}, "force": {"true"}}
	if dryRun {
		q.Set("dryRun", "All")
	}
	body, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	var out Object
	if err := a.do(ctx, http.MethodPatch, p+"?"+q.Encode(), "application/apply-patch+yaml", body, &out); err != nil {
		return nil, fmt.Errorf("%s: %w", obj.Ref(), err)
	}
	return out, nil
}

func (a *httpAPI) Get(ctx context.Context, apiVersion, kind, namespace, name string) (Object, error) {
	p, err := a.path(ctx, apiVersion, kind, namespace, name)
	if err != nil {
		return nil, err
	}
	var out Object
	if err := a.do(ctx, http.MethodGet, p, "", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (a *httpAPI) Patch(ctx context.Context, apiVersion, kind, namespace, name string, patch map[string]any) (Object, error) {
	p, err := a.path(ctx, apiVersion, kind, namespace, name)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return nil, err
	}
	var out Object
	q := url.Values{"fieldManager": {FieldManager}}
	if err := a.do(ctx, http.MethodPatch, p+"?"+q.Encode(), "application/merge-patch+json", body, &out); err != nil {
		return nil, fmt.Errorf("%s/%s %s: %w", kind, name, namespace, err)
	}
	return out, nil
}

func (a *httpAPI) Delete(ctx context.Context, apiVersion, kind, namespace, name string) error {
	p, err := a.path(ctx, apiVersion, kind, namespace, name)
	if err != nil {
		return err
	}
	body := []byte(`{"kind":"DeleteOptions","apiVersion":"v1","propagationPolicy":"Background"}`)
	err = a.do(ctx, http.MethodDelete, p, "application/json", body, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (a *httpAPI) List(ctx context.Context, apiVersion, kind, namespace, selector string) ([]Object, error) {
	p, err := a.path(ctx, apiVersion, kind, namespace, "")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []Object `json:"items"`
	}
	if err := a.do(ctx, http.MethodGet, p+"?"+url.Values{"labelSelector": {selector}}.Encode(), "", nil, &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (a *httpAPI) Version(ctx context.Context) (string, error) {
	var v struct {
		GitVersion string `json:"gitVersion"`
	}
	if err := a.do(ctx, http.MethodGet, "/version", "", nil, &v); err != nil {
		return "", err
	}
	// /version answers even without credentials: discovery checks them.
	if _, _, err := a.Resource(ctx, "v1", "ConfigMap"); err != nil {
		return "", err
	}
	return v.GitVersion, nil
}

func (a *httpAPI) GroupVersions(ctx context.Context) ([]string, error) {
	var list struct {
		Groups []struct {
			Versions []struct {
				GroupVersion string `json:"groupVersion"`
			} `json:"versions"`
		} `json:"groups"`
	}
	if err := a.do(ctx, http.MethodGet, "/apis", "", nil, &list); err != nil {
		return nil, err
	}
	out := []string{"v1"}
	for _, g := range list.Groups {
		for _, v := range g.Versions {
			out = append(out, v.GroupVersion)
		}
	}
	return out, nil
}

// do runs an API request and decodes the response; errors carry the Kubernetes Status message.
func (a *httpAPI) do(ctx context.Context, method, path, contentType string, body []byte, out any) error {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.server+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var st struct {
			Message string `json:"message"`
			Reason  string `json:"reason"`
		}
		_ = json.Unmarshal(data, &st)
		msg := st.Message
		if msg == "" {
			msg = strings.TrimSpace(string(data))
		}
		if resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("%w: %s", ErrNotFound, msg)
		}
		return fmt.Errorf("API %s %s: stato %d: %s", method, strings.SplitN(path, "?", 2)[0], resp.StatusCode, msg)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}
