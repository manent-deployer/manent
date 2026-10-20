package clientcli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joeg-ita/manent/internal/service"
)

func TestLetturaDirectoryDellaPolicy(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"governance.rego":      "package manent.governance",
		"governance_test.rego": "package manent.governance_test",
		"prod/data.yaml":       "a: 1",
		"README.md":            "ignorato",
		".git/config":          "ignorato",
	} {
		p := filepath.Join(dir, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files, err := readPolicyDir(dir)
	if err != nil || len(files) != 3 || files["prod/data.yaml"] != "a: 1" {
		t.Fatalf("file letti: %v %v", files, err)
	}
	if _, err := readPolicyDir(t.TempDir()); err == nil {
		t.Error("directory vuota accettata")
	}
}

func TestDiffDelleRevisioni(t *testing.T) {
	out := diffFiles(
		map[string]string{"a.rego": "x\ny\nz\n", "old.rego": "o\n", "same.rego": "s\n"},
		map[string]string{"a.rego": "x\nY\nz\n", "new.rego": "n\n", "same.rego": "s\n"}, "1", "2")
	for _, want := range []string{"--- 1/a.rego", "- y", "+ Y", "  x", "--- 1/new.rego", "+ n", "--- 1/old.rego", "- o"} {
		if !strings.Contains(out, want) {
			t.Errorf("manca %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "same.rego") {
		t.Errorf("file uguale nel diff:\n%s", out)
	}
}

func TestRevisioneDaArgomento(t *testing.T) {
	if n, err := parseRevision("current"); err != nil || n != 0 {
		t.Errorf("current: %d %v", n, err)
	}
	if n, err := parseRevision("12"); err != nil || n != 12 {
		t.Errorf("12: %d %v", n, err)
	}
	for _, v := range []string{"0", "-1", "x"} {
		if _, err := parseRevision(v); err == nil {
			t.Errorf("%s accettata", v)
		}
	}
}

func TestStampaDegliAccessi(t *testing.T) {
	until := time.Date(2026, 10, 20, 18, 0, 0, 0, time.UTC)
	var b bytes.Buffer
	printAccess(&b, &service.PolicyAccess{
		Revision: "2:def", Subject: "oidc:alice", Compared: "1:abc",
		Grants: []service.Grant{
			{Name: "team-shop", Role: "deployer", Applications: []string{"shop-*"}, Environments: []string{"*"}},
			{Name: "oncall", Actions: []string{"deployment.read", "deployment.cancel"}, Applications: []string{"*"}, Environments: []string{"prod"}, Until: &until},
		},
		Gained:   []service.Grant{{Name: "oncall", Actions: []string{"deployment.cancel"}, Applications: []string{"*"}, Environments: []string{"prod"}}},
		Warnings: []string{"grant x: unknown action"},
		Check:    &service.AccessOutcome{GrantedBy: "team-shop", Denied: []string{"contractors cannot change production"}},
	})
	out := b.String()
	for _, want := range []string{"Identità oidc:alice, revisione 2:def", "team-shop", "deployer", "deployment.read,deployment.cancel",
		"permesso scartato: grant x", "revisione attiva 1:abc", "+ oncall", "negato dalla policy", "- contractors cannot change production"} {
		if !strings.Contains(out, want) {
			t.Errorf("manca %q in:\n%s", want, out)
		}
	}
}
