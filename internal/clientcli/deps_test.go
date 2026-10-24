package clientcli

import (
	"os/exec"
	"strings"
	"testing"
)

// TestClientLeggero checks that the client does not include the engine: DBOS, database
// drivers, plugins, SSH, cloud SDKs, HTTP server, metrics and tracing stay in the server.
func TestClientLeggero(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "github.com/manent-deployer/manent/cmd/manent").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	forbidden := []string{
		"github.com/dbos-inc/", "github.com/jackc/pgx", "modernc.org/sqlite", "golang.org/x/crypto/ssh",
		"github.com/hashicorp/go-plugin", "github.com/gin-gonic/gin", "github.com/prometheus/", "go.opentelemetry.io/",
		"github.com/manent-deployer/manent/internal/engine", "github.com/manent-deployer/manent/internal/app",
		"github.com/manent-deployer/manent/internal/steps", "github.com/manent-deployer/manent/plugins/",
		"github.com/manent-deployer/manent/internal/secrets", "github.com/manent-deployer/manent/internal/extplugin",
		"github.com/manent-deployer/manent/internal/webui", "github.com/aws/", "golang.org/x/oauth2/google",
		"github.com/open-policy-agent/", "github.com/manent-deployer/manent/internal/policy", "github.com/manent-deployer/manent/internal/cluster",
	}
	for _, dep := range strings.Fields(string(out)) {
		for _, f := range forbidden {
			if strings.HasPrefix(dep, f) {
				t.Errorf("il client dipende da %s", dep)
			}
		}
	}
}
