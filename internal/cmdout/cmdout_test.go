package cmdout

import (
	"strings"
	"testing"
)

func TestRedazione(t *testing.T) {
	r := NewRedactor([]string{"topsecret\n", "abc", "-----BEGIN KEY-----\nMIIEvQIBADANBgkqhkiG9w0\n-----END KEY-----"})
	for _, in := range []string{"topsecret", "dG9wc2VjcmV0", "x=topsecret;", "MIIEvQIBADANBgkqhkiG9w0"} {
		if out := r.Apply(in); strings.Contains(out, "topsecret") || strings.Contains(out, "dG9wc2VjcmV0") || strings.Contains(out, "MIIEvQ") {
			t.Errorf("%q → %q", in, out)
		}
	}
	if out := r.Apply("abc def"); out != "abc def" {
		t.Errorf("i valori troppo corti non vanno redatti: %q", out)
	}
	if !r.Contains("a topsecret b") || r.Contains("nulla") {
		t.Error("contains")
	}
}

func TestOutputs(t *testing.T) {
	out, err := ParseOutputs("# comment\na=1\r\n\nb=x=y\n")
	if err != nil || out["a"] != "1" || out["b"] != "x=y" || len(out) != 2 {
		t.Fatalf("%v %v", out, err)
	}
	for _, bad := range []string{"novalue", "=x", "a=1\na=2"} {
		if _, err := ParseOutputs(bad); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}
