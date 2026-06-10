package engine

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abd-ulbasit/pgbranch/internal/cow"
	"github.com/abd-ulbasit/pgbranch/internal/registry"
	"github.com/abd-ulbasit/pgbranch/internal/runtime"
)

// Unit tests for the zfs backend: every layer step must become a privileged
// zfs helper invocation instead of a docker/kube volume operation, and the
// branch container must run on the clone's mountpoint via a hostpath mount.

func zfsEngine(t *testing.T, d runtime.Driver) (*Engine, *registry.Registry) {
	t.Helper()
	r, err := registry.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return NewWithPlanner(r, d, "postgres:17", cow.Planner{Backend: cow.BackendZFS, Dataset: "tank/pgbranch"}), r
}

// readyZFSSource registers a ready source whose layer is a zfs dataset.
func readyZFSSource(t *testing.T, r *registry.Registry) *registry.Source {
	t.Helper()
	s := &registry.Source{Name: "main", PGVersion: "17", Volume: "tank/pgbranch/src-main-g1"}
	if err := r.CreateSource(s); err != nil {
		t.Fatal(err)
	}
	if err := r.SetSourceState(s.ID, registry.SourceReady, "test"); err != nil {
		t.Fatal(err)
	}
	return s
}

func helperCmd(s runtime.HelperSpec) string { return strings.Join(s.Cmd, " ") }

// findZFSHelper returns the first recorded helper whose command contains
// substr, asserting it ran privileged with /dev/zfs mapped in.
func findZFSHelper(t *testing.T, helpers []runtime.HelperSpec, substr string) runtime.HelperSpec {
	t.Helper()
	for _, h := range helpers {
		if strings.Contains(helperCmd(h), substr) {
			if !h.Privileged {
				t.Fatalf("helper %q not privileged", substr)
			}
			if len(h.HostDevices) != 1 || h.HostDevices[0] != "/dev/zfs" {
				t.Fatalf("helper %q HostDevices = %v, want [/dev/zfs]", substr, h.HostDevices)
			}
			return h
		}
	}
	t.Fatalf("no helper invocation containing %q; got %d helpers", substr, len(helpers))
	return runtime.HelperSpec{}
}

func helperIndex(helpers []runtime.HelperSpec, substr string) int {
	for i, h := range helpers {
		if strings.Contains(helperCmd(h), substr) {
			return i
		}
	}
	return -1
}

func TestZFSAddSourceCreatesDatasetAndSeedsMountpoint(t *testing.T) {
	d := newFake()
	e, r := zfsEngine(t, d)

	s := &registry.Source{Name: "main", PGVersion: "17", ConnHost: "db", ConnPort: 5432, ConnUser: "postgres"}
	if err := e.AddSource(context.Background(), s, "pw"); err != nil {
		t.Fatal(err)
	}
	got, err := r.GetSourceByName("main")
	if err != nil {
		t.Fatal(err)
	}
	if got.Volume != "tank/pgbranch/src-main-g1" {
		t.Fatalf("source layer = %q, want tank/pgbranch/src-main-g1", got.Volume)
	}
	if got.State != registry.SourceReady {
		t.Fatalf("state = %q", got.State)
	}
	// the dataset is created by a privileged zfs helper, not a docker volume
	findZFSHelper(t, d.helpers, "zfs create -p tank/pgbranch/src-main-g1")
	if len(d.volumes) != 0 {
		t.Fatalf("zfs mode created docker volumes: %v", d.volumes)
	}
	// seeding targets the dataset's mountpoint via a hostpath mount
	var seeded bool
	for _, h := range d.helpers {
		for _, m := range h.Mounts {
			if m.Target == "/seed" {
				seeded = true
				if m.Kind != runtime.MountHostPath || m.Volume != "/tank/pgbranch/src-main-g1" {
					t.Fatalf("seed mount = %+v, want hostpath /tank/pgbranch/src-main-g1", m)
				}
			}
		}
	}
	if !seeded {
		t.Fatal("no seed helper mounted the dataset mountpoint")
	}
}

