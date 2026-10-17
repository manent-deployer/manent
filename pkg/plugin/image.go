package plugin

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// ImageRefExt is the extension of the file manent writes instead of the artifact when the
// source is an image in a registry (oci:// or oci+http://): it holds the image reference
// pinned to the digest, "registry/repository@sha256:<digest>". In this case Artifact.SHA256
// is the manifest digest.
const ImageRefExt = ".image"

var imageRefRe = regexp.MustCompile(`^[A-Za-z0-9.-]+(:[0-9]+)?/[a-z0-9._/-]+@sha256:[a-f0-9]{64}$`)

// ImageReference returns the image reference pinned to the digest for an artifact coming from
// a registry; an error for other artifacts.
func ImageReference(a Artifact) (string, error) {
	if !strings.HasSuffix(a.LocalPath, ImageRefExt) {
		return "", fmt.Errorf("artifact %s is not a registry image: provide a source oci://registry/repository:tag", a.Name)
	}
	b, err := os.ReadFile(a.LocalPath)
	if err != nil {
		return "", fmt.Errorf("image reference: %w", err)
	}
	ref := strings.TrimSpace(string(b))
	if !imageRefRe.MatchString(ref) {
		return "", fmt.Errorf("invalid image reference %q", ref)
	}
	if a.SHA256 != "" && !strings.HasSuffix(ref, "@sha256:"+strings.ToLower(a.SHA256)) {
		return "", fmt.Errorf("image reference %q differs from the verified digest %s", ref, a.SHA256)
	}
	return ref, nil
}
