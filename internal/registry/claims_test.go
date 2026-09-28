package registry

import (
	"errors"
	"testing"
)

// A pending-volume claim keeps a volume live while its saga runs: on a branch
// only while the branch is creating/resetting (a crash leaves the row failed
// and frees the volume for GC), on a source until the generation bump.
func TestPendingVolumeClaims(t *testing.T) {
	r := openTest(t)
	s := mkSource(t, r, "main")
	b := mkBranch(t, r, &Branch{Name: "p", SourceID: s.ID})
	if err := r.MarkBranchReady(b.ID, "c", "127.0.0.1", 1); err != nil {
		t.Fatal(err)
	}
	if err := r.TransitionBranch(b.ID, BranchResetting, "freeze for child c"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetBranchPendingVolume(b.ID, "pgoverlay-br-p-rw-g2"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetSourcePendingVolume(s.ID, "pgoverlay-src-main-g2"); err != nil {
		t.Fatal(err)
	}
	live, _ := r.LiveVolumeSet()
	if !live["pgoverlay-br-p-rw-g2"] || !live["pgoverlay-src-main-g2"] {
		t.Fatalf("claims not live: %v", live)
	}
	if used, _ := r.VolumeNameUsed("pgoverlay-br-p-rw-g2"); !used {
		t.Fatal("claimed volume name reported unused")
	}
	// the saga died: reconcile fails the row, the claim stops counting
	if err := r.TransitionBranch(b.ID, BranchFailed, "reconcile: stuck resetting"); err != nil {
		t.Fatal(err)
	}
	if live, _ := r.LiveVolumeSet(); live["pgoverlay-br-p-rw-g2"] {
		t.Fatal("claim of a failed branch still pins its volume")
	}
	if err := r.BumpSourceGeneration(s.ID, "pgoverlay-src-main-g2"); err != nil {
		t.Fatal(err)
	}
	var pending string
	if err := r.db.QueryRow(`SELECT pending_volume FROM sources WHERE id=?`, s.ID).Scan(&pending); err != nil || pending != "" {
		t.Fatalf("generation bump left pending=%q err=%v", pending, err)
	}
	if err := r.SetBranchPendingVolume("missing", "v"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown branch: %v", err)
	}
}
