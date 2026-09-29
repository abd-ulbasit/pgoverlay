package engine

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/pgoverlay/internal/cow"
	"github.com/abd-ulbasit/pgoverlay/internal/pgctl/pgctltest"
	"github.com/abd-ulbasit/pgoverlay/internal/registry"
	"github.com/abd-ulbasit/pgoverlay/internal/runtime"
)

// Copy-up detection, reflink-aware usage and the docker volume root, end to
// end on a real daemon. Always runs on docker-managed volumes; with
// PGOVERLAY_IT_VOLUME_ROOT=<dir on the Docker host> it runs again with every
// volume under that directory (make it a directory on an XFS reflink=1 or
// btrfs filesystem to see clone mode). PGOVERLAY_IT_COPYUP (for the
// docker-managed run) and PGOVERLAY_IT_VOLUME_ROOT_COPYUP (for the volume
// root) assert the mode the probe must find: clone or copy.
//
// The branches are read over localhost, so run it on the Docker host.
func TestCopyUpAndVolumeRootEndToEnd(t *testing.T) {
	if os.Getenv("PGOVERLAY_IT") != "1" {
		t.Skip("set PGOVERLAY_IT=1")
	}
	t.Run("docker-volumes", func(t *testing.T) {
		copyUpEndToEnd(t, "", os.Getenv("PGOVERLAY_IT_COPYUP"))
	})
	root := os.Getenv("PGOVERLAY_IT_VOLUME_ROOT")
	if root == "" {
		t.Log("PGOVERLAY_IT_VOLUME_ROOT is unset: skipping the volume-root run")
		return
	}
	t.Run("volume-root", func(t *testing.T) {
		copyUpEndToEnd(t, root, os.Getenv("PGOVERLAY_IT_VOLUME_ROOT_COPYUP"))
	})
}

