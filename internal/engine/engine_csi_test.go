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

// Unit tests for the csi backend: the branch's writable layer is a PVC clone
// of its base volume (CloneVolume), the pod runs the direct entrypoint on the
// clone (no overlay lowers), and cloning a live parent branch briefly stops
// it for a crash-consistent clone.

func csiEngine(t *testing.T, d runtime.Driver, opts ...Option) (*Engine, *registry.Registry) {
	t.Helper()
	r, err := registry.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return NewWithPlanner(r, d, "postgres:17", cow.Planner{Backend: cow.BackendCSI}, opts...), r
}

func TestCSICreateBranchClonesAndStartsDirectPod(t *testing.T) {
	d := newFake()
	e, r := csiEngine(t, d)
	readySource(t, r)

	b, err := e.CreateBranch(context.Background(), "pr-1", "main", 0)
	if err != nil {
		t.Fatal(err)
	}
	if b.State != registry.BranchReady {
		t.Fatalf("state = %q", b.State)
	}
	if b.RWVolume != "pgbranch-br-pr-1-rw" || b.SourceVolume != "pgbranch-src-main" {
		t.Fatalf("volumes: rw=%q source=%q", b.RWVolume, b.SourceVolume)
	}
	// the rw volume is a CloneVolume of the source PVC, not a fresh CreateVolume
	if len(d.clones) != 1 || d.clones[0] != [2]string{"pgbranch-src-main", "pgbranch-br-pr-1-rw"} {
		t.Fatalf("clones = %v, want source->rw", d.clones)
	}
	if i := d.logIndex("volume:pgbranch-br-pr-1-rw"); i >= 0 {
		t.Fatal("csi branch must not CreateVolume its rw layer (it is cloned)")
	}
	// the direct entrypoint is installed into the clone via an unprivileged
	// helper with a plain volume (PVC) mount
	var ep *runtime.HelperSpec
	for i := range d.helpers {
		if strings.Contains(helperCmd(d.helpers[i]), "entrypoint.sh") {
			ep = &d.helpers[i]
		}
	}
	if ep == nil {
		t.Fatal("no entrypoint install helper")
	}
	if ep.Privileged {
		t.Fatal("entrypoint install must not be privileged")
	}
	if len(ep.Env) != 1 || ep.Env[0] != "PGBRANCH_ENTRYPOINT="+cow.EntrypointScriptDirect {
		t.Fatalf("entrypoint helper env = %v, want the direct entrypoint script", ep.Env)
	}
	if len(ep.Mounts) != 1 || ep.Mounts[0].Kind != runtime.MountVolume ||
		ep.Mounts[0].Volume != "pgbranch-br-pr-1-rw" || ep.Mounts[0].Target != cow.RWPath {
		t.Fatalf("entrypoint helper mounts = %+v, want the clone PVC at %s", ep.Mounts, cow.RWPath)
	}
	// branch pod: only the clone mounted rw, PGDATA in its data/ subdir,
	// direct entrypoint, no overlay lowers
	if len(d.branches) != 1 {
		t.Fatalf("StartBranch calls = %d", len(d.branches))
	}
	bs := d.branches[0]
	if len(bs.Mounts) != 1 || bs.Mounts[0].Kind != runtime.MountVolume ||
		bs.Mounts[0].Volume != "pgbranch-br-pr-1-rw" || bs.Mounts[0].Target != cow.RWPath || bs.Mounts[0].ReadOnly {
		t.Fatalf("branch mounts = %+v, want rw clone PVC at %s", bs.Mounts, cow.RWPath)
	}
	if len(bs.Env) != 1 || bs.Env[0] != "PGDATA="+cow.DirectDataPath {
		t.Fatalf("branch env = %v, want only PGDATA=%s (no PGBRANCH_LOWERS)", bs.Env, cow.DirectDataPath)
	}
	if len(bs.Entrypoint) != 2 || bs.Entrypoint[1] != cow.RWPath+"/entrypoint.sh" {
		t.Fatalf("branch entrypoint = %v", bs.Entrypoint)
	}
}

