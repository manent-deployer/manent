package steps

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/joeg-ita/manent/internal/pipeline"
)

// HTTPDoer abstracts the HTTP client to ease testing.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// PostTest runs the HTTP checks declared in the descriptor after the deploy.
// Retries are handled by DBOS through the step's max_retries.
type PostTest struct {
	Client HTTPDoer
	Logger *slog.Logger
}

// Name returns the step name.
func (PostTest) Name() string { return "post-test" }

// Run checks that every endpoint answers with the expected status (default 200).
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
