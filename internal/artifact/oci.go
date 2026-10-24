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

	"github.com/manent-deployer/manent/pkg/plugin"
)

// Container image schemes: oci:// uses HTTPS, oci+http:// a plain-text registry
// (development and labs only).
const (
	SchemeOCI     = "oci://"
	SchemeOCIHTTP = "oci+http://"
)

// IsOCI reports whether the source is an image in a registry.
func IsOCI(source string) bool {
	return strings.HasPrefix(source, SchemeOCI) || strings.HasPrefix(source, SchemeOCIHTTP)
}

// ociManifestTypes are the accepted manifest formats, multi-architecture indexes included:
// the index digest identifies the image for all architectures.
var ociManifestTypes = []string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}

// ociRef is a reference to an image: registry/repository[:tag][@sha256:digest].
type ociRef struct {
	scheme   string // https or http
	registry string // host[:port] as written in the reference
	repo     string
	tag      string
	digest   string // hexadecimal, without "sha256:"
}

var (
	ociRepoRe   = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*$`)
	ociTagRe    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	ociDigestRe = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// parseOCI parses an oci:// or oci+http:// source.
func parseOCI(source string) (ociRef, error) {
	r := ociRef{scheme: "https"}
	rest := strings.TrimPrefix(source, SchemeOCI)
	if strings.HasPrefix(source, SchemeOCIHTTP) {
		r.scheme, rest = "http", strings.TrimPrefix(source, SchemeOCIHTTP)
	}
	host, name, ok := strings.Cut(rest, "/")
	if !ok || host == "" || name == "" {
		return r, fmt.Errorf("invalid image %q: expected %sregistry/repository:tag", source, SchemeOCI)
	}
	r.registry = host
	if n, d, ok := strings.Cut(name, "@"); ok {
		if !strings.HasPrefix(d, "sha256:") || !ociDigestRe.MatchString(strings.TrimPrefix(d, "sha256:")) {
			return r, fmt.Errorf("image %q: invalid digest %q (expected sha256:<64 hex digits>)", source, d)
		}
		name, r.digest = n, strings.TrimPrefix(d, "sha256:")
	}
	if i := strings.LastIndex(name, ":"); i >= 0 {
		name, r.tag = name[:i], name[i+1:]
		if !ociTagRe.MatchString(r.tag) {
			return r, fmt.Errorf("image %q: invalid tag %q", source, r.tag)
		}
	}
	if !ociRepoRe.MatchString(name) {
		return r, fmt.Errorf("image %q: invalid repository %q", source, name)
	}
	if r.tag == "" && r.digest == "" {
		return r, fmt.Errorf("image %q: provide a tag or a digest", source)
	}
	r.repo = name
	return r, nil
}

// apiHost is the host of the registry API: Docker Hub answers on registry-1.docker.io.
func (r ociRef) apiHost() string {
	if r.registry == "docker.io" || r.registry == "index.docker.io" {
		return "registry-1.docker.io"
	}
	return r.registry
}

// reference is the image name pinned to the digest, to pass to docker/podman or Kubernetes.
func (r ociRef) reference(digest string) string {
	return r.registry + "/" + r.repo + "@sha256:" + digest
}

// fetchOCI resolves the image manifest and computes its digest, which becomes the artifact's
// sha256: promotions and rollbacks then release exactly the verified image, even if the tag
// has moved in the meantime. Only the reference pinned to the digest is written locally
// (plugin.ImageRefExt), not the layers.
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
		return nil, fmt.Errorf("image manifest %s: %w", strings.TrimPrefix(strings.TrimPrefix(source, SchemeOCIHTTP), SchemeOCI), err)
	}
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	if ref.digest != "" && ref.digest != digest {
		return nil, fmt.Errorf("the registry returned a manifest with digest sha256:%s instead of sha256:%s", digest, ref.digest)
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

// ociGet reads the manifest. If the registry asks for a token (401 with WWW-Authenticate
// Bearer, as Docker Hub, GHCR, Harbor) it gets one from the given service, with the source
// credentials if configured, and repeats the request.
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

// ociToken gets a read-only token for the repository from the service named in the
// registry's Bearer challenge.
func (f DefaultFetcher) ociToken(ctx context.Context, source string, ref ociRef, challenge string) (string, error) {
	scheme, params, _ := strings.Cut(challenge, " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return "", errors.New("access denied by the registry: configure the source credentials in artifacts.sources")
	}
	p := parseChallenge(params)
	if p["realm"] == "" {
		return "", errors.New("registry challenge without realm")
	}
	tu, err := url.Parse(p["realm"])
	if err != nil {
		return "", fmt.Errorf("invalid registry realm: %w", err)
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
	// The source credentials (basic) are for the token service.
	client, err := f.Sources.prepare(ctx, req, source)
	if err != nil {
		return "", err
	}
	resp, err := f.client(client).Do(req)
	if err != nil {
		return "", fmt.Errorf("registry token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry token: status %d", resp.StatusCode)
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t); err != nil {
		return "", fmt.Errorf("registry token: %w", err)
	}
	if t.Token == "" {
		t.Token = t.AccessToken
	}
	if t.Token == "" {
		return "", errors.New("empty registry token")
	}
	return t.Token, nil
}

// parseChallenge reads the key="value" parameters of a WWW-Authenticate challenge.
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

// client returns the given client or the default one.
func (f DefaultFetcher) client(c *http.Client) *http.Client {
	switch {
	case c != nil:
		return c
	case f.Client != nil:
		return f.Client
	}
	return http.DefaultClient
}
