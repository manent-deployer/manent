package clientcli

import (
	"os/exec"
	"strings"
	"testing"
)

// TestClientLeggero verifica che il client non includa il motore: DBOS, driver di database,
// plugin, SSH, server HTTP, metriche e tracing restano nel server.
func TestClientLeggero(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "github.com/joeg-ita/manent/cmd/manent").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	forbidden := []string{
		"github.com/dbos-inc/", "github.com/jackc/pgx", "modernc.org/sqlite", "golang.org/x/crypto/ssh",
		"github.com/hashicorp/go-plugin", "github.com/gin-gonic/gin", "github.com/prometheus/", "go.opentelemetry.io/",
		"github.com/joeg-ita/manent/internal/engine", "github.com/joeg-ita/manent/internal/app",
		"github.com/joeg-ita/manent/internal/steps", "github.com/joeg-ita/manent/plugins/",
		"github.com/joeg-ita/manent/internal/secrets", "github.com/joeg-ita/manent/internal/extplugin",
	}
	for _, dep := range strings.Fields(string(out)) {
		for _, f := range forbidden {
			if strings.HasPrefix(dep, f) {
				t.Errorf("il client dipende da %s", dep)
			}
		}
	}
}