func TestCSICreateBranchUnwindsCloneOnStartFailure(t *testing.T) {
	d := newFake()
	d.failStart = true
	e, r := csiEngine(t, d)
	readySource(t, r)

	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err == nil {
		t.Fatal("want error")
	}
	if d.volumes["pgbranch-br-pr-1-rw"] {
		t.Fatal("clone PVC not removed by compensation")
	}
	b, err := r.GetBranchByName("pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if b.State != registry.BranchFailed {
		t.Fatalf("state = %q, want failed", b.State)
	}
}

// Branch-from-branch in csi mode: CHECKPOINT parent -> stop parent -> clone
// the parent's PVC -> restart parent -> start child. No freeze, no layer rows.
func TestCSICreateBranchFromStopsClonesRestartsParent(t *testing.T) {
	d := newFake()
	e, r := csiEngine(t, d)
	src := readySource(t, r)
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
	// the child's volume is a full clone of the PARENT'S PVC
	if b.RWVolume != "pgbranch-br-pr-2-rw" || b.SourceVolume != "pgbranch-br-pr-1-rw" {
		t.Fatalf("child volumes: rw=%q source=%q", b.RWVolume, b.SourceVolume)
	}
	if len(d.clones) != 2 || d.clones[1] != [2]string{"pgbranch-br-pr-1-rw", "pgbranch-br-pr-2-rw"} {
		t.Fatalf("clones = %v, want parent rw -> child rw", d.clones)
	}
	// ordering: checkpoint -> stop parent -> clone -> restart parent -> child
	ckpt := d.logIndex("exec:psql:cid-pgbranch-br-pr-1")
	stop := d.logIndex("stop:cid-pgbranch-br-pr-1")
	clone := d.logIndex("clone:pgbranch-br-pr-1-rw>pgbranch-br-pr-2-rw")
	if !(ckpt >= 0 && stop >= 0 && clone >= 0 && ckpt < stop && stop < clone) {
		t.Fatalf("order ckpt=%d stop=%d clone=%d; log=%v", ckpt, stop, clone, d.log)
	}
	var parentRestart, childStart int = -1, -1
	for i, ent := range d.log {
		if ent == "start:pgbranch-br-pr-1" && i > stop {
			parentRestart = i
		}
		if ent == "start:pgbranch-br-pr-2" {
			childStart = i
		}
	}
	if !(clone < parentRestart && parentRestart < childStart) {
		t.Fatalf("order clone=%d parentRestart=%d childStart=%d; log=%v", clone, parentRestart, childStart, d.log)
	}
	// the parent is back ready with a (new) container recorded
	p, err := r.GetBranchByName("pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if p.State != registry.BranchReady || p.ContainerID == "" {
		t.Fatalf("parent after clone: %+v", p)
	}
	if !d.containers[p.ContainerID] {
		t.Fatal("parent container not running")
	}
	// PVC clones are independent: no overlay layer rows, no base layer
	if layers, _ := r.ListLayersBySource(src.ID); len(layers) != 0 {
		t.Fatalf("csi mode created layer rows: %v", layers)
	}
	if b.BaseLayerID != "" {
		t.Fatalf("csi child has BaseLayerID %q", b.BaseLayerID)
	}
}

// A checkpoint failure aborts the clone before the parent is ever stopped.
func TestCSICreateBranchFromCheckpointFailureLeavesParentRunning(t *testing.T) {
	d := newFake()
	e, r := csiEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}
	d.psqlErr = errors.New("checkpoint boom")

	if _, err := e.CreateBranchFrom(context.Background(), "pr-2", "pr-1", 0); err == nil {
		t.Fatal("want error")
	}
	if !d.containers["cid-pgbranch-br-pr-1"] {
		t.Fatal("parent container was stopped after a checkpoint failure")
	}
	if p, _ := r.GetBranchByName("pr-1"); p.State != registry.BranchReady {
		t.Fatalf("parent state = %q, want ready", p.State)
	}
	if len(d.clones) != 1 { // only the original pr-1 clone
		t.Fatalf("clones = %v, want no child clone", d.clones)
	}
}

