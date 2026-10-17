package kubernetes

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/joeg-ita/manent/pkg/plugin"
)

// writeChart packages a chart like "helm package": a .tgz with <name>/... .
func writeChart(t *testing.T, files map[string]string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "shop-1.0.0.tgz")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := gzip.NewWriter(f)
	tw := tar.NewWriter(zw)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []interface{ Close() error }{tw, zw, f} {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

// shopChart is a minimal chart: a Deployment with image and replicas from the values, a
// ConfigMap only if the cluster serves monitoring.coreos.com/v1, a hook and a test.
var shopChart = map[string]string{
	"shop/Chart.yaml":       "apiVersion: v2\nname: shop\nversion: 1.0.0\n",
	"shop/values.yaml":      "replicas: 2\nimage: {repository: registry/shop, tag: dev}\n",
	"shop/values-prod.yaml": "replicas: 1\n",
	"shop/templates/deployment.yaml": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Release.Name }}
  namespace: {{ .Release.Namespace }}
  labels: {app.kubernetes.io/managed-by: {{ .Release.Service }}}
spec:
  replicas: {{ .Values.replicas }}
  template:
    spec:
      containers:
        - name: web
          image: "{{ .Values.image.repository }}:{{ .Values.image.tag }}"
`,
	"shop/templates/monitoring.yaml": `{{- if .Capabilities.APIVersions.Has "monitoring.coreos.com/v1" }}
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Release.Name }}-monitoring
data:
  kube: {{ .Capabilities.KubeVersion.Version | quote }}
{{- end }}
`,
	"shop/templates/migrate.yaml": `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Release.Name }}-migrate
  annotations: {"helm.sh/hook": pre-upgrade}
`,
	"shop/templates/tests/ping.yaml": `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Release.Name }}-test
  annotations: {"helm.sh/hook": test}
`,
}

func helmRequest(t *testing.T, server, version, chart string) *plugin.Request {
	req := request(server, version, "", t)
	req.Artifact.LocalPath = chart
	req.Params[ParamHelmValuesFiles] = "values-prod.yaml"
	req.Params[ParamHelmValues] = "image:\n  repository: registry.example.com/shop\n"
	req.Params[ParamHelmValuePrefix+"image.tag"] = version
	return req
}

func TestHelmReale(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm non installato")
	}
	ctx := context.Background()
	p, fc, server := setup(t)
	chart := writeChart(t, shopChart)
	req := helmRequest(t, server, "1.0.0", chart)
	if err := p.Validate(req); err != nil {
		t.Fatal(err)
	}
	res, err := p.Deploy(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	d := fc.get(deployPath)
	if image(d) != "registry.example.com/shop:1.0.0" || num(d, "spec", "replicas") != 1 {
		t.Errorf("deployment: immagine %q, repliche %d", image(d), num(d, "spec", "replicas"))
	}
	if d.str("metadata", "labels", LabelManagedBy) != managedBy {
		t.Errorf("etichetta managed-by: %v", d.metadata()["labels"])
	}
	if cm := fc.get("/api/v1/namespaces/shop/configmaps/shop-monitoring"); cm.str("data", "kube") != "v1.31.2" {
		t.Errorf("Capabilities non dal cluster: %v", cm)
	}
	if fc.get("/api/v1/namespaces/shop/configmaps/shop-migrate") != nil || fc.get("/api/v1/namespaces/shop/configmaps/shop-test") != nil {
		t.Error("hook applicati")
	}
	if got := res.Targets[0].Details[DetailHooksSkipped]; got != "ConfigMap/shop-migrate" {
		t.Errorf("hook saltati: %q", got)
	}

	// The rollback applies again the objects saved in the history, without Helm.
	rel, err := p.CurrentRelease(ctx, req)
	if err != nil || rel == nil {
		t.Fatalf("release: %+v, %v", rel, err)
	}
	req2 := helmRequest(t, server, "2.0.0", chart)
	if _, err := p.Deploy(ctx, req2); err != nil {
		t.Fatal(err)
	}
	p.helm.Command = "/inesistente/helm"
	if _, err := p.Rollback(ctx, req2, *rel); err != nil {
		t.Fatal(err)
	}
	if img := image(fc.get(deployPath)); img != "registry.example.com/shop:1.0.0" {
		t.Errorf("dopo il rollback: %s", img)
	}

	// With helm_hooks: apply the hooks (not the tests) are normal objects.
	p.helm.Command = ""
	req3 := helmRequest(t, server, "3.0.0", chart)
	req3.Params[ParamHelmHooks] = "apply"
	if _, err := p.Deploy(ctx, req3); err != nil {
		t.Fatal(err)
	}
	if fc.get("/api/v1/namespaces/shop/configmaps/shop-migrate") == nil || fc.get("/api/v1/namespaces/shop/configmaps/shop-test") != nil {
		t.Error("helm_hooks: apply")
	}

	// A template with errors reports Helm's message.
	broken := map[string]string{}
	for k, v := range shopChart {
		broken[k] = v
	}
	broken["shop/templates/bad.yaml"] = "{{ .Values.missing.field }}"
	if _, err := p.DryRun(ctx, helmRequest(t, server, "4.0.0", writeChart(t, broken))); err == nil || !strings.Contains(err.Error(), "bad.yaml") {
		t.Errorf("template errato: %v", err)
	}
}

// fakeHelm writes a script that records the received arguments and values and prints fixed
// manifests: it checks the command line without depending on Helm.
func fakeHelm(t *testing.T) (string, string) {
	if runtime.GOOS == "windows" {
		t.Skip("script di shell")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "args")
	script := `#!/bin/sh
