package clientcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
