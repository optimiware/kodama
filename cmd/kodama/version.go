package main

import (
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
)

// version and revision are set at link time by `make build`, `make dist` and
// the image build (-ldflags "-X main.version=... -X main.revision=..."). A
// plain `go build` leaves version at "dev" and takes the revision from the
// VCS data the toolchain embeds, when there is any.
var (
	version  = "dev"
	revision = ""
)

// printVersion writes the one-line build identity.
func printVersion(out io.Writer) {
	fmt.Fprintf(out, "kodama %s (revision %s, %s, %s/%s)\n",
		version, buildRevision(), runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

// buildRevision returns the linked revision, else the embedded VCS revision,
// else "unknown". A VCS revision of a modified tree carries a "-dirty" suffix.
func buildRevision() string {
	if revision != "" {
		return revision
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	found, dirty := "", false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			found = setting.Value
		case "vcs.modified":
			dirty = setting.Value == "true"
		}
	}
	if found == "" {
		return "unknown"
	}
	if dirty {
		return found + "-dirty"
	}
	return found
}
