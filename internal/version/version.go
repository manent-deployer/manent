// Package version identifica il binario di manent. I valori sono impostati in fase di build
// con -ldflags "-X github.com/joeg-ita/manent/internal/version.Version=..." (Makefile, GoReleaser).
package version

import "runtime/debug"

var (
	// Version è la versione rilasciata (tag SemVer), "dev" per una build locale.
	Version = "dev"
	// Commit è l'hash del commit da cui è stato costruito il binario.
	Commit = ""
	// Date è la data di build in formato RFC 3339.
	Date = ""
)

// Info descrive il binario.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit,omitempty"`
	Date      string `json:"date,omitempty"`
	GoVersion string `json:"go_version"`
}

// Get restituisce le informazioni sul binario; senza ldflags usa quelle registrate da Go.
func Get() Info {
	i := Info{Version: Version, Commit: Commit, Date: Date}
	if bi, ok := debug.ReadBuildInfo(); ok {
		i.GoVersion = bi.GoVersion
		for _, s := range bi.Settings {
			switch {
			case s.Key == "vcs.revision" && i.Commit == "":
				i.Commit = s.Value
			case s.Key == "vcs.time" && i.Date == "":
				i.Date = s.Value
			}
		}
	}
	return i
}
