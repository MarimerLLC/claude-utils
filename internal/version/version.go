// Package version resolves the version string for the claude-memsync and
// claude-memmerge binaries.
//
// We use the standard Go pattern of a build-time -ldflags "-X" override. If
// that override is missing (e.g. plain `go build` or `go install`), we fall
// back to the VCS info that Go 1.18+ embeds automatically in every binary.
//
// Build-time override (preferred for releases):
//
//	go build -ldflags "-X github.com/MarimerLLC/claude-utils/internal/version.Override=$(git describe --tags --always --dirty)" ./cmd/claude-memsync
package version

import (
	"fmt"
	"runtime/debug"
	"strings"
)

// Override is set at link time via -ldflags "-X <pkg>.Override=<value>".
// Tools that build from source without ldflags will see the empty string
// here and fall back to runtime/debug.BuildInfo.
var Override string

// String returns the version to display in --version output.
func String() string {
	if Override != "" {
		return Override
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	var rev, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	if rev == "" {
		return "dev"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if modified == "true" {
		return fmt.Sprintf("dev+%s-dirty", rev)
	}
	return fmt.Sprintf("dev+%s", rev)
}

// Strings returns the version preceded by the binary name, e.g.
// "claude-memsync v0.1.7".
func Strings(binary string) string {
	return strings.TrimSpace(binary + " " + String())
}
