package main

import (
	"os"
	"os/exec"
	"testing"

	"github.com/abd-ulbasit/pgoverlay/internal/version"
)

// TestVersionFlag re-runs the test binary as `branchd -version`: it must print
// the build version and exit 0 before touching PGOVERLAY_TOKEN, the registry
// or a container runtime.
func TestVersionFlag(t *testing.T) {
	if os.Getenv("BRANCHD_VERSION_CHILD") == "1" {
		os.Args = []string{"branchd", "-version"}
		if err := run(); err != nil {
			os.Stderr.WriteString(err.Error())
			os.Exit(3)
		}
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestVersionFlag$")
	cmd.Env = append(os.Environ(), "BRANCHD_VERSION_CHILD=1", "PGOVERLAY_TOKEN=", "PGOVERLAY_HOME="+t.TempDir())
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("branchd -version: %v (stdout %q)", err, out)
	}
	if want := "branchd " + version.String() + "\n"; string(out) != want {
		t.Fatalf("branchd -version printed %q, want %q", out, want)
	}
}
