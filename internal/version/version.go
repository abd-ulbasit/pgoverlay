// Package version reports the build version of pgoverlay's binaries (pgb,
// branchd, pgoverlay-github).
//
// Release builds stamp the variables with the linker, for example:
//
//	go build -ldflags "-X github.com/abd-ulbasit/pgoverlay/internal/version.Version=v1.0.0
//	  -X github.com/abd-ulbasit/pgoverlay/internal/version.Commit=1a2b3c4
//	  -X github.com/abd-ulbasit/pgoverlay/internal/version.Date=2026-09-28T10:00:00Z"
//
// `make build` fills them in from `git describe`. Unstamped builds fall back
// to the binary's embedded build info: `go install …/cmd/pgb@v1.0.0` reports
// v1.0.0, and a plain `go build` inside a checkout reports "dev" plus the VCS
// revision.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

// Set with -ldflags "-X …". Version stays "dev" for local builds.
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

// Info is the resolved build identity: the ldflags values, completed from the
// embedded build info where they were left unset.
type Info struct {
	Version   string // "v1.0.0", or "dev" for an unstamped local build
	Commit    string // VCS revision (at most 12 characters), "" when unknown
	Date      string // build or commit time (RFC 3339), "" when unknown
	Modified  bool   // built from a checkout with uncommitted changes
	GoVersion string // e.g. go1.26.5
	Platform  string // GOOS/GOARCH
}

// readBuildInfo is replaced in tests.
var readBuildInfo = debug.ReadBuildInfo

// Get resolves the build identity.
func Get() Info {
	info := Info{
		Version:   Version,
		Commit:    Commit,
		Date:      Date,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
	bi, ok := readBuildInfo()
	if !ok {
		return info
	}
	// `go install module/cmd/pgb@v1.0.0` records the module version. A build
	// inside the checkout records "(devel)", which says nothing useful.
	if info.Version == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		info.Version = bi.Main.Version
	}
	if Commit == "" { // a stamped commit is taken as-is
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				info.Commit = s.Value
			case "vcs.time":
				if info.Date == "" {
					info.Date = s.Value
				}
			case "vcs.modified":
				info.Modified = s.Value == "true"
			}
		}
	}
	if len(info.Commit) > 12 {
		info.Commit = info.Commit[:12]
	}
	return info
}

// String renders the build identity on one line, for example
// "v1.0.0 (commit 1a2b3c4, built 2026-09-28T10:00:00Z, go1.26.5 linux/amd64)".
// Unknown parts are left out.
func String() string { return Get().String() }

func (i Info) String() string {
	parts := make([]string, 0, 3)
	if i.Commit != "" {
		c := "commit " + i.Commit
		if i.Modified {
			c += "-dirty"
		}
		parts = append(parts, c)
	}
	if i.Date != "" {
		parts = append(parts, "built "+i.Date)
	}
	parts = append(parts, i.GoVersion+" "+i.Platform)
	return fmt.Sprintf("%s (%s)", i.Version, strings.Join(parts, ", "))
}
