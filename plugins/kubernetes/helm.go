package kubernetes

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/joeg-ita/manent/pkg/plugin"
)

// Descriptor parameters (deploy.params) for Helm charts.
const (
	ParamHelmRelease     = "helm_release"      // release name (.Release.Name, default: artifact name)
	ParamHelmValues      = "helm_values"       // values in YAML
	ParamHelmValuesFiles = "helm_values_files" // values files inside the chart, comma separated, applied in order
	ParamHelmValuePrefix = "helm_value."       // helm_value.<path> = YAML value (e.g. helm_value.image.tag)
	ParamHelmHooks       = "helm_hooks"        // skip (default): hooks are not applied; apply: applied as normal objects
	ParamHelmSkipCRDs    = "helm_skip_crds"    // "true": does not apply the CRDs of the chart's crds/ folder
	helmHookAnnotation   = "helm.sh/hook"
	defaultHelmTimeout   = 2 * time.Minute
	maxChartSize         = 64 << 20
	maxRenderedSize      = 32 << 20
)

// DetailHooksSkipped is the details key with the chart hooks not applied.
const DetailHooksSkipped = "helm_hooks_skipped"

var releaseRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

// Helm generates a chart's manifests with "helm template": the chart is only rendered, never
// installed, so Helm does not contact the cluster and keeps no state. Apply, history, wait
// and rollback stay the plugin's.
type Helm struct {
	// Command is the Helm binary (default "helm", looked up in PATH).
	Command string
	// SHA256, if set, is verified at every run.
	SHA256 string
	// Timeout of the rendering (default 2m).
	Timeout time.Duration
}

// isChart reports whether the artifact is a packaged Helm chart (helm package): a .tgz
// archive with <name>/Chart.yaml.
func isChart(file string) (bool, error) {
	lower := strings.ToLower(file)
	if !strings.HasSuffix(lower, ".tgz") && !strings.HasSuffix(lower, ".tar.gz") {
		return false, nil
	}
	f, err := os.Open(file)
	if err != nil {
		return false, fmt.Errorf("reading the artifact: %w", err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return false, fmt.Errorf("archive: %w", err)
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("archive: %w", err)
		}
		dir, name := path.Split(path.Clean(h.Name))
		if name == "Chart.yaml" && dir != "" && !strings.Contains(strings.Trim(dir, "/"), "/") {
			return true, nil
		}
	}
}

// extractChart extracts the chart into dir and returns the chart folder. Only files and
// folders inside dir are allowed, at most maxChartSize bytes in total.
func extractChart(file, dir string) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("chart: %w", err)
	}
	tr := tar.NewReader(zr)
	var total int64
	root := ""
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("chart: %w", err)
		}
		name := path.Clean(h.Name)
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return "", fmt.Errorf("chart: path %q not allowed", h.Name)
		}
		dst := filepath.Join(dir, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o700); err != nil {
				return "", err
			}
		case tar.TypeReg:
			total += h.Size
			if total > maxChartSize {
				return "", fmt.Errorf("chart over %d MiB", maxChartSize>>20)
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
				return "", err
			}
			out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return "", err
			}
			_, err = io.Copy(out, io.LimitReader(tr, h.Size))
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return "", err
			}
			if d, base := path.Split(name); base == "Chart.yaml" && d != "" && !strings.Contains(strings.Trim(d, "/"), "/") {
				root = strings.Trim(d, "/")
			}
		default:
			// Links and special files are of no use to a chart and could escape dir.
			continue
		}
	}
	if root == "" {
		return "", errors.New("chart without Chart.yaml")
	}
	return filepath.Join(dir, root), nil
}

// helmValues builds the values from helm_values and from the helm_value.<path> parameters,
// which win. Values are parsed as YAML: "3" is a number, "true" a boolean.
func helmValues(params map[string]string) (map[string]any, error) {
	vals := map[string]any{}
	if raw := params[ParamHelmValues]; strings.TrimSpace(raw) != "" {
		if err := yaml.Unmarshal([]byte(raw), &vals); err != nil {
			return nil, fmt.Errorf("%s is not a YAML document with a map: %w", ParamHelmValues, err)
		}
		if vals == nil {
			vals = map[string]any{}
		}
	}
	keys := slices.Sorted(func(yield func(string) bool) {
		for k := range params {
			if strings.HasPrefix(k, ParamHelmValuePrefix) && !yield(k) {
				return
			}
		}
	})
	for _, k := range keys {
		p := strings.Split(strings.TrimPrefix(k, ParamHelmValuePrefix), ".")
		if slices.Contains(p, "") {
			return nil, fmt.Errorf("parameter %s: invalid path", k)
		}
		var v any
		if err := yaml.Unmarshal([]byte(params[k]), &v); err != nil {
			return nil, fmt.Errorf("parameter %s: %w", k, err)
		}
		cur := vals
		for _, part := range p[:len(p)-1] {
			next, ok := cur[part].(map[string]any)
			if !ok {
				next = map[string]any{}
				cur[part] = next
			}
			cur = next
		}
		cur[p[len(p)-1]] = v
	}
	return vals, nil
}

