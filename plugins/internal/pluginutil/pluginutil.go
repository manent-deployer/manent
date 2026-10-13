// Package pluginutil raccoglie le funzioni comuni ai plugin dei servizi cloud gestiti: tipi di
// artefatto dalla configurazione, durate e versioni.
package pluginutil

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

// VersionRe sono le versioni ammesse: diventano etichette, tag e annotazioni.
var VersionRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)

// ArtifactTypes legge artifact_types dalla configurazione del plugin, con il default indicato.
func ArtifactTypes(cfg map[string]any, def ...string) ([]string, error) {
	raw, ok := cfg["artifact_types"]
	if !ok {
		return def, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, errors.New("artifact_types deve essere un elenco di stringhe")
	}
	types := make([]string, 0, len(list))
	for _, t := range list {
		s, ok := t.(string)
		if !ok {
			return nil, errors.New("artifact_types deve contenere stringhe")
		}
		types = append(types, s)
	}
	return types, nil
}

// Duration legge il parametro key come durata positiva; def se assente.
func Duration(params map[string]string, key string, def time.Duration) (time.Duration, error) {
	raw := params[key]
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s %q non valido: atteso una durata positiva (es. 10m)", key, raw)
	}
	return d, nil
}
