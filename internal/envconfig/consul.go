package envconfig

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ConsulProvider reads the configuration from the Consul KV store through the HTTP API.
type ConsulProvider struct {
	// Address is the URL of the Consul agent (e.g. http://127.0.0.1:8500).
	Address string
	// Token is the ACL token sent in the X-Consul-Token header (empty if ACLs are disabled).
	Token string
	// Datacenter is optional.
	Datacenter string
	// KeyTemplate is the key template, with {tenant}, {application}, {artifact} and {environment}
	// placeholders.
	KeyTemplate string
	Client      *http.Client
}

// consulKV is an element of the GET /v1/kv/<key> response.
type consulKV struct {
	Key         string
	Value       string // base64
	ModifyIndex uint64
}

// Get reads and decodes the key of the artifact in the application/environment; the revision
// is the ModifyIndex.
func (p *ConsulProvider) Get(ctx context.Context, k Key) (*Document, error) {
	return readKey(p.KeyTemplate, k, func(key string) (*Document, error) { return p.read(ctx, key) })
}

// read reads and decodes a Consul key.
func (p *ConsulProvider) read(ctx context.Context, key string) (*Document, error) {
	u, err := url.Parse(strings.TrimRight(p.Address, "/") + "/v1/kv/" + strings.TrimLeft(key, "/"))
	if err != nil {
		return nil, fmt.Errorf("invalid Consul address: %w", err)
	}
	if p.Datacenter != "" {
		u.RawQuery = url.Values{"dc": {p.Datacenter}}.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	if p.Token != "" {
		req.Header.Set("X-Consul-Token", p.Token)
	}
	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reading from Consul: %w", err)
	}
	defer resp.Body.Close()

	source := "consul:" + key
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, fmt.Errorf("%w: %s", ErrNotFound, source)
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("reading from Consul %s: status %d: %s", key, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var kvs []consulKV
	if err := json.NewDecoder(resp.Body).Decode(&kvs); err != nil {
		return nil, fmt.Errorf("invalid Consul response: %w", err)
	}
	if len(kvs) == 0 || kvs[0].Value == "" {
		return nil, fmt.Errorf("%w: %s (empty key)", ErrNotFound, source)
	}
	raw, err := base64.StdEncoding.DecodeString(kvs[0].Value)
	if err != nil {
		return nil, fmt.Errorf("invalid Consul value: %w", err)
	}
	cfg, err := Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	return &Document{Config: cfg, Source: source, Revision: strconv.FormatUint(kvs[0].ModifyIndex, 10)}, nil
}
