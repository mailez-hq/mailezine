// Package version holds the build version of mailezine.
package version

// Version is overridden at build time with
// -ldflags "-X mailezine/internal/version.Version=<tag>".
var Version = "dev"

// String returns the human-readable version line.
func String() string { return "mailezine " + Version }
