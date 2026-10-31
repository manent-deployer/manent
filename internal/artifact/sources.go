package artifact

import (
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/manent-deployer/manent/internal/secrets"
)

// Authentication types towards an artifact source.
const (
	AuthBasic  = "basic"  // Authorization: Basic <username>:<secret>
	AuthBearer = "bearer" // Authorization: Bearer <secret>
	AuthHeader = "header" // <header>: <secret> (e.g. X-JFrog-Art-Api)
)

// SourceAuth are the credentials of a source. SecretRef is a secret provider reference
// (e.g. "vault:secret/data/manent/nexus#password"), resolved at every download.
type SourceAuth struct {
	Type      string
	Username  string
	Header    string
	SecretRef string
}

// Source is a configured artifact source, identified by a URL prefix
// (e.g. "https://nexus.example.com/repository/releases/" or "file:///srv/artifacts/").
type Source struct {
	Prefix string
	Auth   *SourceAuth
	// CAFile is a bundle of extra CAs for the source.
	CAFile string
	client *http.Client
}

// Sources applies the configured sources: which ones are allowed and with which credentials.
type Sources struct {
	list []Source
	// AllowUnlisted also allows sources that match no prefix (without credentials).
	AllowUnlisted bool
	Secrets       secrets.Provider
	// Client is the HTTP client of the sources without a dedicated CA.
	Client *http.Client
}

// NewSources checks the sources and prepares the clients with dedicated CAs.
func NewSources(list []Source, allowUnlisted bool, sp secrets.Provider, client *http.Client) (*Sources, error) {
	s := &Sources{AllowUnlisted: allowUnlisted, Secrets: sp, Client: client}
	for i, src := range list {
		if !strings.HasPrefix(src.Prefix, "https://") && !strings.HasPrefix(src.Prefix, "http://") && !strings.HasPrefix(src.Prefix, "file:///") && !IsOCI(src.Prefix) {
			return nil, fmt.Errorf("artifacts.sources[%d]: prefix must start with https://, http://, file:///, oci:// or oci+http://", i)
		}
		if a := src.Auth; a != nil {
			if strings.HasPrefix(src.Prefix, "file:") {
				return nil, fmt.Errorf("artifacts.sources[%d]: auth does not apply to a file source", i)
			}
			switch {
			case a.Type != AuthBasic && a.Type != AuthBearer && a.Type != AuthHeader:
				return nil, fmt.Errorf("artifacts.sources[%d].auth.type %q is invalid (basic|bearer|header)", i, a.Type)
			case a.SecretRef == "":
				return nil, fmt.Errorf("artifacts.sources[%d].auth.secret_ref is required", i)
			case a.Type == AuthBasic && a.Username == "":
				return nil, fmt.Errorf("artifacts.sources[%d].auth.username is required with basic", i)
			case a.Type == AuthHeader && a.Header == "":
				return nil, fmt.Errorf("artifacts.sources[%d].auth.header is required with header", i)
			}
		}
		if src.CAFile != "" {
			c, err := clientWithCA(client, src.CAFile)
			if err != nil {
				return nil, fmt.Errorf("artifacts.sources[%d]: %w", i, err)
			}
			src.client = c
		}
		s.list = append(s.list, src)
	}
	return s, nil
}

// clientWithCA returns a copy of the client that also trusts the CAs of the given file.
func clientWithCA(base *http.Client, caFile string) (*http.Client, error) {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("reading ca_file: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("ca_file contains no PEM certificates")
	}
	c := &http.Client{}
	if base != nil {
		*c = *base
	}
	c.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	return c, nil
}

// normalize turns the source into the form compared with the prefixes: http(s) URLs and oci
// images are unchanged, local paths become "file://" plus the cleaned absolute path.
func normalize(source string) (string, error) {
	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") || IsOCI(source) {
		return source, checkRemote(source)
	}
	abs, err := filepath.Abs(strings.TrimPrefix(source, "file://"))
	if err != nil {
		return "", err
	}
	return "file://" + filepath.ToSlash(abs), nil
}

