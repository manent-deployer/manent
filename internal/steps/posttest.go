package steps

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/joeg-ita/go-dart/internal/pipeline"
)

// HTTPDoer astrae il client HTTP per facilitare i test.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// PostTest esegue i controlli HTTP dichiarati nel descrittore dopo il deploy.
// I retry sono gestiti da DBOS tramite max_retries dello step.
type PostTest struct {
	Client HTTPDoer
	Logger *slog.Logger
}

// Name restituisce il nome dello step.
func (PostTest) Name() string { return "post-test" }

// Run verifica che ogni endpoint risponda con lo status atteso (default 200).
func (p PostTest) Run(ctx context.Context, in pipeline.Input) (pipeline.Output, error) {
	out := pipeline.Output{}
	for _, c := range in.Descriptor.PostTest.HTTP {
		want := c.ExpectStatus
		if want == 0 {
			want = http.StatusOK
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
		if err != nil {
			return nil, err
		}
		resp, err := p.Client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("post-test %s: %w", c.URL, err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			return nil, fmt.Errorf("post-test %s: status %d, atteso %d", c.URL, resp.StatusCode, want)
		}
		out[c.URL] = "passed"
	}
	p.Logger.InfoContext(ctx, "post-test superati", "deployment", in.DeploymentID, "checks", len(out))
	return out, nil
}