func TestZFSCreateBranchSnapshotsClonesAndMountsClone(t *testing.T) {
	d := newFake()
	e, r := zfsEngine(t, d)
	readyZFSSource(t, r)

	b, err := e.CreateBranch(context.Background(), "pr-1", "main", 0)
	if err != nil {
		t.Fatal(err)
	}
	if b.State != registry.BranchReady {
		t.Fatalf("state = %q", b.State)
	}
	if b.RWVolume != "tank/pgbranch/br-pr-1" {
		t.Fatalf("branch layer = %q, want the clone dataset", b.RWVolume)
	}
	// snapshot then clone, both privileged zfs helpers, in that order
	findZFSHelper(t, d.helpers, "zfs snapshot tank/pgbranch/src-main-g1@br-pr-1")
	findZFSHelper(t, d.helpers, "zfs clone tank/pgbranch/src-main-g1@br-pr-1 tank/pgbranch/br-pr-1")
	if snap, clone := helperIndex(d.helpers, "zfs snapshot"), helperIndex(d.helpers, "zfs clone"); snap > clone {
		t.Fatalf("snapshot (helper %d) must precede clone (helper %d)", snap, clone)
	}
	// the zfs entrypoint (no overlay assembly) is installed into the clone
	// mountpoint by an unprivileged helper
	i := helperIndex(d.helpers, "entrypoint.sh")
	if i < 0 {
		t.Fatal("no entrypoint install helper")
	}
	ep := d.helpers[i]
	if ep.Privileged {
		t.Fatal("entrypoint install must not be privileged")
	}
	if len(ep.Env) != 1 || ep.Env[0] != "PGBRANCH_ENTRYPOINT="+cow.EntrypointScriptZFS {
		t.Fatalf("entrypoint helper env = %v, want the zfs entrypoint script", ep.Env)
	}
	if len(ep.Mounts) != 1 || ep.Mounts[0].Kind != runtime.MountHostPath || ep.Mounts[0].Volume != "/tank/pgbranch/br-pr-1" {
		t.Fatalf("entrypoint helper mounts = %+v, want hostpath clone mountpoint", ep.Mounts)
	}
	// branch container: clone mountpoint bind-mounted rw, PGDATA in its
	// data/ subdir, no overlay lowers, no source mount, no docker volumes
	if len(d.branches) != 1 {
		t.Fatalf("StartBranch calls = %d", len(d.branches))
	}
	bs := d.branches[0]
	if len(bs.Mounts) != 1 || bs.Mounts[0].Kind != runtime.MountHostPath ||
		bs.Mounts[0].Volume != "/tank/pgbranch/br-pr-1" || bs.Mounts[0].Target != cow.RWPath || bs.Mounts[0].ReadOnly {
		t.Fatalf("branch mounts = %+v, want rw hostpath clone mountpoint at %s", bs.Mounts, cow.RWPath)
	}
	if len(bs.Env) != 1 || bs.Env[0] != "PGDATA="+cow.ZFSDataPath {
		t.Fatalf("branch env = %v, want only PGDATA=%s", bs.Env, cow.ZFSDataPath)
	}
	if len(d.volumes) != 0 {
		t.Fatalf("zfs mode created docker volumes: %v", d.volumes)
	}
}

func TestZFSDestroyBranchDestroysCloneThenSnapshot(t *testing.T) {
	d := newFake()
	e, r := zfsEngine(t, d)
	readyZFSSource(t, r)
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}

	if err := e.DestroyBranch(context.Background(), "pr-1"); err != nil {
		t.Fatal(err)
	}
	findZFSHelper(t, d.helpers, "zfs destroy -r tank/pgbranch/br-pr-1")
	findZFSHelper(t, d.helpers, "zfs destroy -r tank/pgbranch/src-main-g1@br-pr-1")
	// the clone depends on the snapshot: it must be destroyed first
	ci := helperIndex(d.helpers, "zfs destroy -r tank/pgbranch/br-pr-1")
	si := helperIndex(d.helpers, "zfs destroy -r tank/pgbranch/src-main-g1@br-pr-1")
	if ci > si {
		t.Fatalf("clone destroy (helper %d) must precede snapshot destroy (helper %d)", ci, si)
	}
	if len(d.containers) != 0 {
		t.Fatalf("container leaked: %v", d.containers)
	}
	if _, err := r.GetBranchByName("pr-1"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("want branch gone, got %v", err)
	}
}

func TestZFSCreateBranchUnwindsSnapshotAndClone(t *testing.T) {
	d := newFake()
	d.failStart = true
	e, r := zfsEngine(t, d)
	readyZFSSource(t, r)

	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err == nil {
		t.Fatal("want error")
	}
	// compensations must zfs-destroy both the clone and the snapshot
	findZFSHelper(t, d.helpers, "zfs destroy -r tank/pgbranch/br-pr-1")
	findZFSHelper(t, d.helpers, "zfs destroy -r tank/pgbranch/src-main-g1@br-pr-1")
	b, err := r.GetBranchByName("pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if b.State != registry.BranchFailed {
		t.Fatalf("state = %q, want failed", b.State)
	}
}

