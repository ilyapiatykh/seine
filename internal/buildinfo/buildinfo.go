// Package buildinfo exposes build-time metadata that is stamped in via
// -ldflags during release builds. Defaults are useful for local builds.
package buildinfo

import "runtime/debug"

var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

// Short returns a compact "<version> (<commit-short>)" string.
func Short() string {
	c := Commit
	if len(c) > 7 {
		c = c[:7]
	}
	return Version + " (" + c + ")"
}

// GoModule returns the module path embedded by the Go toolchain.
func GoModule() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		return info.Main.Path
	}
	return ""
}
