package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/joeg-ita/manent/pkg/plugin"
)

// Schemi delle immagini dei container: oci:// usa HTTPS, oci+http:// un registry in chiaro
// (solo sviluppo e laboratori).
const (
	SchemeOCI     = "oci://"
	SchemeOCIHTTP = "oci+http://"
)

// IsOCI indica se la sorgente è un'immagine in un registry.
func IsOCI(source string) bool {
	return strings.HasPrefix(source, SchemeOCI) || strings.HasPrefix(source, SchemeOCIHTTP)
}

// ociManifestTypes sono i formati di manifest accettati, indici multi-architettura compresi:
// il digest dell'indice identifica l'immagine per tutte le architetture.
var ociManifestTypes = []string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}

// ociRef è un riferimento a un'immagine: registry/repository[:tag][@sha256:digest].
type ociRef struct {
	scheme   string // https o http
	registry string // host[:porta] come compare nel riferimento
	repo     string
	tag      string
	digest   string // esadecimale, senza "sha256:"
}

var (
	ociRepoRe   = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*$`)
	ociTagRe    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	ociDigestRe = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// parseOCI interpreta una sorgente oci:// o oci+http://.
func parseOCI(source string) (ociRef, error) {
	r := ociRef{scheme: "https"}
	rest := strings.TrimPrefix(source, SchemeOCI)
	if strings.HasPrefix(source, SchemeOCIHTTP) {
		r.scheme, rest = "http", strings.TrimPrefix(source, SchemeOCIHTTP)
	}
	host, name, ok := strings.Cut(rest, "/")
	if !ok || host == "" || name == "" {
		return r, fmt.Errorf("immagine %q non valida: atteso %sregistry/repository:tag", source, SchemeOCI)
	}
	r.registry = host
	if n, d, ok := strings.Cut(name, "@"); ok {
		if !strings.HasPrefix(d, "sha256:") || !ociDigestRe.MatchString(strings.TrimPrefix(d, "sha256:")) {
			return r, fmt.Errorf("immagine %q: digest %q non valido (atteso sha256:<64 cifre esadecimali>)", source, d)
		}
		name, r.digest = n, strings.TrimPrefix(d, "sha256:")
	}
	if i := strings.LastIndex(name, ":"); i >= 0 {
		name, r.tag = name[:i], name[i+1:]
		if !ociTagRe.MatchString(r.tag) {
			return r, fmt.Errorf("immagine %q: tag %q non valido", source, r.tag)
		}
	}
	if !ociRepoRe.MatchString(name) {
		return r, fmt.Errorf("immagine %q: repository %q non valido", source, name)
	}
	if r.tag == "" && r.digest == "" {
		return r, fmt.Errorf("immagine %q: indicare un tag o un digest", source)
	}
	r.repo = name
	return r, nil
}

// apiHost è l'host dell'API del registry: Docker Hub risponde su registry-1.docker.io.
func (r ociRef) apiHost() string {
	if r.registry == "docker.io" || r.registry == "index.docker.io" {
		return "registry-1.docker.io"
	}
	return r.registry
}

// reference è il nome dell'immagine fissato al digest, da passare a docker/podman o a Kubernetes.
func (r ociRef) reference(digest string) string {
	return r.registry + "/" + r.repo + "@sha256:" + digest
}

// fetchOCI risolve il manifest dell'immagine e ne calcola il digest, che diventa lo sha256
// dell'artefatto: così promozioni e rollback rilasciano esattamente l'immagine verificata,
// anche se nel frattempo il tag è stato spostato. In locale viene scritto solo il riferimento
// fissato al digest (plugin.ImageRefExt), non i layer.
func (f DefaultFetcher) fetchOCI(ctx context.Context, source, destDir string) (*Fetched, error) {
	if err := f.Sources.Check(source); err != nil {
		return nil, err
	}
	ref, err := parseOCI(source)
	if err != nil {
		return nil, err
	}
	want := ref.tag
	if ref.digest != "" {
		want = "sha256:" + ref.digest
	}
	u := ref.scheme + "://" + ref.apiHost() + "/v2/" + ref.repo + "/manifests/" + want
	body, err := f.ociGet(ctx, source, ref, u)
	if err != nil {
		return nil, fmt.Errorf("manifest dell'immagine %s: %w", strings.TrimPrefix(strings.TrimPrefix(source, SchemeOCIHTTP), SchemeOCI), err)
	}
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	if ref.digest != "" && ref.digest != digest {
		return nil, fmt.Errorf("il registry ha restituito un manifest con digest sha256:%s invece di sha256:%s", digest, ref.digest)
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, err
	}
	dest := filepath.Join(destDir, path.Base(ref.repo)+plugin.ImageRefExt)
	if err := os.WriteFile(dest, []byte(ref.reference(digest)+"\n"), 0o644); err != nil {
		return nil, err
	}
	return &Fetched{LocalPath: dest, SHA256: digest, Size: int64(len(body))}, nil
}

// ociGet legge il manifest. Se il registry chiede un token (401 con WWW-Authenticate Bearer,
// come Docker Hub, GHCR, Harbor) lo ottiene dal servizio indicato, con le credenziali della
// sorgente se configurate, e ripete la richiesta.
func (f DefaultFetcher) ociGet(ctx context.Context, source string, ref ociRef, u string) ([]byte, error) {
	get := func(token string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", strings.Join(ociManifestTypes, ", "))
		client, err := f.Sources.prepare(ctx, req, source)
		if err != nil {
			return nil, err
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return f.client(client).Do(req)
	}
	resp, err := get("")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("WWW-Authenticate")
		resp.Body.Close()
		token, err := f.ociToken(ctx, source, ref, challenge)
		if err != nil {
			return nil, err
		}
		if resp, err = get(token); err != nil {
			return nil, err
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

// ociToken ottiene un token di sola lettura per il repository dal servizio indicato nella
// challenge Bearer del registry.
func (f DefaultFetcher) ociToken(ctx context.Context, source string, ref ociRef, challenge string) (string, error) {
	scheme, params, _ := strings.Cut(challenge, " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return "", errors.New("accesso negato dal registry: configurare le credenziali della sorgente in artifacts.sources")
	}
	p := parseChallenge(params)
	if p["realm"] == "" {
		return "", errors.New("challenge del registry senza realm")
	}
	tu, err := url.Parse(p["realm"])
	if err != nil {
		return "", fmt.Errorf("realm del registry non valido: %w", err)
	}
	q := tu.Query()
	if p["service"] != "" {
		q.Set("service", p["service"])
	}
	q.Set("scope", "repository:"+ref.repo+":pull")
	tu.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tu.String(), nil)
	if err != nil {
		return "", err
	}
	// Le credenziali della sorgente (basic) servono al servizio dei token.
	client, err := f.Sources.prepare(ctx, req, source)
	if err != nil {
		return "", err
	}
	resp, err := f.client(client).Do(req)
	if err != nil {
		return "", fmt.Errorf("token del registry: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token del registry: status %d", resp.StatusCode)
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t); err != nil {
		return "", fmt.Errorf("token del registry: %w", err)
	}
	if t.Token == "" {
		t.Token = t.AccessToken
	}
	if t.Token == "" {
		return "", errors.New("token del registry vuoto")
	}
	return t.Token, nil
}

// parseChallenge legge i parametri chiave="valore" di una challenge WWW-Authenticate.
func parseChallenge(s string) map[string]string {
	out := map[string]string{}
	for s = strings.TrimSpace(s); s != ""; {
		k, rest, ok := strings.Cut(s, "=")
		if !ok {
			break
		}
		k = strings.TrimSpace(k)
		var v string
		if strings.HasPrefix(rest, `"`) {
			end := strings.Index(rest[1:], `"`)
			if end < 0 {
				break
			}
			v, rest = rest[1:end+1], rest[end+2:]
		} else {
			v, rest, _ = strings.Cut(rest, ",")
		}
		out[strings.ToLower(k)] = v
		s = strings.TrimLeft(strings.TrimSpace(rest), ",")
		s = strings.TrimSpace(s)
	}
	return out
}

// client restituisce il client indicato o quello di default.
func (f DefaultFetcher) client(c *http.Client) *http.Client {
	switch {
	case c != nil:
		return c
	case f.Client != nil:
		return f.Client
	}
	return http.DefaultClient
}
