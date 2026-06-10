package cow

import (
	"strings"
	"testing"
)

func TestSourceVolumeName(t *testing.T) {
	// gen 1 keeps the legacy P1 name for backward compat with existing volumes
	if got := SourceVolumeName("main", 1); got != "pgbranch-src-main" {
		t.Fatalf("gen1=%q", got)
	}
	if got := SourceVolumeName("main", 2); got != "pgbranch-src-main-g2" {
		t.Fatalf("gen2=%q", got)
	}
	if got := SourceVolumeName("main", 10); got != "pgbranch-src-main-g10" {
		t.Fatalf("gen10=%q", got)
	}
}

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
		"exec docker-entrypoint.sh postgres -c recovery_init_sync_method=syncfs",
	} {
		if !strings.Contains(EntrypointScript, want) {
			t.Fatalf("entrypoint script missing %q", want)
		}
	}
}

func TestParseBackend(t *testing.T) {
	if b, err := ParseBackend("overlay"); err != nil || b != BackendOverlay {
		t.Fatalf("overlay -> %q, %v", b, err)
	}
	if b, err := ParseBackend("zfs"); err != nil || b != BackendZFS {
		t.Fatalf("zfs -> %q, %v", b, err)
	}
	for _, bad := range []string{"", "btrfs", "ZFS", "Overlay"} {
		if _, err := ParseBackend(bad); err == nil {
			t.Errorf("ParseBackend(%q) = nil error, want error", bad)
		}
	}
}

func TestPlannerOverlayNames(t *testing.T) {
	p := Planner{Backend: BackendOverlay}
	// overlay planner must agree with the legacy free functions so existing
	// volumes keep working
	if got := p.SourceLayerName("main", 1); got != SourceVolumeName("main", 1) {
		t.Fatalf("gen1 = %q", got)
	}
	if got := p.SourceLayerName("main", 3); got != "pgbranch-src-main-g3" {
		t.Fatalf("gen3 = %q", got)
	}
	if got := p.BranchLayerName("pr-1"); got != "pgbranch-br-pr-1-rw" {
		t.Fatalf("branch layer = %q", got)
	}
	if p.Entrypoint() != EntrypointScript {
		t.Fatal("overlay planner must use the overlay entrypoint")
	}
}

func TestPlannerZFSNames(t *testing.T) {
	p := Planner{Backend: BackendZFS, Dataset: "tank/pgbranch"}
	// zfs datasets are namespaced under the configured prefix; generation 1
	// has no legacy exception (the backend is new)
	if got := p.SourceLayerName("main", 1); got != "tank/pgbranch/src-main-g1" {
		t.Fatalf("gen1 = %q", got)
	}
	if got := p.SourceLayerName("main", 4); got != "tank/pgbranch/src-main-g4" {
		t.Fatalf("gen4 = %q", got)
	}
	if got := p.BranchLayerName("pr-1"); got != "tank/pgbranch/br-pr-1" {
		t.Fatalf("branch layer = %q", got)
	}
	if got := p.SnapshotName("tank/pgbranch/src-main-g1", "pr-1"); got != "tank/pgbranch/src-main-g1@br-pr-1" {
		t.Fatalf("snapshot = %q", got)
	}
	// default zfs mountpoint: /<dataset>
	if got := p.Mountpoint("tank/pgbranch/br-pr-1"); got != "/tank/pgbranch/br-pr-1" {
		t.Fatalf("mountpoint = %q", got)
	}
	if p.Entrypoint() != EntrypointScriptZFS {
		t.Fatal("zfs planner must use the zfs entrypoint")
	}
}

func TestPlannerZFSCommands(t *testing.T) {
	p := Planner{Backend: BackendZFS, Dataset: "tank/pgbranch"}
	cases := []struct {
		name string
		got  []string
		want string
	}{
		{"create", p.ZFSCreate("tank/pgbranch/src-main-g1"), "zfs create -p tank/pgbranch/src-main-g1"},
		{"snapshot", p.ZFSSnapshot("tank/pgbranch/src-main-g1", "pr-1"), "zfs snapshot tank/pgbranch/src-main-g1@br-pr-1"},
		{"clone", p.ZFSClone("tank/pgbranch/src-main-g1", "pr-1"), "zfs clone tank/pgbranch/src-main-g1@br-pr-1 tank/pgbranch/br-pr-1"},
		{"destroy clone", p.ZFSDestroyClone("pr-1"), "zfs destroy -r tank/pgbranch/br-pr-1"},
		{"destroy snapshot", p.ZFSDestroySnapshot("tank/pgbranch/src-main-g1", "pr-1"), "zfs destroy -r tank/pgbranch/src-main-g1@br-pr-1"},
		{"destroy dataset", p.ZFSDestroyDataset("tank/pgbranch/src-main-g1"), "zfs destroy -r tank/pgbranch/src-main-g1"},
		{"used", p.ZFSUsed("tank/pgbranch/br-pr-1"), "zfs list -Hp -o used tank/pgbranch/br-pr-1"},
	}
	for _, c := range cases {
		if got := strings.Join(c.got, " "); got != c.want {
			t.Errorf("%s = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestZFSEntrypointScriptContent(t *testing.T) {
	// the zfs clone is already writable — no overlay assembly, just perms +
	// stale-pid cleanup + handoff
	for _, want := range []string{
		"chown postgres:postgres \"$PGDATA\"",
		"chmod 0700 \"$PGDATA\"",
		"rm -f \"$PGDATA/postmaster.pid\"",
		"exec docker-entrypoint.sh postgres -c recovery_init_sync_method=syncfs",
	} {
		if !strings.Contains(EntrypointScriptZFS, want) {
			t.Fatalf("zfs entrypoint script missing %q", want)
		}
	}
	for _, reject := range []string{"mount -t overlay", "lowerdir", "upperdir", "workdir", "PGBRANCH_LOWERS"} {
		if strings.Contains(EntrypointScriptZFS, reject) {
			t.Fatalf("zfs entrypoint script must not contain %q", reject)
		}
	}
}

func TestZFSDataPath(t *testing.T) {
	// seeding writes the cluster into <layer>/data; the clone mountpoint is
	// bind-mounted at RWPath, so PGDATA is RWPath/data
	if ZFSDataPath != "/pgbranch/rw/data" {
		t.Fatalf("ZFSDataPath = %q", ZFSDataPath)
	}
}