func TestZFSResetBranchReclones(t *testing.T) {
	d := newFake()
	e, r := zfsEngine(t, d)
	readyZFSSource(t, r)
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ResetBranch(context.Background(), "pr-1"); err != nil {
		t.Fatal(err)
	}
	count := func(substr string) (n int) {
		for _, h := range d.helpers {
			if strings.Contains(helperCmd(h), substr) {
				n++
			}
		}
		return
	}
	// reset destroys the old clone+snapshot and re-snapshots/re-clones
	if n := count("zfs snapshot tank/pgbranch/src-main-g1@br-pr-1"); n != 2 {
		t.Fatalf("snapshot invocations = %d, want 2 (create + reset)", n)
	}
	if n := count("zfs clone "); n != 2 {
		t.Fatalf("clone invocations = %d, want 2", n)
	}
	if n := count("zfs destroy -r tank/pgbranch/br-pr-1"); n != 1 {
		t.Fatalf("clone destroy invocations = %d, want 1", n)
	}
}

func TestZFSBranchUsage(t *testing.T) {
	d := newFake()
	d.helperOut = "123456\n"
	e, r := zfsEngine(t, d)
	readyZFSSource(t, r)
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}

	n, err := e.BranchUsage(context.Background(), "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if n != 123456 {
		t.Fatalf("usage = %d, want 123456", n)
	}
	// measured by zfs (space unique to the clone), not du on a volume
	last := d.helpers[len(d.helpers)-1]
	if !strings.Contains(helperCmd(last), "zfs list -Hp -o used tank/pgbranch/br-pr-1") {
		t.Fatalf("usage helper cmd = %q, want zfs list -Hp -o used", helperCmd(last))
	}
	if !last.Privileged {
		t.Fatal("usage helper must be privileged (zfs ioctl)")
	}
}

func TestZFSRemoveSourceDestroysDataset(t *testing.T) {
	d := newFake()
	e, r := zfsEngine(t, d)
	readyZFSSource(t, r)

	if err := e.RemoveSource(context.Background(), "main"); err != nil {
		t.Fatal(err)
	}
	findZFSHelper(t, d.helpers, "zfs destroy -r tank/pgbranch/src-main-g1")
	if _, err := r.GetSourceByName("main"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("want source gone, got %v", err)
	}
}

func TestZFSRefreshSourceCreatesNextGenerationDataset(t *testing.T) {
	d := newFake()
	e, r := zfsEngine(t, d)
	readyZFSSource(t, r)

	if err := e.RefreshSource(context.Background(), "main", "pw"); err != nil {
		t.Fatal(err)
	}
	s, err := r.GetSourceByName("main")
	if err != nil {
		t.Fatal(err)
	}
	if s.Generation != 2 || s.Volume != "tank/pgbranch/src-main-g2" {
		t.Fatalf("source after refresh: gen=%d layer=%q", s.Generation, s.Volume)
	}
	findZFSHelper(t, d.helpers, "zfs create -p tank/pgbranch/src-main-g2")
	// no live branches -> the old generation dataset is GC'd
	findZFSHelper(t, d.helpers, "zfs destroy -r tank/pgbranch/src-main-g1")
}

// ZFS branch-from-branch is block-level CoW: snapshot the parent's clone and
// clone that. No freeze, no parent stop/restart, no layer rows.
func TestZFSCreateBranchFromSnapshotsParentClone(t *testing.T) {
	d := newFake()
	e, r := zfsEngine(t, d)
	src := readyZFSSource(t, r)
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}

	b, err := e.CreateBranchFrom(context.Background(), "pr-2", "pr-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if b.State != registry.BranchReady || b.ParentBranchName != "pr-1" {
		t.Fatalf("child %+v", b)
	}
	if b.RWVolume != "tank/pgbranch/br-pr-2" || b.SourceVolume != "tank/pgbranch/br-pr-1" {
		t.Fatalf("child datasets: rw=%q source=%q", b.RWVolume, b.SourceVolume)
	}
	// snapshot the PARENT'S CLONE, then clone it
	findZFSHelper(t, d.helpers, "zfs snapshot tank/pgbranch/br-pr-1@br-pr-2")
	findZFSHelper(t, d.helpers, "zfs clone tank/pgbranch/br-pr-1@br-pr-2 tank/pgbranch/br-pr-2")
	// no freeze: the parent keeps running, is never checkpointed or restarted
	if !d.containers["cid-pgbranch-br-pr-1"] {
		t.Fatal("parent container was stopped")
	}
	if d.starts != 2 {
		t.Fatalf("starts=%d want 2 (no parent restart)", d.starts)
	}
	for _, c := range d.psqlExecs() {
		if c[len(c)-1] == "CHECKPOINT" {
			t.Fatal("zfs branch-from-branch must not checkpoint the parent")
		}
	}
	// block-level CoW needs no overlay layer rows
	if layers, _ := r.ListLayersBySource(src.ID); len(layers) != 0 {
		t.Fatalf("zfs mode created layer rows: %v", layers)
	}
	if b.BaseLayerID != "" {
		t.Fatalf("zfs child has BaseLayerID %q", b.BaseLayerID)
	}
}

