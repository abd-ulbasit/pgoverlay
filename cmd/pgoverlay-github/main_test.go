package main

import (
	"os"
	"os/exec"
	"testing"

	"github.com/abd-ulbasit/pgoverlay/internal/version"
)

// TestVersionFlag re-runs the test binary as `pgoverlay-github -version`: it
// must print the build version and exit 0 without any GHOOK_* configuration.
func TestVersionFlag(t *testing.T) {
	if os.Getenv("GHOOK_VERSION_CHILD") == "1" {
		os.Args = []string{"pgoverlay-github", "-version"}
		if err := run(); err != nil {
			os.Stderr.WriteString(err.Error())
			os.Exit(3)
		}
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestVersionFlag$")
	cmd.Env = append(os.Environ(), "GHOOK_VERSION_CHILD=1", "GHOOK_WEBHOOK_SECRET=", "GHOOK_PGOVERLAY_SERVER=")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("pgoverlay-github -version: %v (stdout %q)", err, out)
	}
	if want := "pgoverlay-github " + version.String() + "\n"; string(out) != want {
		t.Fatalf("pgoverlay-github -version printed %q, want %q", out, want)
	}
}
