package engine

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/abd-ulbasit/pgbranch/internal/pgctl"
	"github.com/abd-ulbasit/pgbranch/internal/registry"
	"github.com/abd-ulbasit/pgbranch/internal/runtime"
)

// TestReconcileGCEndToEnd drives the unified reconcile loop against real
// docker: a healthy source+branch must survive, while a stray managed volume,
// a stuck `creating` registry row and a TTL-expired branch are all cleaned by
// one reconcile pass. Names use a gc- prefix so they cannot collide with other
// IT suites' resources.
func TestReconcileGCEndToEnd(t *testing.T) {
	if os.Getenv("PGBRANCH_IT") != "1" {
		t.Skip("set PGBRANCH_IT=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	host, port, network, _ := pgctl.StartSourcePG(t, ctx)

	d, err := runtime.NewDockerDriver()
	if err != nil {
		t.Fatal(err)
	}
	r, err := registry.Open(t.TempDir() + "/it.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	e := New(r, d, "postgres:17")

	src := &registry.Source{Name: "gc-main", PGVersion: "17", ConnHost: host, ConnPort: port, ConnUser: "postgres", Network: network}
	if err := e.AddSource(ctx, src, "secret"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.RemoveVolume(context.Background(), src.Volume) })

	// a healthy branch that must survive reconcile.
	keep, err := e.CreateBranch(ctx, "gc-keep", "gc-main", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.DestroyBranch(context.Background(), "gc-keep") })

	// a TTL-expired branch the loop should reap (1s TTL, already elapsed).
	if _, err := e.CreateBranch(ctx, "gc-expired", "gc-main", time.Second); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)

	// a stray managed volume owned by no branch (tagged with this registry's
	// instance id so reconcile recognizes it as ITS orphan to reclaim).
	strayVol := "pgbranch-br-gc-stray-rw"
	if err := d.CreateVolume(ctx, strayVol, map[string]string{"pgbranch.managed": "true", runtime.LabelInstance: r.InstanceID()}); err != nil {
		t.Fatal(err)
	}

	// a stuck `creating` registry row with its own rw volume.
	stuckVol := "pgbranch-br-gc-stuck-rw"
	if err := d.CreateVolume(ctx, stuckVol, map[string]string{"pgbranch.managed": "true", runtime.LabelInstance: r.InstanceID()}); err != nil {
		t.Fatal(err)
	}
	stuck := &registry.Branch{Name: "gc-stuck", SourceID: src.ID, RWVolume: stuckVol, SourceVolume: src.Volume}
	if err := r.CreateBranch(stuck); err != nil {
		t.Fatal(err)
	}

	// one reconcile pass with a future clock so the just-inserted stuck row is
	// past the 10m stuck timeout, and the 1s-TTL branch is expired.
	taken, err := e.ApplyReconcile(ctx, time.Now().Add(time.Hour), 10*time.Minute)
	if err != nil {
		t.Fatalf("reconcile: %v (took %+v)", err, taken.Actions)
	}

	// the healthy branch survives.
	if b, err := r.GetBranchByName("gc-keep"); err != nil || b.State != registry.BranchReady {
		t.Fatalf("healthy branch gc-keep should survive: %+v err=%v", b, err)
	}

	// the stuck row is failed and its rw volume removed.
	if b, err := r.GetBranchByName("gc-stuck"); err != nil || b.State != registry.BranchFailed {
		t.Fatalf("gc-stuck should be failed: %+v err=%v", b, err)
	}
	if volumeExists(t, ctx, d, r.InstanceID(), stuckVol) {
		t.Fatalf("stuck rw volume %q survived reconcile", stuckVol)
	}

	// the stray volume is gone.
	if volumeExists(t, ctx, d, r.InstanceID(), strayVol) {
		t.Fatalf("stray volume %q survived reconcile", strayVol)
	}

	// the TTL-expired branch is reaped (destroyed tombstone or gone).
	if b, err := r.GetBranchByName("gc-expired"); err == nil && b.State != registry.BranchDestroyed {
		t.Fatalf("gc-expired should be reaped, got %+v", b)
	}

	// the keep branch's rw volume and the source volume are still present.
	if !volumeExists(t, ctx, d, r.InstanceID(), keep.RWVolume) {
		t.Fatalf("kept branch rw volume %q was GC'd", keep.RWVolume)
	}
	if !volumeExists(t, ctx, d, r.InstanceID(), src.Volume) {
		t.Fatalf("source volume %q was GC'd", src.Volume)
	}
}

// volumeExists reports whether a managed volume with the given name is still
// listed by the driver.
func volumeExists(t *testing.T, ctx context.Context, d runtime.Driver, instanceID, name string) bool {
	t.Helper()
	vols, err := d.ListManagedVolumes(ctx, instanceID)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vols {
		if v == name {
			return true
		}
	}
	return false
}
