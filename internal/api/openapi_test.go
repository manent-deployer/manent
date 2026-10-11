package api

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestSpecificaOpenAPI verifica che api/openapi.yaml descriva esattamente le rotte del server:
// la specifica è il contratto della UI e dei client, quindi non deve divergere dal codice.
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
