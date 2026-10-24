package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/manent-deployer/manent/internal/config"
	"github.com/manent-deployer/manent/pkg/plugin"
)

// A plugin with its own queue is a stub outside the workers that serve it: no binary needed,
// but selection by artifact type still works.
func TestPluginConCoda(t *testing.T) {
	cfg := config.Default()
	cfg.Plugins = map[string]map[string]any{
		"ansible": {"external": map[string]any{"command": "/nonexistent/manent-plugin-ansible"}, "artifact_types": []any{"ansible"}, "queue": "ansible-pool"},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg, err := NewPlugins(cfg, log, false)
	if err != nil {
		t.Fatalf("the API must not need the binary of a queued plugin: %v", err)
	}
	defer reg.Close()
	d, ok := reg.Get("ansible")
	if !ok || !d.Supports("ansible") || d.Supports("application") {
		t.Fatalf("stub: %v %v", d, ok)
	}
	_, err = d.Deploy(context.Background(), &plugin.Request{})
	if !errors.Is(err, plugin.ErrPermanent) {
		t.Errorf("a deploy reaching a worker that does not serve the plugin fails without retries: %v", err)
	}
	// A worker that serves the plugin activates the real one, which needs its binary.
	cfg.DBOS.Plugins = []string{"ansible"}
	if _, err := NewPlugins(cfg, log, true); err == nil {
		t.Error("the serving worker must activate the external plugin (missing binary)")
	}
	// A worker that does not list it gets the stub too.
	cfg.DBOS.Plugins = nil
	reg2, err := NewPlugins(cfg, log, true)
	if err != nil {
		t.Fatal(err)
	}
	defer reg2.Close()
}
