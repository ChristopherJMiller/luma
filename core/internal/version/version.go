// Package version holds the build's version metadata, stamped in by the
// linker (see the Makefile's LDFLAGS and the Dockerfile):
//
//	-X github.com/ChristopherJMiller/luma/core/internal/version.Version=v0.1.0
package version

import (
	"fmt"
	"runtime"
)

// Set with -ldflags -X at build time.
var (
	// Version is the release tag or `git describe` output.
	Version = "dev"
	// Commit is the short commit hash.
	Commit = "unknown"
	// Date is the commit time (RFC 3339), not the build time, so two
	// builds of one commit are identical.
	Date = "unknown"
)

// String is the one-line version: "dev (commit unknown, unknown, go1.26.8
// linux/amd64)".
func String() string {
	return fmt.Sprintf("%s (commit %s, %s, %s %s/%s)",
		Version, Commit, Date, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