// checkRemote refuses what would make a URL leave the prefix it matches: credentials before
// the host (https://repo.example.com@evil.example/) and "." or ".." path segments, also
// percent-encoded, which the server would resolve outside the prefix.
func checkRemote(source string) error {
	_, rest, _ := strings.Cut(source, "://")
	host, p, _ := strings.Cut(rest, "/")
	if strings.Contains(host, "@") {
		return errors.New("artifact source: credentials in the URL are not allowed")
	}
	p, _, _ = strings.Cut(p, "?")
	p, _, _ = strings.Cut(p, "#")
	decoded, err := url.PathUnescape(p)
	if err != nil {
		return fmt.Errorf("artifact source: invalid path: %w", err)
	}
	for _, v := range []string{p, decoded, strings.ReplaceAll(decoded, `\`, "/")} {
		for _, seg := range strings.Split(v, "/") {
			if seg == "." || seg == ".." {
				return errors.New("artifact source: '.' and '..' path segments are not allowed")
			}
		}
	}
	return nil
}

// hasPrefix reports whether the normalized source is under the prefix. A prefix that does not
// end with "/" matches only up to a boundary: https://repo.example.com does not cover
// https://repo.example.com.evil.example/, an image prefix covers its tags and digests.
func hasPrefix(norm, prefix string) bool {
	if !strings.HasPrefix(norm, prefix) {
		return false
	}
	if len(norm) == len(prefix) || strings.HasSuffix(prefix, "/") {
		return true
	}
	bounds := "/?#"
	if IsOCI(prefix) {
		bounds = "/:@"
	}
	return strings.ContainsRune(bounds, rune(norm[len(prefix)]))
}

// match returns the configured source with the longest prefix, or nil.
func (s *Sources) match(source string) (*Source, error) {
	norm, err := normalize(source)
	if err != nil {
		return nil, err
	}
	var best *Source
	for i := range s.list {
		if hasPrefix(norm, s.list[i].Prefix) && (best == nil || len(s.list[i].Prefix) > len(best.Prefix)) {
			best = &s.list[i]
		}
	}
	return best, nil
}

// Check verifies that the source is allowed.
func (s *Sources) Check(source string) error {
	if s == nil {
		return nil
	}
	src, err := s.match(source)
	if err != nil {
		return err
	}
	if src == nil && !s.AllowUnlisted {
		return fmt.Errorf("artifact source not allowed: it matches no entry of artifacts.sources")
	}
	return nil
}

// prepare returns the client to use for the request and adds the source credentials, read
// from the secret provider at download time.
func (s *Sources) prepare(ctx context.Context, req *http.Request, source string) (*http.Client, error) {
	if s == nil {
		return nil, nil
	}
	src, err := s.match(source)
	if err != nil {
		return nil, err
	}
	if src == nil {
		if !s.AllowUnlisted {
			return nil, s.Check(source)
		}
		return s.Client, nil
	}
	if a := src.Auth; a != nil {
		secret, err := s.Secrets.Resolve(ctx, a.SecretRef)
		if err != nil {
			return nil, fmt.Errorf("credentials of source %s: %w", src.Prefix, err)
		}
		switch a.Type {
		case AuthBasic:
			req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(a.Username+":"+secret)))
		case AuthBearer:
			req.Header.Set("Authorization", "Bearer "+secret)
		case AuthHeader:
			req.Header.Set(a.Header, secret)
			// Go drops Authorization on a redirect to another host, not a custom header.
			return withoutHeaderElsewhere(cmp.Or(src.client, s.Client), a.Header), nil
		}
	}
	if src.client != nil {
		return src.client, nil
	}
	return s.Client, nil
}

// withoutHeaderElsewhere returns a copy of the client that removes the header from the
// redirects to a host other than the one of the first request.
func withoutHeaderElsewhere(c *http.Client, header string) *http.Client {
	cp := http.Client{}
	if c != nil {
		cp = *c
	}
	next := cp.CheckRedirect
	cp.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Host != via[0].URL.Host {
			req.Header.Del(header)
		}
		if next != nil {
			return next(req, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &cp
}
