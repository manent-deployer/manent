package descriptor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExportsEImport(t *testing.T) {
	base := func() *Descriptor {
		return &Descriptor{
			APIVersion: APIVersion, Application: "os", Environment: "prod",
			Artifact: Artifact{Type: "ansible", Name: "os", Version: "1", Source: "/x"},
			After:    []string{"infra-1"},
			Imports:  map[string]Import{"infra": {Deployment: "infra-1"}, "net": {Current: &ImportCurrent{Application: "network"}}},
			Deploy: Deploy{
				TargetsFrom: []ExportRef{{Import: "infra", Export: "vms"}},
				ParamsFrom:  map[string]ExportRef{"subnet": {Import: "net", Export: "subnet"}},
			},
			Exports: map[string]Export{"hosts": {From: "deploy.hosts", Format: ExportFormatTargets}, "fqdn": {From: "deploy.fqdn", Pattern: `[a-z.]+`}},
		}
	}
	if err := base().Validate(); err != nil {
		t.Fatal(err)
	}
	tests := map[string]struct {
		edit func(*Descriptor)
		want string
	}{
		"deploy non tra le dipendenze": {func(d *Descriptor) { d.After = nil }, "must be among the dependencies"},
		"import senza sorgente":        {func(d *Descriptor) { d.Imports["infra"] = Import{} }, "provide deployment or current"},
		"import non dichiarato":        {func(d *Descriptor) { d.Deploy.TargetsFrom[0].Import = "altro" }, `import "altro" is not declared`},
		"import non usato":             {func(d *Descriptor) { d.Deploy.ParamsFrom = nil }, "imports.net is not used"},
		"parametro ripetuto":           {func(d *Descriptor) { d.Deploy.Params = map[string]string{"subnet": "x"} }, "also set in params"},
		"release di se stesso": {func(d *Descriptor) {
			d.Imports["net"] = Import{Current: &ImportCurrent{Application: "os"}}
		}, "its own release in service"},
		"pattern obbligatorio":    {func(d *Descriptor) { d.Exports["fqdn"] = Export{From: "deploy.fqdn"} }, "pattern is required"},
		"sorgente non valida":     {func(d *Descriptor) { d.Exports["fqdn"] = Export{From: "deploy", Pattern: "x"} }, "must be <step>.<key>"},
		"formato sconosciuto":     {func(d *Descriptor) { d.Exports["hosts"] = Export{From: "deploy.h", Format: "csv"} }, "format \"csv\" is invalid"},
		"pattern non compilabile": {func(d *Descriptor) { d.Exports["fqdn"] = Export{From: "deploy.f", Pattern: "("} }, "exports.fqdn.pattern"},
	}
	for name, tc := range tests {
		d := base()
		tc.edit(d)
		if err := d.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}

	// Promotions and rollbacks drop the imports of given deploys.
	d := base()
	d.DropDeploymentImports()
	if _, ok := d.Imports["infra"]; ok || d.Deploy.TargetsFrom != nil || len(d.Deploy.ParamsFrom) != 1 {
		t.Errorf("import rimossi: %+v %+v", d.Imports, d.Deploy)
	}
	if _, ok := base().Imports["infra"]; !ok {
		t.Error("l'originale non deve cambiare")
	}
}

func TestExportCheck(t *testing.T) {
	targets := Export{From: "deploy.vms", Format: ExportFormatTargets, Pattern: `10\.0\.\d+\.\d+`}
	if err := targets.Check(`["10.0.0.1", {"host": "10.0.0.2", "port": 22, "labels": {"region": "eu"}}]`); err != nil {
		t.Error(err)
	}
	for _, bad := range []string{`"10.0.0.1"`, `["192.168.0.1"]`, `[{"host": ""}]`, `[{"host": "10.0.0.1", "extra": 1}]`, `[{"host": "10.0.0.1", "port": 70000}]`} {
		if err := targets.Check(bad); err == nil {
			t.Errorf("target non validi accettati: %s", bad)
		}
	}
	str := Export{From: "deploy.fqdn", Pattern: `[a-z.]+`}
	if err := str.Check("db.internal"); err != nil {
		t.Error(err)
	}
	// The pattern is anchored: a secret appended to a valid value does not pass.
	if err := str.Check("db.internal password=x"); err == nil {
		t.Error("pattern non ancorato")
	}
}

func TestReleaseConImport(t *testing.T) {
	infra := &Descriptor{APIVersion: APIVersion, Application: "infra", Environment: "prod", Artifact: Artifact{Type: "tf", Name: "infra", Version: "1", Source: "/x"},
		Deploy: Deploy{Targets: []Target{{Host: "vsphere"}}}}
	os := &Descriptor{APIVersion: APIVersion, Application: "os", Environment: "prod", Artifact: Artifact{Type: "ansible", Name: "os", Version: "1", Source: "/y"},
		Imports: map[string]Import{"infra": {Deployment: "infra"}},
		Deploy:  Deploy{TargetsFrom: []ExportRef{{Import: "infra", Export: "vms"}}}}
	r := &Release{APIVersion: APIVersion, Kind: KindRelease, ID: "r1", Deploys: []ReleaseDeploy{
		{Name: "infra", Descriptor: infra}, {Name: "os", Descriptor: os, After: []string{"infra"}},
	}}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(os.After) != 0 {
		t.Errorf("la validazione non deve cambiare il descrittore: %v", os.After)
	}
	r.Deploys[1].After = nil
	if err := r.Validate(); err == nil || !strings.Contains(err.Error(), "must be among the dependencies") {
		t.Errorf("import senza dipendenza: %v", err)
	}
}

func TestLoadReleaseConImport(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"infra.yml": "apiVersion: manent/v1\napplication: infra\nenvironment: prod\nartifact: {type: tf, name: infra, version: '1', source: /x}\ndeploy: {targets: [{host: vsphere}]}\n",
		"os.yml": "apiVersion: manent/v1\napplication: os\nenvironment: prod\nartifact: {type: ansible, name: os, version: '1', source: /y}\n" +
			"imports: {infra: {deployment: infra}}\ndeploy: {targetsFrom: [{import: infra, export: vms}]}\n",
		"release.yml": "apiVersion: manent/v1\nkind: Release\nid: r1\ndeploys:\n  - {name: infra, file: infra.yml}\n  - {name: os, file: os.yml, after: [infra]}\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := LoadReleaseYAMLFile(filepath.Join(dir, "release.yml")); err != nil {
		t.Fatalf("release con import di un deploy della release: %v", err)
	}
}
