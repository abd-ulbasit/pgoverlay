package cow

import (
	"strings"
	"testing"
)

func TestPlanBranch(t *testing.T) {
	p := PlanBranch("pr-1", "pgbranch-src-main")
	if p.RWVolume != "pgbranch-br-pr-1-rw" {
		t.Fatalf("RWVolume=%q", p.RWVolume)
	}
	if p.Lowers[0] != "/pgbranch/lower0/data" {
		t.Fatalf("Lowers=%v", p.Lowers)
	}
	if p.LowerEnv() != "/pgbranch/lower0/data" {
		t.Fatalf("LowerEnv=%q", p.LowerEnv())
	}
	if p.SourceVolume != "pgbranch-src-main" {
		t.Fatalf("SourceVolume=%q", p.SourceVolume)
	}
}

func TestEntrypointScriptContent(t *testing.T) {
	for _, want := range []string{
		"mount -t overlay overlay",
		"lowerdir=${PGBRANCH_LOWERS}",
		"upperdir=/pgbranch/rw/upper",
		"workdir=/pgbranch/rw/work",
		"rm -f \"$PGDATA/postmaster.pid\"",
		"exec docker-entrypoint.sh postgres",
	} {
		if !strings.Contains(EntrypointScript, want) {
			t.Fatalf("entrypoint script missing %q", want)
		}
	}
}
