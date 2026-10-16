package api

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestSpecificaOpenAPI checks that api/openapi.yaml describes exactly the server routes:
// the spec is the contract of the UI and of the clients, so it must not drift from the code.
func TestSpecificaOpenAPI(t *testing.T) {
	raw, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("specifica non valida: %v", err)
	}
	var documented []string
	for p, ops := range spec.Paths {
		for m := range ops {
			documented = append(documented, strings.ToUpper(m)+" "+p)
		}
	}
	param := regexp.MustCompile(`:([a-z]+)`)
	var served []string
	for _, r := range newUIRouter(&mockService{}).Routes() {
		served = append(served, r.Method+" "+param.ReplaceAllString(r.Path, "{$1}"))
	}
	slices.Sort(documented)
	slices.Sort(served)
	for _, s := range served {
		if !slices.Contains(documented, s) {
			t.Errorf("rotta non documentata in api/openapi.yaml: %s", s)
		}
	}
	for _, d := range documented {
		if !slices.Contains(served, d) {
			t.Errorf("rotta documentata ma non servita: %s", d)
		}
	}
}