// A zfs parent cannot be destroyed while children clone its snapshots; the
// engine refuses instead of failing mid-destroy. And destroying the child
// must never GC the parent's clone dataset as an "orphaned source volume".
func TestZFSDestroyParentWithChildRefusedAndChildCleansUp(t *testing.T) {
	d := newFake()
	e, r := zfsEngine(t, d)
	readyZFSSource(t, r)
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateBranchFrom(context.Background(), "pr-2", "pr-1", 0); err != nil {
		t.Fatal(err)
	}

	err := e.DestroyBranch(context.Background(), "pr-1")
	if err == nil || !strings.Contains(err.Error(), "child branch") {
		t.Fatalf("destroy parent with live child: err=%v want child-branch refusal", err)
	}
	if b, _ := r.GetBranchByName("pr-1"); b.State != registry.BranchReady {
		t.Fatalf("refused destroy mutated parent: %+v", b)
	}

	if err := e.DestroyBranch(context.Background(), "pr-2"); err != nil {
		t.Fatal(err)
	}
	findZFSHelper(t, d.helpers, "zfs destroy -r tank/pgbranch/br-pr-2")
	findZFSHelper(t, d.helpers, "zfs destroy -r tank/pgbranch/br-pr-1@br-pr-2")
	// the parent's live clone dataset must NOT have been destroyed by GC
	for _, h := range d.helpers {
		if strings.Contains(helperCmd(h), "zfs destroy -r tank/pgbranch/br-pr-1 ") ||
			strings.HasSuffix(helperCmd(h), "zfs destroy -r tank/pgbranch/br-pr-1") {
			t.Fatalf("child destroy GC'd the parent's clone: %q", helperCmd(h))
		}
	}
	// with the child gone the parent destroys normally
	if err := e.DestroyBranch(context.Background(), "pr-1"); err != nil {
		t.Fatal(err)
	}
	findZFSHelper(t, d.helpers, "zfs destroy -r tank/pgbranch/br-pr-1")
}

// Resetting a zfs child re-clones from its parent's dataset, not the source.
func TestZFSResetChildReclonesFromParent(t *testing.T) {
	d := newFake()
	e, r := zfsEngine(t, d)
	readyZFSSource(t, r)
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateBranchFrom(context.Background(), "pr-2", "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ResetBranch(context.Background(), "pr-2"); err != nil {
		t.Fatal(err)
	}
	count := func(substr string) (n int) {
		for _, h := range d.helpers {
			if strings.Contains(helperCmd(h), substr) {
				n++
			}
		}
		return
	}
	if n := count("zfs snapshot tank/pgbranch/br-pr-1@br-pr-2"); n != 2 {
		t.Fatalf("parent snapshot invocations = %d, want 2 (create + reset)", n)
	}
	if n := count("zfs snapshot tank/pgbranch/src-main-g1@br-pr-2"); n != 0 {
		t.Fatal("child reset snapshotted the SOURCE, want its parent")
	}
}

func TestZFSDestroyHelpersAreIdempotent(t *testing.T) {
	// destroy parity with `docker volume rm -f`: an already-absent dataset or
	// snapshot must not fail the destroy (failed branches stay destroyable)
	d := newFake()
	e, r := zfsEngine(t, d)
	readyZFSSource(t, r)
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}
	if err := e.DestroyBranch(context.Background(), "pr-1"); err != nil {
		t.Fatal(err)
	}
	h := findZFSHelper(t, d.helpers, "zfs destroy -r tank/pgbranch/br-pr-1")
	if !strings.Contains(helperCmd(h), "|| ! zfs list -t all") {
		t.Fatalf("destroy helper %q has no missing-target guard", helperCmd(h))
	}
}
