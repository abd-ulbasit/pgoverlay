package cli

import (
	"testing"

	"github.com/abd-ulbasit/pgoverlay/internal/version"
)

// #15: `pgb version` and `pgb --version` print the same build line.
func TestVersionCommandAndFlag(t *testing.T) {
	want := "pgb " + version.String() + "\n"
	if out := run(t, "version"); out != want {
		t.Fatalf("pgb version = %q, want %q", out, want)
	}
	if out := run(t, "--version"); out != want {
		t.Fatalf("pgb --version = %q, want %q", out, want)
	}
}
