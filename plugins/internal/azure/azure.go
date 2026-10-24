// Package azure groups what the Azure plugins share: the Microsoft Entra ID token (service
// principal with a client secret, workload identity, managed identity) and a client of the
// Azure Resource Manager REST API with long-running operations. It speaks HTTP directly,
// without the Azure SDK, as the other cloud plugins of manent do where an SDK is not needed.
package azure

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/manent-deployer/manent/pkg/plugin"
)

// Default endpoints of the Azure public cloud.
const (
	DefaultManagement = "https://management.azure.com"
	DefaultAuthority  = "https://login.microsoftonline.com"
	DefaultIMDS       = "http://169.254.169.254"
	// ManagementScope is the scope of the Resource Manager API (also accepted by Kudu).
	ManagementScope = "https://management.azure.com/.default"
)

// Parameters shared by the Azure plugins (deploy.params).
const (
	ParamSubscription  = "subscription"         // subscription ID (required)
	ParamResourceGroup = "resource_group"       // default resource group of the targets
	ParamTenantID      = "tenant_id"            // Microsoft Entra tenant (service principal, workload identity)
	ParamClientID      = "client_id"            // application (service principal) or user-assigned managed identity
	ParamClientSecret  = "client_secret_secret" // secret with the client secret of the service principal
)

var (
	// SubscriptionRe is a subscription ID; tenant and client IDs have the same form.
	SubscriptionRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	// ResourceGroupRe is a resource group name.
	ResourceGroupRe = regexp.MustCompile(`^[-\w._()]{0,89}[-\w_()]$`)
)

// ErrNotFound means a resource that does not exist (404).
var ErrNotFound = errors.New("not found")

// Credential selects how to obtain the token. With ClientSecret, the service principal;
// otherwise the worker's identity: workload identity (AZURE_FEDERATED_TOKEN_FILE), the managed
// identity of App Service or Container Apps (IDENTITY_ENDPOINT) or the one of the VM (IMDS).
// ClientID chooses a user-assigned managed identity.
type Credential struct {
	TenantID, ClientID, ClientSecret string
}

// CredentialFrom reads the credential parameters and resolves the client secret, at use time.
func CredentialFrom(ctx context.Context, req *plugin.Request) (Credential, error) {
	c := Credential{TenantID: req.Params[ParamTenantID], ClientID: req.Params[ParamClientID]}
	if name := req.Params[ParamClientSecret]; name != "" {
		s, err := req.Secrets.Secret(ctx, name)
		if err != nil {
			return c, err
		}
		c.ClientSecret = strings.TrimSpace(s)
	}
	return c, nil
}

// ValidateCredential checks the credential parameters.
func ValidateCredential(params map[string]string) []error {
	var errs []error
	for _, k := range []string{ParamTenantID, ParamClientID} {
		if v := params[k]; v != "" && !SubscriptionRe.MatchString(v) {
			errs = append(errs, fmt.Errorf("invalid %s %q: expected a GUID", k, v))
		}
	}
	if params[ParamClientSecret] != "" && (params[ParamTenantID] == "" || params[ParamClientID] == "") {
		errs = append(errs, fmt.Errorf("%s requires %s and %s", ParamClientSecret, ParamTenantID, ParamClientID))
	}
	return errs
}

// Auth obtains and caches the tokens, until shortly before they expire.
type Auth struct {
	Authority string
	IMDS      string
	Client    *http.Client
	// Getenv reads the worker environment (default os.Getenv); replaced in tests.
	Getenv func(string) string

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	token   string
	expires time.Time
}

func (a *Auth) getenv(k string) string {
	if a.Getenv != nil {
		return a.Getenv(k)
	}
	return os.Getenv(k)
}

