// Package version holds the build version injected at release time.
package version

// Version is set with -ldflags "-X github.com/alexander-jacob/dv-backup/internal/version.Version=v1.2.3".
var Version = "dev"

// Tool returns the tool identifier recorded in manifests, e.g. "dv-backup v0.1.0".
func Tool() string { return "dv-backup " + Version }
