package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/pgoverlay/internal/registry"
)

// crashedFreeze reproduces #12's dead end: a freeze parent p (with its own
// data in pgoverlay-br-p-rw) interrupted mid-freeze by a branchd crash, so p
// sits in resetting and its in-flight child c in creating; a reconcile pass
// past the stuck timeout then fails both, keeping p's rw volume.
func crashedFreeze(t *testing.T, e *Engine, r *registry.Registry, d *fakeDriver) *registry.Branch {
	t.Helper()
	src := readySource(t, r)
	d.addOrphanVolume("pgoverlay-src-main", r.InstanceID()) // labelled, like AddSource makes it
	p := &registry.Branch{Name: "p", SourceID: src.ID, RWVolume: "pgoverlay-br-p-rw", SourceVolume: "pgoverlay-src-main"}
	if err := r.CreateBranch(p); err != nil {
		t.Fatal(err)
	}
	d.addOrphanVolume("pgoverlay-br-p-rw", r.InstanceID())
	markReady(t, r, p, "cid-p-old")
	if err := r.SetBranchPassword(p.ID, "p-secret"); err != nil {
		t.Fatal(err)
	}
	if err := r.TransitionBranch(p.ID, registry.BranchResetting, "freeze for child c"); err != nil {
		t.Fatal(err)
	}
	c := &registry.Branch{Name: "c", SourceID: src.ID, RWVolume: "pgoverlay-br-c-rw",
		SourceVolume: "pgoverlay-src-main", ParentBranchName: "p"}
	if err := r.CreateBranch(c); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ApplyReconcile(context.Background(), time.Now().Add(time.Hour), 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	got, _ := r.GetBranchByName("p")
	if got.State != registry.BranchFailed || !d.volumes["pgoverlay-br-p-rw"] {
		t.Fatalf("setup: parent %+v, volume present=%v; want failed with its volume kept", got, d.volumes["pgoverlay-br-p-rw"])
	}
	return got
}

// #12: the crash-failed freeze parent is recoverable without data loss:
// RecoverBranch restarts it on its own rw volume and chain — no re-clone, no
// masking, same password — and it is ready again.
func TestRecoverCrashFailedFreezeParent(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	crashedFreeze(t, e, r, d)
	if err := r.SetMaskScripts(mustSource(t, r).ID, []registry.MaskScript{{Name: "m", SQL: "UPDATE t SET x=1"}}); err != nil {
		t.Fatal(err)
	}
	startsBefore, volsBefore := d.starts, len(d.log)

	p, err := e.RecoverBranch(context.Background(), "p")
	if err != nil {
		t.Fatalf("RecoverBranch: %v", err)
	}
	if p.State != registry.BranchReady || p.ContainerID == "" || p.Host == "" {
		t.Fatalf("recovered parent: %+v", p)
	}
	if p.RWVolume != "pgoverlay-br-p-rw" || p.Password != "p-secret" {
		t.Fatalf("recover changed the data identity: rw=%q password=%q", p.RWVolume, p.Password)
	}
	if d.starts != startsBefore+1 {
		t.Fatalf("starts=%d want one restart", d.starts-startsBefore)
	}
	assertOverlaySpec(t, d.branches[len(d.branches)-1], "pgoverlay-br-p-rw", "pgoverlay-src-main", nil)
	for _, entry := range d.log[volsBefore:] {
		if strings.HasPrefix(entry, "rmvolume:") || strings.HasPrefix(entry, "volume:") {
			t.Fatalf("recover touched volumes: %v", d.log[volsBefore:])
		}
	}
	if n := len(d.psqlExecs()); n != 0 {
		t.Fatalf("recover ran %d psql execs (masking/rotation must not re-run on existing data)", n)
	}
	h, _ := r.BranchHistory("p")
	var sawRecover bool
	for _, tr := range h {
		if tr.FromState == "failed" && tr.ToState == "resetting" && strings.Contains(tr.Reason, "recover") {
			sawRecover = true
		}
	}
	if !sawRecover {
		t.Fatalf("recover not journaled: %+v", h)
	}
	// and the recovered parent can be branched from again
	if _, err := e.CreateBranchFrom(context.Background(), "c2", "p", 0); err != nil {
		t.Fatalf("branch from recovered parent: %v", err)
	}
}

func TestRecoverRefusesNonFailedBranch(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}
	_, err := e.RecoverBranch(context.Background(), "pr-1")
	if !errors.Is(err, ErrNotRecoverable) || !strings.Contains(err.Error(), "is ready") {
		t.Fatalf("recover ready branch: err=%v, want ErrNotRecoverable", err)
	}
}

