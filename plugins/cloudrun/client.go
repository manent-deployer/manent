package cloudrun

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/joeg-ita/manent/pkg/plugin"
)

// DefaultEndpoint è l'endpoint dell'Admin API v2 di Cloud Run.
const DefaultEndpoint = "https://run.googleapis.com"

// scope è l'ambito OAuth richiesto dall'API.
const scope = "https://www.googleapis.com/auth/cloud-platform"

// HTTPConnector parla con l'API REST di Cloud Run. Le credenziali sono la chiave del service
// account nel segreto credentials_secret o, in sua assenza, le Application Default Credentials
// (GOOGLE_APPLICATION_CREDENTIALS, workload identity, metadata server), lette una volta.
type HTTPConnector struct {
	Endpoint string
	// Client è il client HTTP (default: timeout di 60s).
	Client *http.Client

	mu  sync.Mutex
	adc oauth2.TokenSource
}

// Connect costruisce il client con le credenziali della richiesta.
func (c *HTTPConnector) Connect(ctx context.Context, req *plugin.Request) (API, error) {
	ts, err := c.tokenSource(ctx, req)
	if err != nil {
		return nil, err
	}
	hc := c.Client
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	return &httpAPI{endpoint: strings.TrimRight(c.Endpoint, "/"), ts: ts, client: hc}, nil
}

func (c *HTTPConnector) tokenSource(ctx context.Context, req *plugin.Request) (oauth2.TokenSource, error) {
	// Le credenziali sono costruite fuori dal context della richiesta: il token source le
	// riusa per rinnovare il token anche dopo la fine dello step.
	base := context.WithoutCancel(ctx)
	if name := req.Params[ParamCredentialsSecret]; name != "" {
		key, err := req.Secrets.Secret(ctx, name)
		if err != nil {
			return nil, err
		}
		// Solo chiavi di service account: altri tipi di credenziali (es. external account)
		// farebbero chiamare a manent URL scelti da chi scrive il segreto.
		creds, err := google.CredentialsFromJSONWithType(base, []byte(key), google.ServiceAccount, scope)
		if err != nil {
			return nil, fmt.Errorf("credenziali nel segreto %s: %w", name, err)
		}
		return creds.TokenSource, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.adc == nil {
		creds, err := google.FindDefaultCredentials(base, scope)
		if err != nil {
			return nil, fmt.Errorf("Application Default Credentials: %w (oppure indicare %q)", err, ParamCredentialsSecret)
		}
		c.adc = creds.TokenSource
	}
	return c.adc, nil
}

type httpAPI struct {
	endpoint string
	ts       oauth2.TokenSource
	client   *http.Client
}

// apiError è l'errore restituito dalle API Google.
type apiError struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

func (a *httpAPI) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	tok, err := a.ts.Token()
	if err != nil {
		return fmt.Errorf("token di accesso: %w", err)
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	u := a.endpoint + "/v2/" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	r, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	tok.SetAuthHeader(r)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e apiError
		msg := strings.TrimSpace(string(data))
		if json.Unmarshal(data, &e) == nil && e.Error.Message != "" {
			msg = e.Error.Message
		}
		if resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("%w: %s", ErrNotFound, msg)
		}
		return fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, msg)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

func (a *httpAPI) GetService(ctx context.Context, name string) (Object, error) {
	var o Object
	return o, a.do(ctx, http.MethodGet, name, nil, nil, &o)
}

func (a *httpAPI) UpdateService(ctx context.Context, name string, svc Object, validateOnly bool) (*Operation, error) {
	q := url.Values{}
	if validateOnly {
		q.Set("validateOnly", "true")
	}
	var op Operation
	return &op, a.do(ctx, http.MethodPatch, name, q, svc, &op)
}

func (a *httpAPI) GetOperation(ctx context.Context, name string) (*Operation, error) {
	var op Operation
	return &op, a.do(ctx, http.MethodGet, name, nil, nil, &op)
}

func (a *httpAPI) GetRevision(ctx context.Context, name string) (Object, error) {
	var o Object
	return o, a.do(ctx, http.MethodGet, name, nil, nil, &o)
}

func (a *httpAPI) ListRevisions(ctx context.Context, service string) ([]Object, error) {
	var all []Object
	token := ""
	for {
		q := url.Values{"pageSize": {"200"}}
		if token != "" {
			q.Set("pageToken", token)
		}
		var page struct {
			Revisions     []Object `json:"revisions"`
			NextPageToken string   `json:"nextPageToken"`
		}
		if err := a.do(ctx, http.MethodGet, service+"/revisions", q, nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Revisions...)
		if page.NextPageToken == "" {
			return all, nil
		}
		token = page.NextPageToken
	}
}
