package cli

import "runtime/debug"

// version is set by release builds:
//
//	go build -ldflags "-X github.com/pmlucero/retrace/internal/cli.version=v0.1.0"
var version string

// Version returns the release version if set, otherwise the module version that
// the Go toolchain stamps into the binary (a tag for `go install`, a pseudo-version
// for builds inside a git checkout), otherwise "dev" (e.g. in test binaries).
func Version() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}
