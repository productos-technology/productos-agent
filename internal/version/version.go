// Package version holds the build identity, set by the linker at release time.
package version

// Set with -ldflags "-X github.com/productos-technology/productos-agent/internal/version.Version=1.2.3 ...".
var (
	Version = "0.0.0-dev"
	Commit  = "none"
	Date    = "unknown"
)

// String is the one-line form shown by `productos-agent version` and sent as agent_version.
func String() string { return Version }
