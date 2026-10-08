package descriptor

import (
	"os"
	"testing"
)

func TestParseYAMLEsempio(t *testing.T) {
	data, err := os.ReadFile("../../examples/descriptor.yml")
	if err != nil {
		t.Fatal(err)
	}
	d, err := ParseYAML(data)
	if err != nil {
		t.Fatal(err)
	}
	if d.Deploy.Plugin != "remote-copy" || len(d.Deploy.Targets) != 1 {
		t.Errorf("descrittore inatteso: %+v", d.Deploy)
	}
}

func TestParseJSONEsempio(t *testing.T) {
	data, err := os.ReadFile("../../examples/descriptor.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseJSON(data); err != nil {
		t.Fatal(err)
	}
}

func TestValidateErrori(t *testing.T) {
	tests := map[string]string{
		"campo sconosciuto": "apiVersion: go-dart/v1\nfoo: bar\n",
		"campi obbligatori": "apiVersion: go-dart/v1\n",
		"apiVersion errata": "apiVersion: v0\napplication: a\n",
		"segreto duplicato": "apiVersion: go-dart/v1\nsecrets:\n  - {name: a, ref: env:A}\n  - {name: a, ref: env:B}\n",
	}
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseYAML([]byte(in)); err == nil {
				t.Fatal("atteso errore")
			}
		})
	}
}