// validateHelm checks the Helm parameters.
func validateHelm(params map[string]string) []error {
	var errs []error
	if r := params[ParamHelmRelease]; r != "" && (len(r) > 53 || !releaseRe.MatchString(r)) {
		errs = append(errs, fmt.Errorf("invalid %s %q: lowercase letters, digits, '-' and '.', at most 53 characters", ParamHelmRelease, r))
	}
	if h := params[ParamHelmHooks]; h != "" && h != "skip" && h != "apply" {
		errs = append(errs, fmt.Errorf("invalid %s %q (skip|apply)", ParamHelmHooks, h))
	}
	for _, f := range valuesFiles(params) {
		if c := path.Clean(f); path.IsAbs(c) || c == ".." || strings.HasPrefix(c, "../") {
			errs = append(errs, fmt.Errorf("%s: %q must be a path inside the chart", ParamHelmValuesFiles, f))
		}
	}
	if _, err := helmValues(params); err != nil {
		errs = append(errs, err)
	}
	return errs
}

func valuesFiles(params map[string]string) []string {
	var out []string
	for _, f := range strings.Split(params[ParamHelmValuesFiles], ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func releaseName(req *plugin.Request) string {
	if r := req.Params[ParamHelmRelease]; r != "" {
		return r
	}
	// The artifact name: two charts of the same application render objects with different
	// names. It is the application name for the artifact named after it.
	return dnsName(cmpOr(req.Artifact.Name, req.Application))
}

// Render generates the chart manifests for a cluster with the given version and APIs, so
// .Capabilities matches the destination cluster.
func (h Helm) Render(ctx context.Context, req *plugin.Request, kubeVersion string, apiVersions []string) ([]byte, error) {
	cmd, err := h.command()
	if err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp("", "manent-helm-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	chart, err := extractChart(req.Artifact.LocalPath, filepath.Join(tmp, "chart"))
	if err != nil {
		return nil, err
	}
	args := []string{"template", releaseName(req), chart, "--namespace", namespace(req), "--skip-tests"}
	if kubeVersion != "" {
		args = append(args, "--kube-version", kubeVersion)
	}
	for _, v := range apiVersions {
		args = append(args, "--api-versions", v)
	}
	if req.Params[ParamHelmSkipCRDs] != "true" {
		args = append(args, "--include-crds")
	}
	for _, f := range valuesFiles(req.Params) {
		p := filepath.Join(chart, filepath.FromSlash(path.Clean(f)))
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("%s: %s missing from the chart", ParamHelmValuesFiles, f)
		}
		args = append(args, "--values", p)
	}
	vals, err := helmValues(req.Params)
	if err != nil {
		return nil, err
	}
	if len(vals) > 0 {
		b, err := yaml.Marshal(vals)
		if err != nil {
			return nil, err
		}
		vf := filepath.Join(tmp, "values.yaml")
		if err := os.WriteFile(vf, b, 0o600); err != nil {
			return nil, err
		}
		args = append(args, "--values", vf)
	}
	timeout := h.Timeout
	if timeout == 0 {
		timeout = defaultHelmTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c := exec.CommandContext(ctx, cmd, args...)
	// Helm does not inherit the worker environment (secrets, KUBECONFIG): configuration, cache
	// and data live in a temporary folder, so it reads no local repositories or credentials.
	c.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + tmp, "KUBECONFIG=" + filepath.Join(tmp, "kubeconfig"),
		"HELM_CONFIG_HOME=" + filepath.Join(tmp, "config"), "HELM_CACHE_HOME=" + filepath.Join(tmp, "cache"),
		"HELM_DATA_HOME=" + filepath.Join(tmp, "data"),
	}
	c.Dir = tmp
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &limitWriter{w: &stdout, n: maxRenderedSize}, &limitWriter{w: &stderr, n: 64 << 10}
	if err := c.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("helm template not completed within %s", timeout)
		}
		return nil, fmt.Errorf("helm template: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// command resolves the Helm binary and verifies its sha256, if configured.
func (h Helm) command() (string, error) {
	name := h.Command
	if name == "" {
		name = "helm"
	}
	p, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("Helm binary %q not found: install it on the worker or set helm.command in the plugin configuration", name)
	}
	if h.SHA256 == "" {
		return p, nil
	}
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}
	if got := hex.EncodeToString(sum.Sum(nil)); !strings.EqualFold(got, h.SHA256) {
		return "", fmt.Errorf("sha256 of the Helm binary %s differs from the configured one (%s)", got, h.SHA256)
	}
	return p, nil
}

// limitWriter writes at most n bytes and returns an error beyond the limit.
type limitWriter struct {
	w io.Writer
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if len(p) > l.n {
		return 0, errors.New("helm output too large")
	}
	l.n -= len(p)
	return l.w.Write(p)
}

// filterHooks removes the chart hooks: test ones always, the others unless helm_hooks:
// apply. It returns the remaining objects and the references of the removed hooks.
func filterHooks(objs []Object, params map[string]string) ([]Object, []string) {
	var kept []Object
	var skipped []string
	for _, o := range objs {
		md := o.metadata()
		ann, _ := md["annotations"].(map[string]any)
		hook, _ := ann[helmHookAnnotation].(string)
		switch {
		case hook == "":
			kept = append(kept, o)
		case strings.Contains(hook, "test"):
		case params[ParamHelmHooks] == "apply":
			kept = append(kept, o)
		default:
			skipped = append(skipped, o.Ref())
		}
	}
	return kept, skipped
}
