package version

import (
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

// stub replaces the ldflags variables and the build-info reader for one test.
func stub(t *testing.T, v, commit, date string, bi *debug.BuildInfo) {
	t.Helper()
	oldV, oldC, oldD, oldR := Version, Commit, Date, readBuildInfo
	t.Cleanup(func() { Version, Commit, Date, readBuildInfo = oldV, oldC, oldD, oldR })
	Version, Commit, Date = v, commit, date
	readBuildInfo = func() (*debug.BuildInfo, bool) { return bi, bi != nil }
}

func TestStampedBuildWins(t *testing.T) {
	stub(t, "v1.0.0", "1a2b3c4", "2026-09-28T10:00:00Z", &debug.BuildInfo{
		Main: debug.Module{Version: "v0.9.0"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "ffffffffffffffffffff"},
			{Key: "vcs.time", Value: "2020-01-01T00:00:00Z"},
			{Key: "vcs.modified", Value: "true"},
		},
	})
	got := Get()
	if got.Version != "v1.0.0" || got.Commit != "1a2b3c4" || got.Date != "2026-09-28T10:00:00Z" || got.Modified {
		t.Fatalf("Get() = %+v, want the ldflags values untouched", got)
	}
	want := "v1.0.0 (commit 1a2b3c4, built 2026-09-28T10:00:00Z, " + runtime.Version() + " " + runtime.GOOS + "/" + runtime.GOARCH + ")"
	if s := String(); s != want {
		t.Fatalf("String() = %q, want %q", s, want)
	}
}

// `go install …@v1.0.0` carries no ldflags but records the module version.
func TestGoInstallUsesModuleVersion(t *testing.T) {
	stub(t, "dev", "", "", &debug.BuildInfo{Main: debug.Module{Version: "v1.0.0"}})
	if got := Get(); got.Version != "v1.0.0" {
		t.Fatalf("Version = %q, want v1.0.0 from build info", got.Version)
	}
}

// A local `go build` in a checkout: "(devel)" is not a version, but the VCS
// stamp still identifies the build.
func TestLocalBuildKeepsDevAndUsesVCS(t *testing.T) {
	stub(t, "dev", "", "", &debug.BuildInfo{
		Main: debug.Module{Version: "(devel)"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "c55cab2e3b1a0f9d8c7b6a5f4e3d2c1b0a9f8e7d"},
			{Key: "vcs.time", Value: "2026-09-27T12:00:00Z"},
			{Key: "vcs.modified", Value: "true"},
		},
	})
	got := Get()
	if got.Version != "dev" {
		t.Fatalf("Version = %q, want dev", got.Version)
	}
	if got.Commit != "c55cab2e3b1a" || !got.Modified || got.Date != "2026-09-27T12:00:00Z" {
		t.Fatalf("Get() = %+v, want the VCS revision (12 chars), dirty flag and time", got)
	}
	if s := got.String(); !strings.HasPrefix(s, "dev (commit c55cab2e3b1a-dirty, built 2026-09-27T12:00:00Z, go") {
		t.Fatalf("String() = %q", s)
	}
}

func TestNoBuildInfo(t *testing.T) {
	stub(t, "dev", "", "", nil)
	want := "dev (" + runtime.Version() + " " + runtime.GOOS + "/" + runtime.GOARCH + ")"
	if s := String(); s != want {
		t.Fatalf("String() = %q, want %q", s, want)
	}
}