func (a *Auth) client() *http.Client {
	if a.Client != nil {
		return a.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Token returns a token for the scope (e.g. https://management.azure.com/.default).
func (a *Auth) Token(ctx context.Context, c Credential, scope string) (string, error) {
	kind, tenant, client := "imds", c.TenantID, c.ClientID
	switch {
	case c.ClientSecret != "":
		kind = "secret"
	case a.getenv("AZURE_FEDERATED_TOKEN_FILE") != "":
		kind = "workload"
		tenant, client = firstOf(tenant, a.getenv("AZURE_TENANT_ID")), firstOf(client, a.getenv("AZURE_CLIENT_ID"))
	case a.getenv("IDENTITY_ENDPOINT") != "" && a.getenv("IDENTITY_HEADER") != "":
		kind = "appservice"
	}
	sum := sha256.Sum256([]byte(c.ClientSecret))
	key := strings.Join([]string{kind, tenant, client, scope, hex.EncodeToString(sum[:8])}, "|")
	a.mu.Lock()
	if t, ok := a.cache[key]; ok && time.Until(t.expires) > 5*time.Minute {
		a.mu.Unlock()
		return t.token, nil
	}
	a.mu.Unlock()

	var tok tokenResponse
	var err error
	switch kind {
	case "secret":
		tok, err = a.clientCredentials(ctx, tenant, url.Values{"client_id": {client}, "client_secret": {c.ClientSecret}}, scope)
	case "workload":
		var assertion []byte
		if assertion, err = os.ReadFile(a.getenv("AZURE_FEDERATED_TOKEN_FILE")); err != nil {
			return "", fmt.Errorf("workload identity: %w", err)
		}
		if tenant == "" || client == "" {
			return "", errors.New("workload identity: AZURE_TENANT_ID and AZURE_CLIENT_ID (or tenant_id and client_id) are required")
		}
		tok, err = a.clientCredentials(ctx, tenant, url.Values{"client_id": {client}, "client_assertion": {strings.TrimSpace(string(assertion))},
			"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"}}, scope)
	case "appservice":
		q := url.Values{"api-version": {"2019-08-01"}, "resource": {resource(scope)}}
		if client != "" {
			q.Set("client_id", client)
		}
		tok, err = a.get(ctx, a.getenv("IDENTITY_ENDPOINT")+"?"+q.Encode(), "X-IDENTITY-HEADER", a.getenv("IDENTITY_HEADER"))
	default:
		q := url.Values{"api-version": {"2018-02-01"}, "resource": {resource(scope)}}
		if client != "" {
			q.Set("client_id", client)
		}
		tok, err = a.get(ctx, firstOf(a.IMDS, DefaultIMDS)+"/metadata/identity/oauth2/token?"+q.Encode(), "Metadata", "true")
	}
	if err != nil {
		return "", fmt.Errorf("Azure token (%s): %w", kind, err)
	}
	exp := time.Now().Add(time.Hour)
	if n, err := strconv.ParseInt(strings.Trim(string(tok.ExpiresIn), `"`), 10, 64); err == nil && n > 0 {
		exp = time.Now().Add(time.Duration(n) * time.Second)
	}
	a.mu.Lock()
	if a.cache == nil {
		a.cache = map[string]cached{}
	}
	a.cache[key] = cached{token: tok.AccessToken, expires: exp}
	a.mu.Unlock()
	return tok.AccessToken, nil
}

func firstOf(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// resource is the resource of a scope, for the managed identity endpoints.
func resource(scope string) string { return strings.TrimSuffix(scope, "/.default") }

type tokenResponse struct {
	AccessToken string          `json:"access_token"`
	ExpiresIn   json.RawMessage `json:"expires_in"`
	Error       string          `json:"error"`
	Description string          `json:"error_description"`
}

func (a *Auth) clientCredentials(ctx context.Context, tenant string, form url.Values, scope string) (tokenResponse, error) {
	form.Set("grant_type", "client_credentials")
	form.Set("scope", scope)
	u := strings.TrimRight(firstOf(a.Authority, DefaultAuthority), "/") + "/" + url.PathEscape(tenant) + "/oauth2/v2.0/token"
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, err
	}
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return a.token(r)
}

func (a *Auth) get(ctx context.Context, u, header, value string) (tokenResponse, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return tokenResponse{}, err
	}
	r.Header.Set(header, value)
	return a.token(r)
}

func (a *Auth) token(r *http.Request) (tokenResponse, error) {
	var out tokenResponse
	resp, err := a.client().Do(r)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(data, &out); err != nil && resp.StatusCode == http.StatusOK {
		return out, fmt.Errorf("invalid response: %w", err)
	}
	if resp.StatusCode != http.StatusOK || out.AccessToken == "" {
		msg := out.Description
		if msg == "" {
			msg = strings.TrimSpace(string(data))
		}
		// The first line of an Entra error is enough and does not repeat trace IDs.
		msg, _, _ = strings.Cut(msg, "\r\n")
		return out, fmt.Errorf("status %d: %.300s", resp.StatusCode, msg)
	}
	return out, nil
}

// Client calls the Resource Manager API with the token of a credential.
type Client struct {
	Endpoint string
	Auth     *Auth
	Cred     Credential
	HTTP     *http.Client
	// Poll is the interval of the long-running operations (default 3s).
	Poll time.Duration
	// Hosts are the other hosts (e.g. Kudu) that may receive the token, besides the endpoint.
	Hosts []string
	// Scope is the scope of the token (default ManagementScope).
	Scope string
}

// Error is an error response of the API.
type Error struct {
	Status        int
	Code, Message string
}

func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("status %d %s: %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("status %d: %s", e.Status, e.Message)
}

// Unwrap makes a 404 an ErrNotFound.
func (e *Error) Unwrap() error {
	if e.Status == http.StatusNotFound {
		return ErrNotFound
	}
	return nil
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 2 * time.Minute}
}

