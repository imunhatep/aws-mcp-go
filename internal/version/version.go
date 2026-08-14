// Package version exposes build metadata, overridable via -ldflags.
package version

// Version is the semantic version, injected at build time.
var Version = "dev"

// Commit is the git commit, injected at build time.
var Commit = "none"