// A failed create's undo removed the rw volume: there is nothing to restart
// on, so recover refuses (reset retries the create instead) and changes
// nothing — it never boots an empty auto-created volume as "recovered".
func TestRecoverRefusesWhenDataIsGone(t *testing.T) {
	d := newFake()
	d.failStart = true
	e, r := testEngine(t, d)
	readySource(t, r)
	d.addOrphanVolume("pgoverlay-src-main", r.InstanceID())
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err == nil {
		t.Fatal("want create to fail")
	}
	d.failStart = false
	starts := d.startAttempts
	_, err := e.RecoverBranch(context.Background(), "pr-1")
	if !errors.Is(err, ErrNotRecoverable) || !strings.Contains(err.Error(), "pgoverlay-br-pr-1-rw") {
		t.Fatalf("recover without data: err=%v, want ErrNotRecoverable naming the volume", err)
	}
	if d.startAttempts != starts {
		t.Fatal("recover started a container despite missing data")
	}
	if b, _ := r.GetBranchByName("pr-1"); b.State != registry.BranchFailed {
		t.Fatalf("state=%s want failed (unchanged)", b.State)
	}
	// reset is the way forward: it retries the create
	b, err := e.ResetBranch(context.Background(), "pr-1")
	if err != nil {
		t.Fatalf("reset from failed: %v", err)
	}
	if b.State != registry.BranchReady {
		t.Fatalf("state after reset=%s", b.State)
	}
}

// A restart that fails returns the branch to failed and leaves its data.
func TestRecoverFailureKeepsDataAndFailedState(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	crashedFreeze(t, e, r, d)
	d.failStart = true
	if _, err := e.RecoverBranch(context.Background(), "p"); err == nil {
		t.Fatal("want recover to fail")
	}
	p, _ := r.GetBranchByName("p")
	if p.State != registry.BranchFailed {
		t.Fatalf("state=%s want failed", p.State)
	}
	if !d.volumes["pgoverlay-br-p-rw"] {
		t.Fatal("failed recover removed the branch's data")
	}
	last := lastTransition(t, r, "p")
	if last.ToState != "failed" || !strings.Contains(last.Reason, "recover failed") {
		t.Fatalf("last transition %+v", last)
	}
	// it can be tried again
	d.failStart = false
	if _, err := e.RecoverBranch(context.Background(), "p"); err != nil {
		t.Fatalf("second recover: %v", err)
	}
}

// A csi branch recovers onto its own PVC with the direct entrypoint.
func TestRecoverCSIBranchStartsOnItsPVC(t *testing.T) {
	d := newFake()
	e, r := csiEngine(t, d)
	readySource(t, r)
	b, err := e.CreateBranch(context.Background(), "pr-1", "main", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.TransitionBranch(b.ID, registry.BranchResetting, "stop for clone to x"); err != nil {
		t.Fatal(err)
	}
	if err := r.TransitionBranch(b.ID, registry.BranchFailed, "restart failed"); err != nil {
		t.Fatal(err)
	}
	clones := len(d.clones)
	got, err := e.RecoverBranch(context.Background(), "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != registry.BranchReady || len(d.clones) != clones {
		t.Fatalf("recovered %+v, clones %d -> %d (must not re-clone)", got, clones, len(d.clones))
	}
	spec := d.branches[len(d.branches)-1]
	if m := mountAt(t, spec, "/pgoverlay/rw"); m.Volume != b.RWVolume {
		t.Fatalf("csi recover mounted %q, want its PVC %q", m.Volume, b.RWVolume)
	}
}

// Reset and recover refuse while a child is still being created from the
// branch: its volumes may be mid-freeze or mid-clone.
func TestResetAndRecoverRefuseWithInFlightChild(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	src := readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "p", "main", 0); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateBranch(&registry.Branch{Name: "c", SourceID: src.ID, RWVolume: "pgoverlay-br-c-rw",
		SourceVolume: "pgoverlay-src-main", ParentBranchName: "p"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ResetBranch(context.Background(), "p"); err == nil || !strings.Contains(err.Error(), `in-flight child branch "c"`) {
		t.Fatalf("reset with in-flight child: %v", err)
	}
	p, _ := r.GetBranchByName("p")
	if p.State != registry.BranchReady {
		t.Fatalf("refused reset changed state to %s", p.State)
	}
	if err := r.TransitionBranch(p.ID, registry.BranchResetting, "x"); err != nil {
		t.Fatal(err)
	}
	if err := r.TransitionBranch(p.ID, registry.BranchFailed, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RecoverBranch(context.Background(), "p"); err == nil || !strings.Contains(err.Error(), "child branch") {
		t.Fatalf("recover with in-flight child: %v", err)
	}
}

// LIFECYCLE-11: resetting a zfs parent with live clones is refused before
// anything changes (zfs destroy -r of its dataset would fail after the
// container was already gone, leaving a healthy parent failed).
func TestZFSResetParentWithChildrenRefused(t *testing.T) {
	d := newFake()
	e, r := zfsEngine(t, d)
	readyZFSSource(t, r)
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateBranchFrom(context.Background(), "pr-2", "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	stops := strings.Count(strings.Join(d.log, "\n"), "stop:")
	_, err := e.ResetBranch(context.Background(), "pr-1")
	if err == nil || !strings.Contains(err.Error(), "child branch") {
		t.Fatalf("zfs reset of a parent with clones: err=%v, want child-branch refusal", err)
	}
	if b, _ := r.GetBranchByName("pr-1"); b.State != registry.BranchReady {
		t.Fatalf("refused reset left parent %s, want ready", b.State)
	}
	if got := strings.Count(strings.Join(d.log, "\n"), "stop:"); got != stops {
		t.Fatal("refused reset stopped the parent's container")
	}
}
