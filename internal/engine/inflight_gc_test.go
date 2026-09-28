package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/pgoverlay/internal/runtime"
)

// hookDriver runs onHelper inside RunHelper calls whose command contains
// match — a point in a saga after it created a volume that no registry row
// names yet.
type hookDriver struct {
	*fakeDriver
	match    string
	onHelper func()
}

func (d *hookDriver) RunHelper(ctx context.Context, s runtime.HelperSpec) (string, error) {
	if d.onHelper != nil && strings.Contains(strings.Join(s.Cmd, " ")+" "+mountsOf(s), d.match) {
		f := d.onHelper
		d.onHelper = nil
		f()
	}
	return d.fakeDriver.RunHelper(ctx, s)
}

func mountsOf(s runtime.HelperSpec) string {
	var out []string
	for _, m := range s.Mounts {
		out = append(out, m.Volume)
	}
	return strings.Join(out, " ")
}

// LIFECYCLE-08: a reconcile pass landing while a freeze has created the
// parent's swap volume (before CommitFreeze records it) must not GC it.
func TestReconcileDuringFreezeKeepsParentSwapVolume(t *testing.T) {
	d := &hookDriver{fakeDriver: newFake(), match: "pgoverlay-br-p-rw-g2"}
	e, r := testEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "p", "main", 0); err != nil {
		t.Fatal(err)
	}
	var taken ReconcilePlan
	d.onHelper = func() {
		var err error
		taken, err = e.ApplyReconcile(context.Background(), time.Now(), time.Hour)
		if err != nil {
			t.Errorf("reconcile: %v", err)
		}
	}
	if _, err := e.CreateBranchFrom(context.Background(), "c", "p", 0); err != nil {
		t.Fatal(err)
	}
	for _, a := range taken.Actions {
		if a.Kind == ActionGCVolume {
			t.Fatalf("reconcile GC'd an in-flight volume: %+v", a)
		}
	}
	p, _ := r.GetBranchByName("p")
	if p.RWVolume != "pgoverlay-br-p-rw-g2" || !d.volumes[p.RWVolume] {
		t.Fatalf("parent after freeze: rw=%q present=%v", p.RWVolume, d.volumes[p.RWVolume])
	}
	// the claim is released at commit: nothing else keeps the volume live
	live, _ := r.LiveVolumeSet()
	if !live["pgoverlay-br-p-rw-g2"] || !live["pgoverlay-br-p-rw"] {
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

// Same for a source refresh: the next generation is being seeded before
// BumpSourceGeneration records it.
func TestReconcileDuringRefreshKeepsNewGeneration(t *testing.T) {
	d := &hookDriver{fakeDriver: newFake(), match: "pgoverlay-src-main-g2"}
	e, r := testEngine(t, d)
	readySource(t, r)
	var taken ReconcilePlan
	d.onHelper = func() {
		var err error
		taken, err = e.ApplyReconcile(context.Background(), time.Now(), time.Hour)
		if err != nil {
			t.Errorf("reconcile: %v", err)
		}
	}
	if err := e.RefreshSource(context.Background(), "main", "pw"); err != nil {
		t.Fatal(err)
	}
	for _, a := range taken.Actions {
		if a.Kind == ActionGCVolume {
			t.Fatalf("reconcile GC'd the generation being seeded: %+v", a)
		}
	}
	if !d.volumes["pgoverlay-src-main-g2"] {
		t.Fatal("new generation volume gone")
	}
	s := mustSource(t, r)
	if s.Volume != "pgoverlay-src-main-g2" {
		t.Fatalf("source volume=%q", s.Volume)
	}
}
