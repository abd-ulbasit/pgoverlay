package pgctl

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// TestShippedBinariesDoNotLinkTestDeps keeps integration-test helpers out of
// the release binaries. The testcontainers source helpers once lived in a
// non-test file of this package, which linked testcontainers-go, its Moby
// dependencies and the testing package into every binary (and with them
// every advisory against those modules). They now live in pgctltest, which
// only _test.go files import.
func TestShippedBinariesDoNotLinkTestDeps(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go list")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH")
	}
	cmd := exec.Command(goBin, "list", "-deps", "-f", "{{.ImportPath}}",
		"./cmd/pgb", "./cmd/branchd", "./cmd/pgoverlay-github")
	cmd.Dir = "../.." // module root
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("go list -deps: %v\n%s", err, ee.Stderr)
		}
		t.Fatalf("go list -deps: %v", err)
	}
	for _, pkg := range strings.Fields(string(out)) {
		if pkg == "testing" ||
			strings.HasPrefix(pkg, "github.com/testcontainers/") ||
			strings.HasPrefix(pkg, "github.com/moby/go-archive") ||
			strings.HasSuffix(pkg, "/pgctltest") {
			t.Errorf("shipped binaries link test-only package %q", pkg)
		}
	}
}