func (c *Client) base() string { return strings.TrimRight(firstOf(c.Endpoint, DefaultManagement), "/") }

// Do calls path (from /subscriptions/...) with the given API version (none if empty): body is
// serialized as JSON, the response decoded into out. It returns the response headers (for Wait).
func (c *Client) Do(ctx context.Context, method, path, apiVersion string, body, out any) (http.Header, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	u := c.base() + path
	if apiVersion != "" {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		u += sep + "api-version=" + url.QueryEscape(apiVersion)
	}
	return c.Raw(ctx, method, u, rd, "application/json", out)
}

// Raw calls an absolute URL with the token: only the endpoint and the Hosts receive it.
func (c *Client) Raw(ctx context.Context, method, u string, body io.Reader, contentType string, out any) (http.Header, error) {
	if !c.allowed(u) {
		return nil, fmt.Errorf("URL %s outside the Azure endpoints: the token is not sent there", u)
	}
	tok, err := c.Auth.Token(ctx, c.Cred, firstOf(c.Scope, ManagementScope))
	if err != nil {
		return nil, err
	}
	r, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	r.Header.Set("Authorization", "Bearer "+tok)
	r.Header.Set("Accept", "application/json")
	if body != nil {
		r.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http().Do(r)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		e := &Error{Status: resp.StatusCode, Message: strings.TrimSpace(string(data))}
		var ae struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
			Code    string `json:"Code"`
			Message string `json:"Message"`
		}
		if json.Unmarshal(data, &ae) == nil {
			switch {
			case ae.Error.Message != "":
				e.Code, e.Message = ae.Error.Code, ae.Error.Message
			case ae.Message != "":
				e.Code, e.Message = ae.Code, ae.Message
			}
		}
		if len(e.Message) > 500 {
			e.Message = e.Message[:500]
		}
		return resp.Header, e
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.Header, fmt.Errorf("invalid response from %s: %w", r.URL.Path, err)
		}
	}
	return resp.Header, nil
}

// allowed reports whether u is on the endpoint or on one of the Hosts, with the same scheme.
func (c *Client) allowed(u string) bool {
	t, err := url.Parse(u)
	if err != nil {
		return false
	}
	ok := func(base string) bool {
		b, err := url.Parse(base)
		return err == nil && b.Scheme == t.Scheme && strings.EqualFold(b.Host, t.Host)
	}
	if ok(c.base()) {
		return true
	}
	for _, h := range c.Hosts {
		if ok(h) {
			return true
		}
	}
	return false
}

