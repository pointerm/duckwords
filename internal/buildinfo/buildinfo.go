// Package buildinfo exposes immutable metadata about the running DuckWords binary.
package buildinfo

import (
	"fmt"
	"runtime"
)

// These values are intentionally safe defaults. Release builds replace them through
// -ldflags so local builds remain reproducible and never invent provenance.
var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

// Info describes the version and toolchain used to build DuckWords.
type Info struct {
	Version   string
	Commit    string
	BuildDate string
	GoVersion string
}

// Current returns metadata embedded in the running binary.
func Current() Info {
	return Info{
		Version:   version,
		Commit:    commit,
		BuildDate: buildDate,
		GoVersion: runtime.Version(),
	}
}

// String returns a stable, human-readable representation of the build metadata.
func (i Info) String() string {
	return fmt.Sprintf(
		"duckwords version=%s commit=%s built=%s go=%s",
		i.Version,
		i.Commit,
		i.BuildDate,
		i.GoVersion,
	)
}
