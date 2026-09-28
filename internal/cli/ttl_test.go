package cli

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/pgoverlay/internal/registry"
)

// CLI-12: --ttl promised auto-destroy, but in local mode nothing reaps.
func TestTTLHelpAndLocalExpiredNote(t *testing.T) {
	root := NewRootCmd()
	create, _, _ := root.Find([]string{"branch", "create"})
	if usage := create.Flags().Lookup("ttl").Usage; !strings.Contains(usage, "pgb gc") || !strings.Contains(usage, "local mode does not reap") {
		t.Fatalf("--ttl help = %q", usage)
	}

	home := t.TempDir()
	t.Setenv("PGOVERLAY_HOME", home)
	t.Setenv("PGOVERLAY_SERVER", "")
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1") // branch ls without --usage never dials it
	reg, err := registry.Open(filepath.Join(home, "pgoverlay.db"))
	if err != nil {
		t.Fatal(err)
	}
	src := &registry.Source{Name: "main", PGVersion: "17", Volume: "v"}
	if err := reg.CreateSource(src); err != nil {
		t.Fatal(err)
	}
	for name, exp := range map[string]string{
		"old":   time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		"fresh": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"never": "",
	} {
		if err := reg.CreateBranch(&registry.Branch{Name: name, SourceID: src.ID, RWVolume: "rw-" + name, SourceVolume: "v", ExpiresAt: exp}); err != nil {
			t.Fatal(err)
		}
	}
	reg.Close()

	out, err := runErr(t, "branch", "ls")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1 branch(es) are past their TTL") || !strings.Contains(out, "pgb gc") {
		t.Fatalf("no expired-branch note in %q", out)
	}
}
