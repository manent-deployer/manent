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

// ConsulProvider legge la configurazione dal KV store di Consul tramite l'API HTTP.
type ConsulProvider struct {
	// Address è l'URL dell'agent Consul (es. http://127.0.0.1:8500).
	Address string
	// Token ACL inviato nell'header X-Consul-Token (vuoto se le ACL sono disabilitate).
	Token string
	// Datacenter opzionale.
	Datacenter string
	// KeyTemplate è il modello della chiave, con segnaposto {application} e {environment}.
	KeyTemplate string
	Client      *http.Client
}

// consulKV è un elemento della risposta di GET /v1/kv/<key>.
type consulKV struct {
	Key         string
	Value       string // base64
	ModifyIndex uint64
}

// Get legge e decodifica la chiave dell'applicazione/ambiente; la revisione è il ModifyIndex.
func (p *ConsulProvider) Get(ctx context.Context, application, environment string) (*Document, error) {
	key := expandKey(p.KeyTemplate, application, environment)
	u, err := url.Parse(strings.TrimRight(p.Address, "/") + "/v1/kv/" + strings.TrimLeft(key, "/"))
	if err != nil {
		return nil, fmt.Errorf("indirizzo Consul non valido: %w", err)
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
		return nil, fmt.Errorf("lettura da Consul: %w", err)
	}
	defer resp.Body.Close()

	source := "consul:" + key
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, fmt.Errorf("%w: %s", ErrNotFound, source)
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("lettura da Consul %s: status %d: %s", key, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var kvs []consulKV
	if err := json.NewDecoder(resp.Body).Decode(&kvs); err != nil {
		return nil, fmt.Errorf("risposta Consul non valida: %w", err)
	}
	if len(kvs) == 0 || kvs[0].Value == "" {
		return nil, fmt.Errorf("%w: %s (chiave vuota)", ErrNotFound, source)
	}
	raw, err := base64.StdEncoding.DecodeString(kvs[0].Value)
	if err != nil {
		return nil, fmt.Errorf("valore Consul non valido: %w", err)
	}
	cfg, err := Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	return &Document{Config: cfg, Source: source, Revision: strconv.FormatUint(kvs[0].ModifyIndex, 10)}, nil
}