printf '%s\n' "$@" > ` + log + `
for a in "$@"; do case "$a" in */values.yaml) cat "$a" >> ` + log + `.values;; esac; done
env > ` + log + `.env
cat <<'EOF'
---
# Source: shop/templates/cm.yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: shop-config
EOF
`
	cmd := filepath.Join(dir, "helm")
	if err := os.WriteFile(cmd, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return cmd, log
}

func TestHelmRigaDiComando(t *testing.T) {
	cmd, log := fakeHelm(t)
	ctx := context.Background()
	p, fc, server := setup(t)
	p.helm.Command = cmd
	t.Setenv("MANENT_TOKEN_DI_PROVA", "segreto")
	req := helmRequest(t, server, "1.0.0", writeChart(t, shopChart))
	req.Params[ParamHelmRelease] = "shop-eu"
	req.Params[ParamHelmSkipCRDs] = "true"
	if _, err := p.Deploy(ctx, req); err != nil {
		t.Fatal(err)
	}
	if fc.get("/api/v1/namespaces/shop/configmaps/shop-config") == nil {
		t.Fatal("ConfigMap non applicata")
	}
	raw, _ := os.ReadFile(log)
	args := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if args[0] != "template" || args[1] != "shop-eu" || !strings.HasSuffix(args[2], "/chart/shop") {
		t.Errorf("argomenti: %q", args)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"--namespace shop", "--kube-version v1.31.2", "--api-versions apps/v1", "--api-versions v1", "--skip-tests", "values-prod.yaml"} {
		if !strings.Contains(joined, want) {
			t.Errorf("manca %q in %s", want, joined)
		}
	}
	if strings.Contains(joined, "--include-crds") {
		t.Error("helm_skip_crds ignorato")
	}
	vals, _ := os.ReadFile(log + ".values")
	if !strings.Contains(string(vals), "tag: 1.0.0") || !strings.Contains(string(vals), "repository: registry.example.com/shop") {
		t.Errorf("values: %s", vals)
	}
	env, _ := os.ReadFile(log + ".env")
	if strings.Contains(string(env), "MANENT_TOKEN_DI_PROVA") {
		t.Error("helm ha ereditato l'ambiente del worker")
	}

	p.helm.SHA256 = strings.Repeat("0", 64)
	if _, err := p.Deploy(ctx, helmRequest(t, server, "2.0.0", writeChart(t, shopChart))); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Errorf("sha256 del binario: %v", err)
	}
}

func TestHelmValuesEValidazione(t *testing.T) {
	vals, err := helmValues(map[string]string{
		ParamHelmValues:                      "image: {repository: r, tag: x}\nreplicas: 1\n",
		ParamHelmValuePrefix + "image.tag":   "1.2.0",
		ParamHelmValuePrefix + "replicas":    "3",
		ParamHelmValuePrefix + "ingress.on":  "true",
		ParamHelmValuePrefix + "annotations": "{a: b}",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"image": map[string]any{"repository": "r", "tag": "1.2.0"}, "replicas": 3,
		"ingress": map[string]any{"on": true}, "annotations": map[string]any{"a": "b"},
	}
	if !reflect.DeepEqual(vals, want) {
		t.Errorf("values: %#v", vals)
	}
	errs := validateHelm(map[string]string{
		ParamHelmRelease: "Shop_EU", ParamHelmHooks: "sempre", ParamHelmValuesFiles: "values.yaml, ../segreti.yaml",
		ParamHelmValues: "- lista", ParamHelmValuePrefix + "a..b": "1",
	})
	if len(errs) != 4 {
		t.Errorf("errori di validazione: %v", errs)
	}

	// A manifest archive is not a chart; a chart with external paths is rejected.
	manifests := writeChart(t, map[string]string{"deploy/app.yaml": "kind: ConfigMap"})
	if ok, err := isChart(manifests); ok || err != nil {
		t.Errorf("archivio di manifest riconosciuto come chart: %v", err)
	}
	if ok, _ := isChart(writeChart(t, shopChart)); !ok {
		t.Error("chart non riconosciuto")
	}
	evil := writeChart(t, map[string]string{"shop/Chart.yaml": "name: shop", "../../fuori.yaml": "x"})
	if _, err := extractChart(evil, t.TempDir()); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("percorso esterno: %v", err)
	}
}

func TestHelmSlot(t *testing.T) {
	cmd, log := fakeHelm(t)
	p, _, server := setup(t)
	p.helm.Command = cmd
	req := helmRequest(t, server, "1.0.0", writeChart(t, shopChart))
	req.Params[ParamTrafficRoute], req.Params[ParamTrafficService] = "shop", "shop"
	req.Traffic = &plugin.TrafficRequest{State: map[string]string{"candidate@" + server: "b"}}
	if _, err := p.DeployCandidate(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	// The chart receives the slot in .Values.manent.slot.
	if vals, _ := os.ReadFile(log + ".values"); !strings.Contains(string(vals), "slot: b") {
		t.Errorf("values: %s", vals)
	}
}
