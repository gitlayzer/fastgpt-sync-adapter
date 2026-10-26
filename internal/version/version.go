package version

import (
	"fmt"
	"runtime"
)

var (
	// Version is the current semver version of fastgpt-sync-adapter.
	Version = "v2.0.0-dev"
	// GitCommit is the git commit SHA injected at build time.
	GitCommit = "none"
	// BuildDate is the build timestamp injected at build time.
	BuildDate = "unknown"
)

// Info returns formatted build and version details.
func Info() string {
	return fmt.Sprintf("fastgpt-sync-adapter %s (commit: %s, built: %s, %s/%s, %s)",
		Version, GitCommit, BuildDate, runtime.GOOS, runtime.GOARCH, runtime.Version())
}
