// Package version identifies the manent binary. Values are set at build time with
// -ldflags "-X github.com/manent-deployer/manent/internal/version.Version=..." (Makefile, GoReleaser).
package version

import "runtime/debug"

var (
	// Version is the released version (SemVer tag), "dev" for a local build.
	Version = "dev"
	// Commit is the hash of the commit the binary was built from.
	Commit = ""
	// Date is the build date in RFC 3339 format.
	Date = ""
)

// Info describes the binary.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit,omitempty"`
	Date      string `json:"date,omitempty"`
	GoVersion string `json:"go_version"`
}

// Get returns the binary information; without ldflags it uses the data recorded by Go.
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
