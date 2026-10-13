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

// Parametri del descrittore (deploy.params) per i chart Helm.
const (
	ParamHelmRelease     = "helm_release"      // nome della release (.Release.Name, default: nome dell'applicazione)
	ParamHelmValues      = "helm_values"       // values in YAML
	ParamHelmValuesFiles = "helm_values_files" // file di values contenuti nel chart, separati da virgola, applicati in ordine
	ParamHelmValuePrefix = "helm_value."       // helm_value.<percorso> = valore YAML (es. helm_value.image.tag)
	ParamHelmHooks       = "helm_hooks"        // skip (default): gli hook non vengono applicati; apply: applicati come oggetti normali
	ParamHelmSkipCRDs    = "helm_skip_crds"    // "true": non applica le CRD della cartella crds/ del chart
	helmHookAnnotation   = "helm.sh/hook"
	defaultHelmTimeout   = 2 * time.Minute
	maxChartSize         = 64 << 20
	maxRenderedSize      = 32 << 20
)

// DetailHooksSkipped è la chiave dei dettagli con gli hook del chart non applicati.
const DetailHooksSkipped = "helm_hooks_skipped"

var releaseRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

// Helm genera i manifest di un chart con "helm template": il chart viene solo renderizzato,
// mai installato, quindi Helm non contatta il cluster e non tiene uno stato. Applicazione,
// storico, attesa e rollback restano quelli del plugin.
type Helm struct {
	// Command è il binario di Helm (default "helm", cercato nel PATH).
	Command string
	// SHA256, se indicato, è verificato a ogni esecuzione.
	SHA256 string
	// Timeout del rendering (default 2m).
	Timeout time.Duration
}

// isChart indica se l'artefatto è un chart Helm impacchettato (helm package): un archivio
// .tgz con <nome>/Chart.yaml.
func isChart(file string) (bool, error) {
	lower := strings.ToLower(file)
	if !strings.HasSuffix(lower, ".tgz") && !strings.HasSuffix(lower, ".tar.gz") {
		return false, nil
	}
	f, err := os.Open(file)
	if err != nil {
		return false, fmt.Errorf("lettura dell'artefatto: %w", err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return false, fmt.Errorf("archivio: %w", err)
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("archivio: %w", err)
		}
		dir, name := path.Split(path.Clean(h.Name))
		if name == "Chart.yaml" && dir != "" && !strings.Contains(strings.Trim(dir, "/"), "/") {
			return true, nil
		}
	}
}

// extractChart estrae il chart in dir e restituisce la cartella del chart. Sono ammessi solo
// file e cartelle dentro dir, al più maxChartSize byte in tutto.
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
			return "", fmt.Errorf("chart: percorso %q non ammesso", h.Name)
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
				return "", fmt.Errorf("chart oltre %d MiB", maxChartSize>>20)
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
			// Link e file speciali non servono a un chart e potrebbero uscire da dir.
			continue
		}
	}
	if root == "" {
		return "", errors.New("chart senza Chart.yaml")
	}
	return filepath.Join(dir, root), nil
}

// helmValues costruisce i values da helm_values e dai parametri helm_value.<percorso>, che
// prevalgono. I valori sono interpretati come YAML: "3" è un numero, "true" un booleano.
func helmValues(params map[string]string) (map[string]any, error) {
	vals := map[string]any{}
	if raw := params[ParamHelmValues]; strings.TrimSpace(raw) != "" {
		if err := yaml.Unmarshal([]byte(raw), &vals); err != nil {
			return nil, fmt.Errorf("%s non è un documento YAML con una mappa: %w", ParamHelmValues, err)
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
			return nil, fmt.Errorf("parametro %s: percorso non valido", k)
		}
		var v any
		if err := yaml.Unmarshal([]byte(params[k]), &v); err != nil {
			return nil, fmt.Errorf("parametro %s: %w", k, err)
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

// validateHelm controlla i parametri Helm.
func validateHelm(params map[string]string) []error {
	var errs []error
	if r := params[ParamHelmRelease]; r != "" && (len(r) > 53 || !releaseRe.MatchString(r)) {
		errs = append(errs, fmt.Errorf("%s %q non valido: minuscole, cifre, '-' e '.', al più 53 caratteri", ParamHelmRelease, r))
	}
	if h := params[ParamHelmHooks]; h != "" && h != "skip" && h != "apply" {
		errs = append(errs, fmt.Errorf("%s %q non valido (skip|apply)", ParamHelmHooks, h))
	}
	for _, f := range valuesFiles(params) {
		if c := path.Clean(f); path.IsAbs(c) || c == ".." || strings.HasPrefix(c, "../") {
			errs = append(errs, fmt.Errorf("%s: %q deve essere un percorso dentro il chart", ParamHelmValuesFiles, f))
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
	return dnsName(req.Application)
}

// Render genera i manifest del chart per un cluster con la versione e le API indicate, così
// .Capabilities corrisponde al cluster di destinazione.
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
			return nil, fmt.Errorf("%s: %s assente dal chart", ParamHelmValuesFiles, f)
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
	// Helm non eredita l'ambiente del worker (segreti, KUBECONFIG): configurazione, cache e
	// dati in una cartella temporanea, così non legge repository né credenziali locali.
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
			return nil, fmt.Errorf("helm template non concluso entro %s", timeout)
		}
		return nil, fmt.Errorf("helm template: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// command risolve il binario di Helm e ne verifica lo sha256, se configurato.
func (h Helm) command() (string, error) {
	name := h.Command
	if name == "" {
		name = "helm"
	}
	p, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("binario di Helm %q non trovato: installarlo sul worker o indicare helm.command nella configurazione del plugin", name)
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
		return "", fmt.Errorf("sha256 del binario di Helm %s diverso da quello configurato (%s)", got, h.SHA256)
	}
	return p, nil
}

// limitWriter scrive al più n byte e restituisce errore oltre il limite.
type limitWriter struct {
	w io.Writer
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if len(p) > l.n {
		return 0, errors.New("output di helm troppo grande")
	}
	l.n -= len(p)
	return l.w.Write(p)
}

// filterHooks toglie gli hook del chart: quelli di test sempre, gli altri salvo helm_hooks:
// apply. Restituisce gli oggetti rimasti e i riferimenti degli hook tolti.
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