// Wait waits for a long-running operation started by a call that returned hdr: it reads
// Azure-AsyncOperation (status Succeeded, Failed, Canceled) or, without it, Location (202
// while running). Without either header the operation is already complete.
func (c *Client) Wait(ctx context.Context, hdr http.Header) error {
	poll := c.Poll
	if poll <= 0 {
		poll = 3 * time.Second
	}
	async, loc := hdr.Get("Azure-AsyncOperation"), hdr.Get("Location")
	for async != "" || loc != "" {
		select {
		case <-ctx.Done():
			return fmt.Errorf("operation not completed: %w", context.Cause(ctx))
		case <-time.After(poll):
		}
		if async != "" {
			var st struct {
				Status string `json:"status"`
				Error  struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if _, err := c.Raw(ctx, http.MethodGet, async, nil, "", &st); err != nil {
				return fmt.Errorf("reading the operation: %w", err)
			}
			switch strings.ToLower(st.Status) {
			case "succeeded":
				return nil
			case "failed", "canceled", "cancelled":
				return fmt.Errorf("operation %s: %s %s", strings.ToLower(st.Status), st.Error.Code, st.Error.Message)
			}
			continue
		}
		h, err := c.status(ctx, loc)
		if err != nil {
			return fmt.Errorf("reading the operation: %w", err)
		}
		if h != http.StatusAccepted {
			return nil
		}
	}
	return nil
}

// status returns the status code of a GET on u (Location of a long-running operation).
func (c *Client) status(ctx context.Context, u string) (int, error) {
	if !c.allowed(u) {
		return 0, fmt.Errorf("URL %s outside the Azure endpoints: the token is not sent there", u)
	}
	tok, err := c.Auth.Token(ctx, c.Cred, firstOf(c.Scope, ManagementScope))
	if err != nil {
		return 0, err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	r.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.http().Do(r)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return resp.StatusCode, &Error{Status: resp.StatusCode, Message: strings.TrimSpace(string(data))}
	}
	return resp.StatusCode, nil
}

// Target is an Azure resource of a deploy target: host "<name>" or "<resource group>/<name>".
type Target struct {
	ResourceGroup, Name string
}

// ParseTarget reads the resource of a target, with the default resource group of the params.
func ParseTarget(t plugin.Target, params map[string]string, nameRe *regexp.Regexp) (Target, error) {
	rg, name, ok := strings.Cut(t.Host, "/")
	if !ok {
		rg, name = params[ParamResourceGroup], t.Host
	}
	if !ResourceGroupRe.MatchString(rg) {
		return Target{}, fmt.Errorf("target %q: resource group missing or invalid (target <resource group>/<name> or parameter %s)", t.Host, ParamResourceGroup)
	}
	if !nameRe.MatchString(name) {
		return Target{}, fmt.Errorf("target %q: invalid resource name %q", t.Host, name)
	}
	return Target{ResourceGroup: rg, Name: name}, nil
}

// Settings of the plugin configuration shared by the Azure plugins.
type Settings struct {
	Management, Authority, IMDS string
}

// SettingsFrom reads management_endpoint, authority and imds_endpoint (sovereign clouds, tests).
func SettingsFrom(cfg map[string]any) (Settings, error) {
	var s Settings
	for k, dst := range map[string]*string{"management_endpoint": &s.Management, "authority": &s.Authority, "imds_endpoint": &s.IMDS} {
		raw, ok := cfg[k]
		if !ok {
			continue
		}
		v, ok := raw.(string)
		if !ok || !(strings.HasPrefix(v, "https://") || strings.HasPrefix(v, "http://")) {
			return s, fmt.Errorf("%s must be an http(s) URL", k)
		}
		*dst = strings.TrimRight(v, "/")
	}
	return s, nil
}
