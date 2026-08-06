// Package version carries build identity. Values are injected at link time by
// GoReleaser; the defaults below are what a `go build` from source produces.
package version

import (
	"fmt"
	"runtime"
)

var (
	// Version is the semver tag, or "dev" for an untagged build.
	Version = "dev"
	// Commit is the git SHA the binary was built from.
	Commit = "none"
	// Date is the RFC3339 build timestamp.
	Date = "unknown"
)

// String renders a single-line build identity.
func String() string {
	return fmt.Sprintf("dkm %s (%s) built %s %s/%s %s",
		Version, Commit, Date, runtime.GOOS, runtime.GOARCH, runtime.Version())
}

// Short returns just the version, for API responses and User-Agent headers.
func Short() string { return Version }

// UserAgent is sent on every outbound HTTP request the client makes.
func UserAgent() string {
	return fmt.Sprintf("dkm/%s (%s/%s)", Version, runtime.GOOS, runtime.GOARCH)
}
