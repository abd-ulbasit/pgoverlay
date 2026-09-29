package main

import (
	"strings"
	"testing"

	"github.com/abd-ulbasit/pgoverlay/internal/cow"
)

func TestCheckVolumeRootFlag(t *testing.T) {
	cases := []struct {
		root, runtime string
		backend       cow.Backend
		wantErr       string
	}{
		{"", "docker", cow.BackendOverlay, ""},
		{"", "kube", cow.BackendCSI, ""},
		{"/data/pgoverlay", "docker", cow.BackendOverlay, ""},
		{"/data/pgoverlay", "kube", cow.BackendOverlay, "--kube-data-root"},
		{"/data/pgoverlay", "docker", cow.BackendZFS, "overlay backend"},
		{"data/pgoverlay", "docker", cow.BackendOverlay, "absolute"},
		{"/", "docker", cow.BackendOverlay, "below /"},
	}
	for _, tc := range cases {
		err := checkVolumeRootFlag(tc.root, tc.runtime, tc.backend)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%q/%s/%s: %v", tc.root, tc.runtime, tc.backend, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%q/%s/%s: err %v, want one mentioning %q", tc.root, tc.runtime, tc.backend, err, tc.wantErr)
		}
	}
}

func TestParseByteSize(t *testing.T) {
	for in, want := range map[string]int64{"0": 0, "16k": 16384, "16K": 16384, "1m": 1 << 20, "8192": 8192, " 32k ": 32768} {
		if got, err := parseByteSize(in); err != nil || got != want {
			t.Errorf("parseByteSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "k", "-1", "16kb", "1g", "1.5k", "99999999m"} {
		if _, err := parseByteSize(bad); err == nil {
			t.Errorf("parseByteSize(%q) accepted", bad)
		}
	}
}

func TestDiskRootOverride(t *testing.T) {
	local := func(p string) bool { return p == "/data/pg" }
	cases := []struct{ flag, root, want string }{
		{"/explicit", "/data/pg", "/explicit"}, // --disk-root wins
		{"", "/data/pg", "/data/pg"},           // the volume root, measurable here
		{"", "/remote/only", ""},               // a remote Docker host's root: not here
		{"", "", ""},
	}
	for _, tc := range cases {
		if got := diskRootOverride(tc.flag, tc.root, local); got != tc.want {
			t.Errorf("diskRootOverride(%q, %q) = %q, want %q", tc.flag, tc.root, got, tc.want)
		}
	}
}

func TestProbeRoot(t *testing.T) {
	if got := probeRoot("docker", "/data/pg", "/var/lib/pgoverlay"); got != "/data/pg" {
		t.Errorf("docker: %q", got)
	}
	if got := probeRoot("docker", "", "/var/lib/pgoverlay"); got != "" {
		t.Errorf("docker without a volume root: %q (docker-managed volume dirs are Docker's)", got)
	}
	if got := probeRoot("kube", "", "/var/lib/pgoverlay"); got != "/var/lib/pgoverlay" {
		t.Errorf("kube: %q", got)
	}
}
