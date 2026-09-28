package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/abd-ulbasit/pgoverlay/internal/runtime"
)

// Issue #11 (LIFECYCLE-08 / DRIFT-02): volumes a saga creates before any row
// names them are claimed in the registry (pending_volume) and counted by
// LiveVolumeSet. TestReconcileMidFreezeKeepsParentsNewRWVolume and
// TestReconcileMidRefreshKeepsNextGenerationVolume (reconcile_test.go) run
// reconcile at the parent's restart and in the middle of a refresh's seed;
// the tests here cover the freeze's earlier window and the claim's release.

// mounts reports whether a helper mounts volume.
func mounts(s runtime.HelperSpec, volume string) bool {
	for _, m := range s.Mounts {
		if m.Volume == volume {
			return true
		}
	}
	return false
}

// A reconcile pass landing right after a freeze created the parent's swap
// volume (installing its entrypoint, before the parent restarts) must not GC
// it, and the claim is released at commit: afterwards the volume is live only
// because the parent row names it.
func TestReconcileDuringFreezeKeepsParentSwapVolume(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "p", "main", 0); err != nil {
		t.Fatal(err)
	}
	const swap = "pgoverlay-br-p-rw-g2"
	var taken ReconcilePlan
	ran := false
	d.onRunHelper = func(s runtime.HelperSpec) {
		if ran || !mounts(s, swap) {
			return
		}
		ran = true
		var err error
		taken, err = e.ApplyReconcile(context.Background(), time.Now(), time.Hour)
		if err != nil {
			t.Errorf("reconcile: %v", err)
		}
	}
	if _, err := e.CreateBranchFrom(context.Background(), "c", "p", 0); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("hook never ran")
	}
	for _, a := range taken.Actions {
		if a.Kind == ActionGCVolume {
			t.Fatalf("reconcile GC'd an in-flight volume: %+v", a)
		}
	}
	p, _ := r.GetBranchByName("p")
	if p.RWVolume != swap || !d.volumes[p.RWVolume] {
		t.Fatalf("parent after freeze: rw=%q present=%v", p.RWVolume, d.volumes[p.RWVolume])
	}
	live, _ := r.LiveVolumeSet()
	if !live[swap] || !live["pgoverlay-br-p-rw"] {
		t.Fatalf("live set after freeze: %v", live)
	}
}

// A failed freeze releases the claim, so the removed swap volume is not
// pinned as live.
func TestFailedFreezeReleasesSwapVolumeClaim(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "p", "main", 0); err != nil {
		t.Fatal(err)
	}
	d.failStartAt = map[int]bool{3: true} // the child start
	if _, err := e.CreateBranchFrom(context.Background(), "c", "p", 0); err == nil {
		t.Fatal("want failure")
	}
	if live, _ := r.LiveVolumeSet(); live["pgoverlay-br-p-rw-g2"] {
		t.Fatal("failed freeze left its swap volume claimed")
	}
}

// A failed refresh seed releases the next generation's claim as well.
func TestFailedRefreshReleasesNextGenerationClaim(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	d.helperErr = errors.New("pg_basebackup: boom")
	if err := e.RefreshSource(context.Background(), "main", "pw"); err == nil {
		t.Fatal("want failure")
	}
	if live, _ := r.LiveVolumeSet(); live["pgoverlay-src-main-g2"] {
		t.Fatal("failed refresh left its next generation claimed")
	}
	if s := mustSource(t, r); s.Volume == "pgoverlay-src-main-g2" {
		t.Fatalf("failed refresh bumped the generation: %q", s.Volume)
	}
}
