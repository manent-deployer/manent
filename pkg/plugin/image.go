package plugin

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// ImageRefExt è l'estensione del file che manent scrive al posto dell'artefatto quando la
// sorgente è un'immagine in un registry (oci:// o oci+http://): contiene il riferimento
// all'immagine fissato al digest, "registry/repository@sha256:<digest>". In questo caso
// Artifact.SHA256 è il digest del manifest.
const ImageRefExt = ".image"

var imageRefRe = regexp.MustCompile(`^[A-Za-z0-9.-]+(:[0-9]+)?/[a-z0-9._/-]+@sha256:[a-f0-9]{64}$`)

// ImageReference restituisce il riferimento all'immagine fissato al digest per un artefatto
// che proviene da un registry; errore per gli altri artefatti.
func ImageReference(a Artifact) (string, error) {
	if !strings.HasSuffix(a.LocalPath, ImageRefExt) {
		return "", fmt.Errorf("l'artefatto %s non è un'immagine di un registry: indicare una sorgente oci://registry/repository:tag", a.Name)
	}
	b, err := os.ReadFile(a.LocalPath)
	if err != nil {
		return "", fmt.Errorf("riferimento all'immagine: %w", err)
	}
	ref := strings.TrimSpace(string(b))
	if !imageRefRe.MatchString(ref) {
		return "", fmt.Errorf("riferimento all'immagine %q non valido", ref)
	}
	if a.SHA256 != "" && !strings.HasSuffix(ref, "@sha256:"+strings.ToLower(a.SHA256)) {
		return "", fmt.Errorf("riferimento all'immagine %q diverso dal digest verificato %s", ref, a.SHA256)
	}
	return ref, nil
}
