package config

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestConfigurazioneRedatta(t *testing.T) {
	cfg := Default()
	cfg.applyDefaults()
	cfg.DBOS.DatabaseURL = "postgres://manent:s3cret@db:5432/manent"
	cfg.Auth.Tokens = []APIToken{{Name: "ci", SHA256: "abcdef"}}
	cfg.Plugins["custom"] = map[string]any{
		"password":     "plain",
		"password_ref": "vault:secret/x#p",
		"external":     map[string]any{"path": "/bin/p", "env": map[string]any{"API_KEY": "zzz"}},
	}
	m, paths, err := cfg.Redacted()
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(m)
	s := string(data)
	for _, leak := range []string{"s3cret", "abcdef", `"plain"`, "zzz"} {
		if strings.Contains(s, leak) {
			t.Errorf("redacted configuration contains %q: %s", leak, s)
		}
	}
	for _, kept := range []string{"vault:secret/x#p", "/bin/p", "postgres://manent:xxxxx@db:5432/manent"} {
		if !strings.Contains(s, kept) {
			t.Errorf("redacted configuration lost %q", kept)
		}
	}
	for _, p := range []string{"dbos.database_url", "auth.tokens[0].sha256", "plugins.custom.password", "plugins.custom.external.env.API_KEY"} {
		if !slices.Contains(paths, p) {
			t.Errorf("paths %v without %s", paths, p)
		}
	}
	if slices.Contains(paths, "plugins.custom.password_ref") {
		t.Errorf("a reference was redacted: %v", paths)
	}
	// Files is not part of the YAML.
	if _, ok := m["files"]; ok {
		t.Error("files in the redacted map")
	}
}
