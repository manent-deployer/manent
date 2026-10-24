package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/manent-deployer/manent/pkg/plugin"
)

// fakeRegistry serves the manifest of app:1.0 on /v2/team/app/manifests/; with a token it
// requires the Bearer flow (as Docker Hub) with basic credentials ci:segreto on the token service.
func fakeRegistry(t *testing.T, token bool) (*httptest.Server, string) {
	t.Helper()
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
	sum := sha256.Sum256(manifest)
	digest := hex.EncodeToString(sum[:])
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			if r.Header.Get("Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("ci:segreto")) ||
				r.URL.Query().Get("scope") != "repository:team/app:pull" || r.URL.Query().Get("service") != "reg" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"token":"t0k"}`))
		case r.URL.Path == "/v2/team/app/manifests/1.0" || r.URL.Path == "/v2/team/app/manifests/sha256:"+digest:
			if token && r.Header.Get("Authorization") != "Bearer t0k" {
				w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token",service="reg",scope="repository:team/app:pull"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if !strings.Contains(r.Header.Get("Accept"), "application/vnd.oci.image.index.v1+json") {
				w.WriteHeader(http.StatusNotAcceptable)
				return
			}
			w.Header().Set("Docker-Content-Digest", "sha256:"+digest)
			_, _ = w.Write(manifest)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, digest
}

func TestImmagineOCI(t *testing.T) {
	srv, digest := fakeRegistry(t, false)
	host := strings.TrimPrefix(srv.URL, "http://")
	ctx := context.Background()
	f := DefaultFetcher{}
	got, err := f.Fetch(ctx, "oci+http://"+host+"/team/app:1.0", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got.SHA256 != digest || !strings.HasSuffix(got.LocalPath, "app"+plugin.ImageRefExt) {
		t.Fatalf("artefatto inatteso: %+v", got)
	}
	ref, err := plugin.ImageReference(plugin.Artifact{Name: "app", LocalPath: got.LocalPath, SHA256: got.SHA256})
	if err != nil || ref != host+"/team/app@sha256:"+digest {
		t.Fatalf("riferimento %q (%v)", ref, err)
	}

	// With the digest in the reference: verified.
	if _, err := f.Fetch(ctx, "oci+http://"+host+"/team/app@sha256:"+digest, t.TempDir()); err != nil {
		t.Errorf("immagine per digest: %v", err)
	}
	if _, err := f.Fetch(ctx, "oci+http://"+host+"/team/app:2.0", t.TempDir()); err == nil {
		t.Error("atteso errore per un tag inesistente")
	}
	for _, bad := range []string{"oci://registry", "oci://registry/App:1", "oci://registry/app", "oci://registry/app@sha256:xyz"} {
		if _, err := parseOCI(bad); err == nil {
			t.Errorf("%s: atteso errore", bad)
		}
	}
	if r, err := parseOCI("oci://docker.io/library/nginx:1.27"); err != nil || r.apiHost() != "registry-1.docker.io" || r.scheme != "https" {
		t.Errorf("Docker Hub: %+v %v", r, err)
	}
}

func TestImmagineOCIConToken(t *testing.T) {
	srv, digest := fakeRegistry(t, true)
	host := strings.TrimPrefix(srv.URL, "http://")
	src := "oci+http://" + host + "/team/"
	sources, err := NewSources([]Source{{Prefix: src, Auth: &SourceAuth{Type: AuthBasic, Username: "ci", SecretRef: "env:REG"}}},
		false, mapProvider{"env:REG": "segreto"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := DefaultFetcher{Sources: sources}
	got, err := f.Fetch(context.Background(), src+"app:1.0", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got.SHA256 != digest {
		t.Errorf("digest %s, atteso %s", got.SHA256, digest)
	}
	// A source outside the allowlist is still rejected.
	if _, err := f.Fetch(context.Background(), "oci+http://"+host+"/altro/app:1.0", t.TempDir()); err == nil {
		t.Error("attesa sorgente non ammessa")
	}

	// Store.Ensure downloads again and compares the digest: a moved tag makes the deploy fail.
	s := &Store{Fetcher: f, WorkDir: t.TempDir()}
	if _, _, err := s.Ensure(context.Background(), "d1", src+"app:1.0", "/assente", digest); err != nil {
		t.Errorf("ensure: %v", err)
	}
	if _, _, err := s.Ensure(context.Background(), "d1", src+"app:1.0", "/assente", strings.Repeat("0", 64)); err == nil {
		t.Error("atteso errore con un digest diverso da quello registrato")
	}
	_ = os.Remove(got.LocalPath)
}
