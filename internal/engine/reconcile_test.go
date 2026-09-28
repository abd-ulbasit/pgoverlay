package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/pgoverlay/internal/registry"
	"github.com/abd-ulbasit/pgoverlay/internal/runtime"
)

// seedReady marks a freshly-created creating branch ready so it counts as live
// with a stable rw volume (no driver provisioning involved).
func markReady(t *testing.T, r *registry.Registry, b *registry.Branch, cid string) {
	t.Helper()
	if err := r.MarkBranchReady(b.ID, cid, "127.0.0.1", 54321); err != nil {
		t.Fatal(err)
	}
}

// TestReconcileFailsStuckCreating: a creating row older than the stuck timeout
// is failed and its rw volume is removed.
func TestReconcileFailsStuckCreating(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	b := &registry.Branch{Name: "stuck", SourceID: mustSource(t, r).ID, RWVolume: "pgoverlay-br-stuck-rw"}
	if err := r.CreateBranch(b); err != nil {
		t.Fatal(err)
	}
	d.volumes["pgoverlay-br-stuck-rw"] = true

	// now in the future so the just-inserted row is past the 10m timeout.
	taken, err := e.ApplyReconcile(context.Background(), time.Now().Add(time.Hour), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !hasAction(taken, ActionFailStuck, "stuck") {
		t.Fatalf("no fail_stuck action: %+v", taken.Actions)
	}
	got, _ := r.GetBranchByName("stuck")
	if got.State != registry.BranchFailed {
		t.Fatalf("state=%q want failed", got.State)
	}
	if d.volumes["pgoverlay-br-stuck-rw"] {
		t.Fatal("stuck rw volume not removed")
	}
}

// A recently-created creating row (within the timeout) is left alone.
func TestReconcileLeavesFreshCreating(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	b := &registry.Branch{Name: "fresh", SourceID: mustSource(t, r).ID, RWVolume: "pgoverlay-br-fresh-rw"}
	if err := r.CreateBranch(b); err != nil {
		t.Fatal(err)
	}
	d.volumes["pgoverlay-br-fresh-rw"] = true

	taken, err := e.ApplyReconcile(context.Background(), time.Now(), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if hasAction(taken, ActionFailStuck, "fresh") {
		t.Fatalf("fresh creating row was failed: %+v", taken.Actions)
	}
	if got, _ := r.GetBranchByName("fresh"); got.State != registry.BranchCreating {
		t.Fatalf("state=%q want still creating", got.State)
	}
}

// A managed container with no live registry row is removed; a container backing
// a live branch is kept.
func TestReconcileRemovesOrphanContainer(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	// a live ready branch with a known container.
	b := &registry.Branch{Name: "live", SourceID: mustSource(t, r).ID, RWVolume: "pgoverlay-br-live-rw"}
	if err := r.CreateBranch(b); err != nil {
		t.Fatal(err)
	}
	d.volumes["pgoverlay-br-live-rw"] = true
	markReady(t, r, b, "cid-live")
	d.addOrphanContainer("cid-live", r.InstanceID())
	// an orphan with no row.
	d.addOrphanContainer("cid-ghost", r.InstanceID())

	taken, err := e.ApplyReconcile(context.Background(), time.Now(), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !hasAction(taken, ActionRemoveOrphanContainer, "cid-ghost") {
		t.Fatalf("ghost not removed: %+v", taken.Actions)
	}
	if d.containers["cid-ghost"] {
		t.Fatal("ghost container still present")
	}
	if !d.containers["cid-live"] {
		t.Fatal("live branch container was removed")
	}
}

// A branch still provisioning (state creating) whose container was recorded
// via SetBranchContainer before the readiness wait must NOT be reaped. This is
// the within-instance race the api IT exposed: a fast reconcile loop fires
// while a branch is mid-create, and the in-flight container is owned.
func TestReconcileSkipsInflightProvisioningContainer(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	b := &registry.Branch{Name: "inflight", SourceID: mustSource(t, r).ID, RWVolume: "pgoverlay-br-inflight-rw"}
	if err := r.CreateBranch(b); err != nil { // state = creating
		t.Fatal(err)
	}
	// provisioning started the container and recorded it before readiness:
	if err := r.SetBranchContainer(b.ID, "cid-inflight"); err != nil {
		t.Fatal(err)
	}
	d.addOrphanContainer("cid-inflight", r.InstanceID())

	taken, err := e.ApplyReconcile(context.Background(), time.Now(), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if hasAction(taken, ActionRemoveOrphanContainer, "cid-inflight") {
		t.Fatalf("in-flight provisioning container was reaped: %+v", taken.Actions)
	}
	if !d.containers["cid-inflight"] {
		t.Fatal("in-flight container removed")
	}
}

// A frozen layer with refcount 0 is GC'd (volume + row); a layer still in a
// live branch's chain is kept.
func TestReconcileGCsDanglingLayerKeepsReferenced(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	src := readySource(t, r)

	// dangling layer: refcount 0.
	dangling := &registry.Layer{SourceID: src.ID, Volume: "pgoverlay-layer-dangling"}
	if err := r.CreateLayer(dangling); err != nil {
		t.Fatal(err)
	}
	d.volumes["pgoverlay-layer-dangling"] = true

	// referenced layer: a live branch chains onto it.
	referenced := &registry.Layer{SourceID: src.ID, Volume: "pgoverlay-layer-referenced"}
	if err := r.CreateLayer(referenced); err != nil {
		t.Fatal(err)
	}
	d.volumes["pgoverlay-layer-referenced"] = true
	b := &registry.Branch{Name: "child", SourceID: src.ID, RWVolume: "pgoverlay-br-child-rw", BaseLayerID: referenced.ID}
	if err := r.CreateBranch(b); err != nil {
		t.Fatal(err)
	}
	d.volumes["pgoverlay-br-child-rw"] = true
	markReady(t, r, b, "cid-child")
	d.containers["cid-child"] = true

	taken, err := e.ApplyReconcile(context.Background(), time.Now(), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !hasAction(taken, ActionGCLayer, "pgoverlay-layer-dangling") {
		t.Fatalf("dangling layer not GC'd: %+v", taken.Actions)
	}
	if hasAction(taken, ActionGCLayer, "pgoverlay-layer-referenced") {
		t.Fatalf("referenced layer GC'd: %+v", taken.Actions)
	}
	if d.volumes["pgoverlay-layer-dangling"] {
		t.Fatal("dangling layer volume survived")
	}
	if !d.volumes["pgoverlay-layer-referenced"] {
		t.Fatal("referenced layer volume removed")
	}
	if _, err := r.GetLayer(dangling.ID); err == nil {
		t.Fatal("dangling layer row survived")
	}
	if _, err := r.GetLayer(referenced.ID); err != nil {
		t.Fatalf("referenced layer row removed: %v", err)
	}
}

// An rw volume owned by no live branch is removed; an in-use rw volume is kept.
func TestReconcileGCsOrphanVolumeKeepsInUse(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	d.volumes["pgoverlay-src-main"] = true // source generation: in use

	// in-use rw volume: a live branch owns it.
	b := &registry.Branch{Name: "live", SourceID: mustSource(t, r).ID, RWVolume: "pgoverlay-br-live-rw"}
	if err := r.CreateBranch(b); err != nil {
		t.Fatal(err)
	}
	d.volumes["pgoverlay-br-live-rw"] = true
	markReady(t, r, b, "cid-live")
	d.containers["cid-live"] = true

	// orphan volume: no branch, no source, no layer references it.
	d.addOrphanVolume("pgoverlay-br-orphan-rw", r.InstanceID())

	taken, err := e.ApplyReconcile(context.Background(), time.Now(), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !hasAction(taken, ActionGCVolume, "pgoverlay-br-orphan-rw") {
		t.Fatalf("orphan volume not GC'd: %+v", taken.Actions)
	}
	if d.volumes["pgoverlay-br-orphan-rw"] {
		t.Fatal("orphan volume survived")
	}
	if !d.volumes["pgoverlay-br-live-rw"] {
		t.Fatal("in-use rw volume removed")
	}
	if !d.volumes["pgoverlay-src-main"] {
		t.Fatal("source generation volume removed")
	}
}

// Dry mode (PlanReconcile) lists actions without applying: the driver records
// no deletes and the registry is untouched.
func TestPlanReconcileDryRunDoesNotApply(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)

	// stuck row.
	b := &registry.Branch{Name: "stuck", SourceID: mustSource(t, r).ID, RWVolume: "pgoverlay-br-stuck-rw"}
	if err := r.CreateBranch(b); err != nil {
		t.Fatal(err)
	}
	d.volumes["pgoverlay-br-stuck-rw"] = true
	// orphan container + orphan volume.
	d.addOrphanContainer("cid-ghost", r.InstanceID())
	d.addOrphanVolume("pgoverlay-br-orphan-rw", r.InstanceID())

	before := len(d.log)
	plan, err := e.PlanReconcile(context.Background(), time.Now().Add(time.Hour), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Drift() {
		t.Fatal("plan reported no drift")
	}
	if !hasAction(plan, ActionFailStuck, "stuck") ||
		!hasAction(plan, ActionRemoveOrphanContainer, "cid-ghost") ||
		!hasAction(plan, ActionGCVolume, "pgoverlay-br-orphan-rw") {
		t.Fatalf("plan missing expected actions: %+v", plan.Actions)
	}
	// nothing mutated: driver log unchanged, registry row still creating,
	// resources still present.
	if len(d.log) != before {
		t.Fatalf("dry-run mutated the driver: %v", d.log[before:])
	}
	if !d.containers["cid-ghost"] || !d.volumes["pgoverlay-br-orphan-rw"] || !d.volumes["pgoverlay-br-stuck-rw"] {
		t.Fatal("dry-run removed resources")
	}
	if got, _ := r.GetBranchByName("stuck"); got.State != registry.BranchCreating {
		t.Fatalf("dry-run changed state to %q", got.State)
	}
}

// A clean system yields an empty plan (no drift).
func TestPlanReconcileCleanNoDrift(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	plan, err := e.PlanReconcile(context.Background(), time.Now(), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Drift() {
		t.Fatalf("clean system reported drift: %+v", plan.Actions)
	}
}

// Reconcile is instance-scoped: an engine bound to registry A must reclaim its
// OWN orphaned managed container and volume, but must leave a managed container
// and volume tagged with a different instance id (a sibling pgoverlay sharing
// the daemon) untouched. This is the regression guard for the CI bug where one
// IT package's reconcile deleted another package's live resources.
func TestReconcileIgnoresForeignInstanceResources(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	const foreign = "ffffffffffffffff" // some other registry's instance id

	// our own orphans (tagged with this registry's instance id) -> reclaimed.
	d.addOrphanContainer("cid-mine", r.InstanceID())
	d.addOrphanVolume("pgoverlay-br-mine-rw", r.InstanceID())
	// a sibling instance's live resources -> must be left alone.
	d.addOrphanContainer("cid-foreign", foreign)
	d.addOrphanVolume("pgoverlay-br-foreign-rw", foreign)

	taken, err := e.ApplyReconcile(context.Background(), time.Now(), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// own resources reclaimed.
	if !hasAction(taken, ActionRemoveOrphanContainer, "cid-mine") {
		t.Fatalf("own orphan container not reclaimed: %+v", taken.Actions)
	}
	if !hasAction(taken, ActionGCVolume, "pgoverlay-br-mine-rw") {
		t.Fatalf("own orphan volume not reclaimed: %+v", taken.Actions)
	}
	if d.containers["cid-mine"] || d.volumes["pgoverlay-br-mine-rw"] {
		t.Fatal("own orphans survived reconcile")
	}
	// foreign resources untouched in plan and in the driver.
	if hasAction(taken, ActionRemoveOrphanContainer, "cid-foreign") {
		t.Fatalf("foreign container reclaimed: %+v", taken.Actions)
	}
	if hasAction(taken, ActionGCVolume, "pgoverlay-br-foreign-rw") {
		t.Fatalf("foreign volume reclaimed: %+v", taken.Actions)
	}
	if !d.containers["cid-foreign"] {
		t.Fatal("foreign instance's container was removed")
	}
	if !d.volumes["pgoverlay-br-foreign-rw"] {
		t.Fatal("foreign instance's volume was removed")
	}
}

func hasAction(p ReconcilePlan, kind ActionKind, target string) bool {
	for _, a := range p.Actions {
		if a.Kind == kind && a.Target == target {
			return true
		}
	}
	return false
}

// TestReconcileStuckResettingParentKeepsReferencedRWVolume is the freeze
// data-loss regression guard. A branch-from-branch freeze parks the PARENT in
// resetting and keeps the parent's live data in parent.RWVolume until
// CommitFreeze. If that freeze is slow (cold pull / large WAL replay) and
// exceeds the stuck timeout, reconcile used to call removeBranchLayer(parent)
// and permanently delete the parent's live data volume. The fail-stuck action
// must SKIP layer removal whenever another live branch references the volume
// (the in-flight child does), while still moving the row out of the stuck
// state so it is not flagged forever.
func TestReconcileStuckResettingParentKeepsReferencedRWVolume(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	src := readySource(t, r)

	// parent: a ready branch with real data in its rw volume.
	parent := &registry.Branch{Name: "parent", SourceID: src.ID,
		RWVolume: "pgoverlay-br-parent-rw", SourceVolume: "pgoverlay-src-main"}
	if err := r.CreateBranch(parent); err != nil {
		t.Fatal(err)
	}
	d.volumes["pgoverlay-br-parent-rw"] = true
	markReady(t, r, parent, "cid-parent")

	// child: created (creating) by freezeAndProvision before CommitFreeze. The
	// overlay freeze records source_volume = the SOURCE and links to the parent
	// by name; csi/zfs would record source_volume = parent.RWVolume. Cover the
	// stricter overlay shape (parent_branch_name only).
	child := &registry.Branch{Name: "child", SourceID: src.ID,
		RWVolume: "pgoverlay-br-child-rw", SourceVolume: "pgoverlay-src-main",
		ParentBranchName: parent.Name}
	if err := r.CreateBranch(child); err != nil {
		t.Fatal(err)
	}
	d.volumes["pgoverlay-br-child-rw"] = true

	// freeze parks the parent in resetting (legal ready->resetting); reconciling
	// an hour in the future makes the row look stuck past the 10m timeout.
	if err := r.TransitionBranch(parent.ID, registry.BranchResetting, "freeze for child"); err != nil {
		t.Fatal(err)
	}

	taken, err := e.ApplyReconcile(context.Background(), time.Now().Add(time.Hour), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// it still fails the stuck row (so it is not stuck forever)...
	if !hasAction(taken, ActionFailStuck, "parent") {
		t.Fatalf("expected fail_stuck on the stuck parent: %+v", taken.Actions)
	}
	got, _ := r.GetBranchByName("parent")
	if got.State != registry.BranchFailed {
		t.Fatalf("parent state=%q want failed", got.State)
	}
	// ...but it MUST NOT delete the parent's live data volume (the child still
	// references it).
	if !d.volumes["pgoverlay-br-parent-rw"] {
		t.Fatal("DATA LOSS: reconcile deleted the freeze parent's live rw volume")
	}
}

// CSI/ZFS shape: the child records the parent's rw volume directly as its
// source_volume. The same volume-reference guard must protect it.
func TestReconcileStuckResettingParentKeepsRWReferencedBySourceVolume(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	src := readySource(t, r)

	parent := &registry.Branch{Name: "parent", SourceID: src.ID,
		RWVolume: "pgoverlay-br-parent-rw", SourceVolume: "pgoverlay-src-main"}
	if err := r.CreateBranch(parent); err != nil {
		t.Fatal(err)
	}
	d.volumes["pgoverlay-br-parent-rw"] = true
	markReady(t, r, parent, "cid-parent")

	// csi/zfs child: source_volume = parent.RWVolume.
	child := &registry.Branch{Name: "child", SourceID: src.ID,
		RWVolume: "pgoverlay-br-child-rw", SourceVolume: parent.RWVolume,
		ParentBranchName: parent.Name}
	if err := r.CreateBranch(child); err != nil {
		t.Fatal(err)
	}
	d.volumes["pgoverlay-br-child-rw"] = true

	if err := r.TransitionBranch(parent.ID, registry.BranchResetting, "freeze for child"); err != nil {
		t.Fatal(err)
	}

	if _, err := e.ApplyReconcile(context.Background(), time.Now().Add(time.Hour), 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if !d.volumes["pgoverlay-br-parent-rw"] {
		t.Fatal("DATA LOSS: reconcile deleted the csi/zfs freeze parent's rw volume")
	}
}

// --- registry -> runtime drift (issue #8) ---

// readyBranch provisions a branch through the engine (fake driver) and returns
// its row.
func readyBranch(t *testing.T, e *Engine, name string) *registry.Branch {
	t.Helper()
	b, err := e.CreateBranch(context.Background(), name, "main", 0)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustBranch(t *testing.T, r *registry.Registry, name string) *registry.Branch {
	t.Helper()
	b, err := r.GetBranchByName(name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A ready branch whose container was removed behind pgoverlay's back
// (`docker rm -f`, a deleted or drained pod) is drift: doctor reports it, and
// reconcile starts a new container on the branch's EXISTING volumes (its
// writes live there) and records it — instead of leaving a "ready" row that
// routes to nothing.
func TestReconcileRestartsReadyBranchWhoseContainerIsGone(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	b := readyBranch(t, e, "pr-1")
	delete(d.containers, b.ContainerID) // docker rm -f

	before := len(d.log)
	plan, err := e.PlanReconcile(context.Background(), time.Now(), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !hasAction(plan, ActionRestartBranch, "pr-1") {
		t.Fatalf("doctor does not report the missing container: %+v", plan.Actions)
	}
	if len(d.log) != before {
		t.Fatalf("plan mutated the driver: %v", d.log[before:])
	}

	starts := d.starts
	taken, err := e.ApplyReconcile(context.Background(), time.Now(), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !hasAction(taken, ActionRestartBranch, "pr-1") {
		t.Fatalf("restart not applied: %+v", taken.Actions)
	}
	if d.starts != starts+1 {
		t.Fatalf("starts = %d, want one new container", d.starts-starts)
	}
	got := mustBranch(t, r, "pr-1")
	if got.State != registry.BranchReady || !d.containers[got.ContainerID] || got.Host != "127.0.0.1" || got.Port != 54321 {
		t.Fatalf("after restart: %+v (container present=%v)", got, d.containers[got.ContainerID])
	}
	// the restart reuses the branch's rw volume and overlay stack; it never
	// creates or removes a volume
	for _, entry := range d.log[before:] {
		if strings.HasPrefix(entry, "volume:") || strings.HasPrefix(entry, "rmvolume:") {
			t.Fatalf("restart touched volumes: %v", d.log[before:])
		}
	}
	spec := d.branches[len(d.branches)-1]
	if spec.Name != "pgoverlay-br-pr-1" || spec.Mounts[len(spec.Mounts)-1].Volume != b.RWVolume || spec.Mounts[0].Volume != b.SourceVolume {
		t.Fatalf("restarted spec = %+v, want the branch's own overlay stack", spec)
	}
	// converged: the next plan is clean
	if plan, _ := e.PlanReconcile(context.Background(), time.Now(), 10*time.Minute); plan.Drift() {
		t.Fatalf("drift after restart: %+v", plan.Actions)
	}
}

// A container that is stopped for good (docker stop, exited and not restarted
// by the policy, an evicted pod) is replaced the same way; the dead container
// is removed first.
func TestReconcileRestartsStoppedContainer(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	b := readyBranch(t, e, "pr-1")
	d.containerState[b.ContainerID] = runtime.ContainerInfo{Stopped: true, Status: "exited (0)"}

	taken, err := e.ApplyReconcile(context.Background(), time.Now(), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !hasAction(taken, ActionRestartBranch, "pr-1") {
		t.Fatalf("stopped container not restarted: %+v", taken.Actions)
	}
	if d.logIndex("stop:"+b.ContainerID) < 0 {
		t.Fatal("the stopped container was not removed before the restart")
	}
	if got := mustBranch(t, r, "pr-1"); got.State != registry.BranchReady || !d.containers[got.ContainerID] {
		t.Fatalf("after restart: %+v", got)
	}
}

// A container the runtime is still bringing back (docker restarting, a
// Pending or terminating pod) is not drift yet.
func TestReconcileLeavesContainerInFluxAlone(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	b := readyBranch(t, e, "pr-1")
	d.containerState[b.ContainerID] = runtime.ContainerInfo{Status: "restarting"}

	plan, err := e.PlanReconcile(context.Background(), time.Now(), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Drift() {
		t.Fatalf("container in flux reported as drift: %+v", plan.Actions)
	}
}

// A container running on a different address than the registry routes to —
// docker re-published it on another port, or the pod came back with a new IP —
// has the new address recorded.
func TestReconcileRecordsMovedEndpoint(t *testing.T) {
	for _, moved := range []runtime.ContainerInfo{
		{Running: true, Host: "127.0.0.1", Port: 40002}, // docker: new host port
		{Running: true, Host: "10.244.0.9", Port: 5432}, // kube: new pod IP
	} {
		d := newFake()
		e, r := testEngine(t, d)
		readySource(t, r)
		b := readyBranch(t, e, "pr-1")
		d.containerState[b.ContainerID] = moved

		plan, err := e.PlanReconcile(context.Background(), time.Now(), 10*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if !hasAction(plan, ActionUpdateEndpoint, "pr-1") {
			t.Fatalf("moved endpoint %s:%d not reported: %+v", moved.Host, moved.Port, plan.Actions)
		}
		starts := d.starts
		if _, err := e.ApplyReconcile(context.Background(), time.Now(), 10*time.Minute); err != nil {
			t.Fatal(err)
		}
		got := mustBranch(t, r, "pr-1")
		if got.Host != moved.Host || got.Port != moved.Port || got.ContainerID != b.ContainerID {
			t.Fatalf("endpoint = %s:%d (container %s), want %s:%d on the same container", got.Host, got.Port, got.ContainerID, moved.Host, moved.Port)
		}
		if d.starts != starts {
			t.Fatal("a moved endpoint must not restart the branch")
		}
	}
}

// A branch that does not come back after the restart is failed with the
// reason, through legal transitions, and keeps its volumes; the container that
// never became ready is removed.
func TestReconcileFailsBranchThatDoesNotComeBack(t *testing.T) {
	old := restartReadyTimeout
	restartReadyTimeout = time.Millisecond
	t.Cleanup(func() { restartReadyTimeout = old })

	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	b := readyBranch(t, e, "pr-1")
	delete(d.containers, b.ContainerID)
	d.execErr = errors.New("no response") // pg_isready never succeeds

	_, err := e.ApplyReconcile(context.Background(), time.Now(), 10*time.Minute)
	if err == nil || !strings.Contains(err.Error(), "marked failed") {
		t.Fatalf("ApplyReconcile = %v, want the restart failure reported", err)
	}
	got := mustBranch(t, r, "pr-1")
	if got.State != registry.BranchFailed {
		t.Fatalf("state = %q, want failed", got.State)
	}
	if !d.volumes[b.RWVolume] {
		t.Fatal("the failed branch's rw volume was removed; its data must be kept")
	}
	if d.containers[got.ContainerID] {
		t.Fatal("the container that never became ready was left running")
	}
	hist, err := r.BranchHistory("pr-1")
	if err != nil {
		t.Fatal(err)
	}
	last := hist[len(hist)-1]
	if last.FromState != "resetting" || last.ToState != "failed" || !strings.Contains(last.Reason, "restart failed") {
		t.Fatalf("last transition = %+v", last)
	}
	// failed rows are not ready: no further restarts are planned
	if plan, _ := e.PlanReconcile(context.Background(), time.Now(), 10*time.Minute); hasAction(plan, ActionRestartBranch, "pr-1") {
		t.Fatalf("restart planned again for a failed branch: %+v", plan.Actions)
	}
}

// csi branches restart as a direct pod on their own PVC.
func TestReconcileRestartsCSIBranchOnItsPVC(t *testing.T) {
	d := newFake()
	e, r := csiEngine(t, d)
	readySource(t, r)
	b := readyBranch(t, e, "pr-1")
	delete(d.containers, b.ContainerID)

	if _, err := e.ApplyReconcile(context.Background(), time.Now(), 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	spec := d.branches[len(d.branches)-1]
	if len(spec.Mounts) != 1 || spec.Mounts[0].Volume != b.RWVolume {
		t.Fatalf("restarted csi spec mounts = %+v, want only the branch PVC", spec.Mounts)
	}
	if len(d.clones) != 1 {
		t.Fatalf("clones = %v: a restart must not re-clone the PVC", d.clones)
	}
	if got := mustBranch(t, r, "pr-1"); got.State != registry.BranchReady || !d.containers[got.ContainerID] {
		t.Fatalf("after restart: %+v", got)
	}
}

// A container that holds the branch's name but was never recorded (a restart
// interrupted between start and record) is removed before the new one
// starts; one that belongs to something else stops the restart.
func TestReconcileRestartClearsStrayContainerName(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	b := readyBranch(t, e, "pr-1")
	delete(d.containers, b.ContainerID)
	// kube-style id == name, labelled for this branch
	d.containers["pgoverlay-br-pr-1"] = true
	d.containerLbls["pgoverlay-br-pr-1"] = e.branchLabels(b)

	if _, err := e.ApplyReconcile(context.Background(), time.Now(), 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if d.logIndex("stop:pgoverlay-br-pr-1") < 0 {
		t.Fatal("stray same-named container was not removed")
	}

	// foreign holder of the name: refuse
	got := mustBranch(t, r, "pr-1")
	delete(d.containers, got.ContainerID)
	d.containers["pgoverlay-br-pr-1"] = true
	d.containerLbls["pgoverlay-br-pr-1"] = map[string]string{runtime.LabelInstance: "someone-else"}
	_, err := e.ApplyReconcile(context.Background(), time.Now(), 10*time.Minute)
	if err == nil || !strings.Contains(err.Error(), "not this branch's container") {
		t.Fatalf("ApplyReconcile = %v, want the name conflict reported", err)
	}
	if !d.containers["pgoverlay-br-pr-1"] {
		t.Fatal("a container belonging to something else was removed")
	}
}

// The Postgres router's dial-failure path re-reads the address from the
// runtime and records it, rate-limited per branch.
func TestRefreshBranchEndpoint(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	b := readyBranch(t, e, "pr-1")
	d.containerState[b.ContainerID] = runtime.ContainerInfo{Running: true, Host: "127.0.0.1", Port: 40002}

	addr, err := e.RefreshBranchEndpoint(context.Background(), "pr-1")
	if err != nil || addr != "127.0.0.1:40002" {
		t.Fatalf("RefreshBranchEndpoint = %q, %v", addr, err)
	}
	if got := mustBranch(t, r, "pr-1"); got.Port != 40002 {
		t.Fatalf("registry port = %d, want 40002", got.Port)
	}
	// within the rate limit the runtime is not asked again
	inspects := d.inspects
	d.containerState[b.ContainerID] = runtime.ContainerInfo{Running: true, Host: "127.0.0.1", Port: 40003}
	if addr, _ := e.RefreshBranchEndpoint(context.Background(), "pr-1"); addr != "127.0.0.1:40002" || d.inspects != inspects {
		t.Fatalf("second refresh = %q (inspects %d -> %d), want the recorded address without an inspect", addr, inspects, d.inspects)
	}
	if _, err := e.RefreshBranchEndpoint(context.Background(), "nope"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("unknown branch: %v", err)
	}
}

// --- in-flight volumes and volume GC grace (issue #11) ---

// Reconcile running while a freeze is between creating the parent's fresh rw
// volume and CommitFreeze must not GC that volume: no row names it yet.
func TestReconcileMidFreezeKeepsParentsNewRWVolume(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	readyBranch(t, e, "p")
	newRW := "pgoverlay-br-p-rw-g2"

	var mid ReconcilePlan
	ran := false
	d.onStartBranch = func(s runtime.BranchSpec) {
		if s.Name != "pgoverlay-br-p" || ran {
			return
		}
		ran = true // the parent's restart over the frozen chain: newRW exists, unrecorded
		taken, err := e.ApplyReconcile(context.Background(), time.Now(), 10*time.Minute)
		if err != nil {
			t.Errorf("mid-freeze reconcile: %v", err)
		}
		mid = taken
	}
	if _, err := e.CreateBranchFrom(context.Background(), "c", "p", 0); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("hook never ran")
	}
	if hasAction(mid, ActionGCVolume, newRW) {
		t.Fatalf("DATA LOSS: mid-freeze reconcile GC'd the parent's new rw volume: %+v", mid.Actions)
	}
	if !d.volumes[newRW] {
		t.Fatal("DATA LOSS: the parent's new rw volume is gone")
	}
	if got := mustBranch(t, r, "p"); got.RWVolume != newRW || got.State != registry.BranchReady {
		t.Fatalf("parent after freeze: %+v", got)
	}
}

// Reconcile running while a source refresh is seeding the next generation
// must not GC that volume: BumpSourceGeneration has not recorded it yet.
func TestReconcileMidRefreshKeepsNextGenerationVolume(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	next := "pgoverlay-src-main-g2"

	var mid ReconcilePlan
	ran := false
	d.onRunHelper = func(runtime.HelperSpec) {
		if ran || !d.volumes[next] {
			return
		}
		ran = true // seeding into the unrecorded next generation
		taken, err := e.ApplyReconcile(context.Background(), time.Now(), 10*time.Minute)
		if err != nil {
			t.Errorf("mid-refresh reconcile: %v", err)
		}
		mid = taken
	}
	if err := e.RefreshSource(context.Background(), "main", "secret"); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("hook never ran")
	}
	if hasAction(mid, ActionGCVolume, next) || !d.volumes[next] {
		t.Fatalf("DATA LOSS: mid-refresh reconcile GC'd the new generation: %+v", mid.Actions)
	}
	if src := mustSource(t, r); src.Volume != next {
		t.Fatalf("source volume = %q, want %q", src.Volume, next)
	}
}

// An unrecorded volume younger than the stuck timeout may belong to an
// operation in another process; it is only GC'd once it is older.
func TestReconcileVolumeGCGracePeriod(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	now := time.Now()
	d.addOrphanVolume("pgoverlay-br-young-rw", r.InstanceID())
	d.volumeCreated["pgoverlay-br-young-rw"] = now.Add(-time.Minute)
	d.addOrphanVolume("pgoverlay-br-old-rw", r.InstanceID())
	d.volumeCreated["pgoverlay-br-old-rw"] = now.Add(-time.Hour)

	plan, err := e.PlanReconcile(context.Background(), now, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if hasAction(plan, ActionGCVolume, "pgoverlay-br-young-rw") {
		t.Fatalf("volume younger than the stuck timeout planned for GC: %+v", plan.Actions)
	}
	if !hasAction(plan, ActionGCVolume, "pgoverlay-br-old-rw") {
		t.Fatalf("old orphan volume not planned: %+v", plan.Actions)
	}
}

// A volume claimed by an operation in this process is skipped at plan time
// and re-checked at apply time.
func TestReconcileSkipsClaimedVolume(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	d.addOrphanVolume("pgoverlay-br-x-rw-g2", r.InstanceID())

	release := e.claimVolume("pgoverlay-br-x-rw-g2")
	plan, err := e.PlanReconcile(context.Background(), time.Now(), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if hasAction(plan, ActionGCVolume, "pgoverlay-br-x-rw-g2") {
		t.Fatalf("claimed volume planned for GC: %+v", plan.Actions)
	}
	release()

	plan, err = e.PlanReconcile(context.Background(), time.Now(), 10*time.Minute)
	if err != nil || !hasAction(plan, ActionGCVolume, "pgoverlay-br-x-rw-g2") {
		t.Fatalf("released volume not planned: %+v, %v", plan.Actions, err)
	}
	// claimed between plan and apply: the apply-time re-check keeps it
	release = e.claimVolume("pgoverlay-br-x-rw-g2")
	defer release()
	applied, err := e.applyAction(context.Background(), Action{Kind: ActionGCVolume, Target: "pgoverlay-br-x-rw-g2"})
	if err != nil || applied || !d.volumes["pgoverlay-br-x-rw-g2"] {
		t.Fatalf("apply on a claimed volume: applied=%v err=%v present=%v", applied, err, d.volumes["pgoverlay-br-x-rw-g2"])
	}
}