// A child start failure unwinds the clone and restores the parent to ready.
func TestCSICreateBranchFromUnwindRestoresParent(t *testing.T) {
	d := newFake()
	e, r := csiEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}
	// attempt 1 = pr-1 create, attempt 2 = parent restart, attempt 3 = child
	d.failStartAt = map[int]bool{3: true}

	if _, err := e.CreateBranchFrom(context.Background(), "pr-2", "pr-1", 0); err == nil {
		t.Fatal("want error")
	}
	if d.volumes["pgbranch-br-pr-2-rw"] {
		t.Fatal("child clone PVC not removed by compensation")
	}
	p, err := r.GetBranchByName("pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if p.State != registry.BranchReady {
		t.Fatalf("parent state = %q, want ready (restored)", p.State)
	}
	if !d.containers[p.ContainerID] {
		t.Fatal("parent container not restored")
	}
	if c, _ := r.GetBranchByName("pr-2"); c.State != registry.BranchFailed {
		t.Fatalf("child state = %q, want failed", c.State)
	}
}

// A parent restart failure marks the parent failed (its PVC is intact) and
// fails the child without a second restart attempt.
func TestCSICreateBranchFromParentRestartFailure(t *testing.T) {
	d := newFake()
	e, r := csiEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}
	d.failStartAt = map[int]bool{2: true} // the parent restart

	if _, err := e.CreateBranchFrom(context.Background(), "pr-2", "pr-1", 0); err == nil {
		t.Fatal("want error")
	}
	p, _ := r.GetBranchByName("pr-1")
	if p.State != registry.BranchFailed {
		t.Fatalf("parent state = %q, want failed", p.State)
	}
	// the parent's PVC must never be removed — it holds the data
	if !d.volumes["pgbranch-br-pr-1-rw"] {
		t.Fatal("parent PVC removed")
	}
	if d.volumes["pgbranch-br-pr-2-rw"] {
		t.Fatal("child clone PVC not removed by compensation")
	}
}

// Resetting a csi child re-clones from its parent's PVC, quiescing the parent
// again.
func TestCSIResetChildReclonesFromParent(t *testing.T) {
	d := newFake()
	e, r := csiEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateBranchFrom(context.Background(), "pr-2", "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ResetBranch(context.Background(), "pr-2"); err != nil {
		t.Fatal(err)
	}
	var fromParent int
	for _, c := range d.clones {
		if c == [2]string{"pgbranch-br-pr-1-rw", "pgbranch-br-pr-2-rw"} {
			fromParent++
		}
	}
	if fromParent != 2 {
		t.Fatalf("parent->child clones = %d, want 2 (create + reset); clones=%v", fromParent, d.clones)
	}
	if p, _ := r.GetBranchByName("pr-1"); p.State != registry.BranchReady {
		t.Fatalf("parent state = %q, want ready after child reset", p.State)
	}
}

// Destroy removes the branch's PVC; destroying a parent while a child lives
// is allowed (PVC clones are independent, unlike zfs snapshots).
func TestCSIDestroyIndependence(t *testing.T) {
	d := newFake()
	e, r := csiEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateBranchFrom(context.Background(), "pr-2", "pr-1", 0); err != nil {
		t.Fatal(err)
	}

	if err := e.DestroyBranch(context.Background(), "pr-1"); err != nil {
		t.Fatalf("destroy parent with live csi child = %v, want ok", err)
	}
	if d.volumes["pgbranch-br-pr-1-rw"] {
		t.Fatal("parent PVC not removed")
	}
	// the child keeps running on its own clone
	if c, _ := r.GetBranchByName("pr-2"); c.State != registry.BranchReady {
		t.Fatalf("child state = %q", c.State)
	}
	if err := e.DestroyBranch(context.Background(), "pr-2"); err != nil {
		t.Fatal(err)
	}
	if d.volumes["pgbranch-br-pr-2-rw"] {
		t.Fatal("child PVC not removed")
	}
	if len(d.containers) != 0 {
		t.Fatalf("containers leaked: %v", d.containers)
	}
}
