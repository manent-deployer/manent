package artifact

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strings"
)

// Probed describes an artifact found at its source without downloading it.
type Probed struct {
	// SHA256 is known only for registry images: the digest of the manifest, read in full.
	SHA256 string
	// Size is the size declared by the source, -1 if unknown. For an image it is the
	// manifest size, as in Fetched.
	Size int64
}

// Probe checks that the artifact exists at its source and that the configured credentials
// give access to it, without downloading it: HEAD for http(s) (GET closed after the headers
// if the server does not support HEAD), the manifest for an image, stat for a local file.
func (f DefaultFetcher) Probe(ctx context.Context, source string) (*Probed, error) {
	if IsOCI(source) {
		_, digest, size, err := f.ociManifest(ctx, source)
		if err != nil {
			return nil, err
		}
		return &Probed{SHA256: digest, Size: size}, nil
	}
	if err := f.Sources.Check(source); err != nil {
		return nil, err
	}
	if isHTTP(source) {
		resp, err := f.httpDo(ctx, http.MethodHead, source)
		if err == nil && (resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusNotImplemented) {
			resp.Body.Close()
			resp, err = f.httpDo(ctx, http.MethodGet, source)
		}
		if err != nil {
			return nil, err
		}
		resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusOK:
			return &Probed{Size: resp.ContentLength}, nil
		case http.StatusNotFound, http.StatusGone:
			return nil, fmt.Errorf("artifact not found at the source (status %d)", resp.StatusCode)
		case http.StatusUnauthorized, http.StatusForbidden:
			return nil, fmt.Errorf("access to the artifact denied (status %d): check the credentials in artifacts.sources", resp.StatusCode)
		}
		return nil, fmt.Errorf("artifact source: status %d", resp.StatusCode)
	}
	st, err := os.Stat(strings.TrimPrefix(source, "file://"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, errors.New("artifact not found: no such file on the API server")
	case err != nil:
		return nil, err
	case !st.Mode().IsRegular():
		return nil, errors.New("the artifact is not a regular file")
	}
	return &Probed{Size: st.Size()}, nil
}