func copyUpEndToEnd(t *testing.T, root, wantMode string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	host, port, network, hostConn := pgctltest.StartSourcePG(t, ctx)
	// ~64 MiB table, frozen on the source so a branch only reads it
	mustExec(t, ctx, hostConn, `CREATE TABLE big AS
		SELECT i, repeat(md5(i::text), 16) AS pad FROM generate_series(1, 100000) i`)
	mustExec(t, ctx, hostConn, `VACUUM (FREEZE, ANALYZE) big`)
	mustExec(t, ctx, hostConn, `CHECKPOINT`)

	d, err := runtime.NewDockerDriver(runtime.WithVolumeRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.CheckVolumeRoot(ctx); err != nil {
		t.Fatal(err)
	}
	r, err := registry.Open(t.TempDir() + "/it.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	e := New(r, d, "postgres:17")
	inst := r.InstanceID()

	// 1. the probe: a mode, its volumes gone again
	res, err := e.DetectCopyUp(ctx, CopyUpOptions{Root: root, CowExtSize: DefaultCowExtSize})
	if err != nil {
		t.Fatalf("DetectCopyUp: %v (result %s)", err, res)
	}
	t.Logf("copy-up probe (root %q): %s", root, res)
	if res.Mode == cow.CopyUpUnknown || e.CopyUpMode() != res.Mode {
		t.Fatalf("probe mode %s, engine reports %s", res.Mode, e.CopyUpMode())
	}
	if wantMode != "" && string(res.Mode) != wantMode {
		t.Fatalf("copy-up mode = %s, want %s", res.Mode, wantMode)
	}
	if res.FSType == "ext4" && res.Mode != cow.CopyUpCopy {
		t.Fatalf("ext4 cannot clone, but the probe says %s", res.Mode)
	}
	assertNoVolumes(t, ctx, d, inst, "after the probe")

	// 2. a source and a branch; usage before and after a one-row write to
	// the big table, which copies its file up (whole, with or without the
	// lazyrw shim): in clone mode the copy shares all but the rewritten
	// blocks with the source, and usage counts only those
	// step timings, to compare volume placements
	last := time.Now()
	lap := func(what string) {
		t.Logf("%s took %s", what, time.Since(last).Round(time.Millisecond))
		last = time.Now()
	}
	name := fmt.Sprintf("cow-w4-%d", time.Now().UnixNano()%100000)
	src := &registry.Source{Name: name, PGVersion: "17", ConnHost: host, ConnPort: port, ConnUser: "postgres", Network: network}
	if err := e.AddSource(ctx, src, "secret"); err != nil {
		t.Fatal(err)
	}
	lap("add source (seed)")
	t.Cleanup(func() { e.RemoveSource(context.Background(), name) })
	b, err := e.CreateBranch(ctx, name+"-b1", name, 0)
	if err != nil {
		t.Fatal(err)
	}
	lap("create branch")
	t.Cleanup(func() { e.DestroyBranch(context.Background(), name+"-b1") })
	u0 := mustUsage(t, ctx, e, b.Name)
	du0 := mustDuSb(t, ctx, d, b.RWVolume)
	if n := mustQueryInt(t, ctx, branchConn(b), `SELECT count(*) FROM big`); n != 100000 {
		t.Fatalf("branch rows = %d", n)
	}
	mustExec(t, ctx, branchConn(b), `UPDATE big SET pad = pad WHERE i = 1; CHECKPOINT`)
	u1 := mustUsage(t, ctx, e, b.Name)
	du1 := mustDuSb(t, ctx, d, b.RWVolume)
	t.Logf("one-row write to a ~56 MiB table: usage %d -> %d (+%d), du -sb %d -> %d (+%d)", u0, u1, u1-u0, du0, du1, du1-du0)
	if du1-du0 < 40<<20 {
		t.Fatalf("du -sb grew %d bytes: the write did not copy the table's file up", du1-du0)
	}
	clone := res.Mode == cow.CopyUpClone && duTool(res.Machine) != nil
	switch {
	case clone && u1-u0 > (du1-du0)/2:
		t.Fatalf("clone mode: usage grew %d bytes of du's %d; want only the rewritten blocks counted", u1-u0, du1-du0)
	case !clone && u1-u0 < 40<<20:
		t.Fatalf("%s mode: usage grew %d bytes, want the copied-up table counted", res.Mode, u1-u0)
	}

	// 3. a write is the branch's own in every mode
	mustExec(t, ctx, branchConn(b), `UPDATE big SET pad = upper(pad) WHERE i <= 20000; CHECKPOINT`)
	u2 := mustUsage(t, ctx, e, b.Name)
	t.Logf("after rewriting 20%% of the table: usage %d (+%d)", u2, u2-u1)
	if u2 <= u1 {
		t.Fatalf("usage did not grow with writes: %d -> %d", u1, u2)
	}

	// 4. branch-from-branch (freeze), diff (throwaways) and reset all work on
	// these volumes
	child, err := e.CreateBranchFrom(ctx, name+"-b2", b.Name, 0)
	if err != nil {
		t.Fatal(err)
	}
	lap("branch from branch")
	t.Cleanup(func() { e.DestroyBranch(context.Background(), name+"-b2") })
	if n := mustQueryInt(t, ctx, branchConn(child), `SELECT count(*) FROM big WHERE pad = upper(pad)`); n < 20000 {
		t.Fatalf("child sees %d rewritten rows, want >= 20000", n)
	}
	b, err = r.GetBranchByName(b.Name)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, ctx, branchConn(b), `CREATE TABLE cow_w4_new(x int); INSERT INTO cow_w4_new SELECT generate_series(1, 10); ANALYZE`)
	diff, err := e.DiffBranch(ctx, b.Name)
	if err != nil {
		t.Fatal(err)
	}
	lap("diff")
	if !strings.Contains(diff.SchemaDiff, "cow_w4_new") {
		t.Fatalf("diff misses the new table:\n%s", diff.SchemaDiff)
	}
	if child, err = e.ResetBranch(ctx, child.Name); err != nil {
		t.Fatal(err)
	}
	lap("reset")
	if n := mustQueryInt(t, ctx, branchConn(child), `SELECT count(*) FROM big`); n != 100000 {
		t.Fatalf("reset child rows = %d", n)
	}

	// 5. reconcile collects an orphan volume, and under a volume root a
	// directory whose volume was removed behind pgoverlay's back
	labels := map[string]string{"pgoverlay.managed": "true", runtime.LabelInstance: inst}
	stray := "pgoverlay-br-" + name + "-stray-rw"
	if err := d.CreateVolume(ctx, stray, labels); err != nil {
		t.Fatal(err)
	}
	left := "pgoverlay-br-" + name + "-left-rw"
	if root != "" {
		if err := d.CreateVolume(ctx, left, labels); err != nil {
			t.Fatal(err)
		}
		cli := itDockerClient(t)
		if err := cli.VolumeRemove(ctx, left, true); err != nil {
			t.Fatal(err)
		}
		if !leftover(t, ctx, d, inst, left) {
			t.Fatalf("the directory of %s is not reported as a leftover", left)
		}
	}
	plan, err := e.ApplyReconcile(ctx, time.Now().Add(time.Hour), 10*time.Minute)
	if err != nil {
		t.Fatalf("reconcile: %v (%+v)", err, plan.Actions)
	}
	lap("reconcile")
	if volumeExists(t, ctx, d, inst, stray) || volumeExists(t, ctx, d, inst, left) {
		t.Fatalf("reconcile left %s or %s behind (%+v)", stray, left, plan.Actions)
	}
	for _, v := range []string{b.RWVolume, child.RWVolume, src.Volume} {
		if !volumeExists(t, ctx, d, inst, v) {
			t.Fatalf("reconcile removed live volume %s", v)
		}
	}
	if u := mustUsage(t, ctx, e, child.Name); u <= 0 {
		t.Fatalf("usage of the reset child = %d", u)
	}

	// 6. teardown leaves nothing: no volume, no directory under the root
	for _, n := range []string{child.Name, b.Name} {
		if err := e.DestroyBranch(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	lap("destroy two branches")
	if err := e.RemoveSource(ctx, name); err != nil {
		t.Fatal(err)
	}
	lap("remove source")
	if _, err := e.ApplyReconcile(ctx, time.Now().Add(time.Hour), 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	assertNoVolumes(t, ctx, d, inst, "after teardown")
}

func mustUsage(t *testing.T, ctx context.Context, e *Engine, branch string) int64 {
	t.Helper()
	n, err := e.BranchUsage(ctx, branch)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// mustDuSb is what usage reported before pgoverlay-du: du -sb of the volume.
func mustDuSb(t *testing.T, ctx context.Context, d runtime.Driver, vol string) int64 {
	t.Helper()
	out, err := d.RunHelper(ctx, runtime.HelperSpec{
		Image:  runtime.UtilityImage,
		Cmd:    []string{"du", "-sb", cow.RWPath},
		Mounts: []runtime.Mount{{Volume: vol, Target: cow.RWPath, ReadOnly: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var n int64
	if _, err := fmt.Sscan(out, &n); err != nil {
		t.Fatalf("du -sb output %q: %v", out, err)
	}
	return n
}

func leftover(t *testing.T, ctx context.Context, d runtime.Driver, inst, name string) bool {
	t.Helper()
	vols, err := d.ListManagedVolumes(ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vols {
		if v.Name == name {
			return v.Leftover
		}
	}
	return false
}

func assertNoVolumes(t *testing.T, ctx context.Context, d runtime.Driver, inst, when string) {
	t.Helper()
	vols, err := d.ListManagedVolumes(ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	if len(vols) != 0 {
		t.Fatalf("%s: volumes (or volume-root directories) left: %+v", when, vols)
	}
}
