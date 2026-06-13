package engine

import (
	"context"
	"testing"
	"time"

	"github.com/abd-ulbasit/pgbranch/internal/registry"
)

// seedReady marks a freshly-created creating branch ready so it counts as live
// with a stable rw volume (no driver provisioning involved).
func markReady(t *testing.T, r *registry.Registry, b *registry.Branch, cid string) {
	t.Helper()
	if err := r.MarkBranchReady(b.ID, cid, "127.0.0.1", 5432); err != nil {
		t.Fatal(err)
	}
}

// TestReconcileFailsStuckCreating: a creating row older than the stuck timeout
// is failed and its rw volume is removed.
func TestReconcileFailsStuckCreating(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	b := &registry.Branch{Name: "stuck", SourceID: mustSource(t, r).ID, RWVolume: "pgbranch-br-stuck-rw"}
	if err := r.CreateBranch(b); err != nil {
		t.Fatal(err)
	}
	d.volumes["pgbranch-br-stuck-rw"] = true

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
	if d.volumes["pgbranch-br-stuck-rw"] {
		t.Fatal("stuck rw volume not removed")
	}
}

// A recently-created creating row (within the timeout) is left alone.
func TestReconcileLeavesFreshCreating(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	b := &registry.Branch{Name: "fresh", SourceID: mustSource(t, r).ID, RWVolume: "pgbranch-br-fresh-rw"}
	if err := r.CreateBranch(b); err != nil {
		t.Fatal(err)
	}
	d.volumes["pgbranch-br-fresh-rw"] = true

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
	b := &registry.Branch{Name: "live", SourceID: mustSource(t, r).ID, RWVolume: "pgbranch-br-live-rw"}
	if err := r.CreateBranch(b); err != nil {
		t.Fatal(err)
	}
	d.volumes["pgbranch-br-live-rw"] = true
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

// A frozen layer with refcount 0 is GC'd (volume + row); a layer still in a
// live branch's chain is kept.
func TestReconcileGCsDanglingLayerKeepsReferenced(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	src := readySource(t, r)

	// dangling layer: refcount 0.
	dangling := &registry.Layer{SourceID: src.ID, Volume: "pgbranch-layer-dangling"}
	if err := r.CreateLayer(dangling); err != nil {
		t.Fatal(err)
	}
	d.volumes["pgbranch-layer-dangling"] = true

	// referenced layer: a live branch chains onto it.
	referenced := &registry.Layer{SourceID: src.ID, Volume: "pgbranch-layer-referenced"}
	if err := r.CreateLayer(referenced); err != nil {
		t.Fatal(err)
	}
	d.volumes["pgbranch-layer-referenced"] = true
	b := &registry.Branch{Name: "child", SourceID: src.ID, RWVolume: "pgbranch-br-child-rw", BaseLayerID: referenced.ID}
	if err := r.CreateBranch(b); err != nil {
		t.Fatal(err)
	}
	d.volumes["pgbranch-br-child-rw"] = true
	markReady(t, r, b, "cid-child")
	d.containers["cid-child"] = true

	taken, err := e.ApplyReconcile(context.Background(), time.Now(), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !hasAction(taken, ActionGCLayer, "pgbranch-layer-dangling") {
		t.Fatalf("dangling layer not GC'd: %+v", taken.Actions)
	}
	if hasAction(taken, ActionGCLayer, "pgbranch-layer-referenced") {
		t.Fatalf("referenced layer GC'd: %+v", taken.Actions)
	}
	if d.volumes["pgbranch-layer-dangling"] {
		t.Fatal("dangling layer volume survived")
	}
	if !d.volumes["pgbranch-layer-referenced"] {
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
	d.volumes["pgbranch-src-main"] = true // source generation: in use

	// in-use rw volume: a live branch owns it.
	b := &registry.Branch{Name: "live", SourceID: mustSource(t, r).ID, RWVolume: "pgbranch-br-live-rw"}
	if err := r.CreateBranch(b); err != nil {
		t.Fatal(err)
	}
	d.volumes["pgbranch-br-live-rw"] = true
	markReady(t, r, b, "cid-live")
	d.containers["cid-live"] = true

	// orphan volume: no branch, no source, no layer references it.
	d.addOrphanVolume("pgbranch-br-orphan-rw", r.InstanceID())

	taken, err := e.ApplyReconcile(context.Background(), time.Now(), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !hasAction(taken, ActionGCVolume, "pgbranch-br-orphan-rw") {
		t.Fatalf("orphan volume not GC'd: %+v", taken.Actions)
	}
	if d.volumes["pgbranch-br-orphan-rw"] {
		t.Fatal("orphan volume survived")
	}
	if !d.volumes["pgbranch-br-live-rw"] {
		t.Fatal("in-use rw volume removed")
	}
	if !d.volumes["pgbranch-src-main"] {
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
	b := &registry.Branch{Name: "stuck", SourceID: mustSource(t, r).ID, RWVolume: "pgbranch-br-stuck-rw"}
	if err := r.CreateBranch(b); err != nil {
		t.Fatal(err)
	}
	d.volumes["pgbranch-br-stuck-rw"] = true
	// orphan container + orphan volume.
	d.addOrphanContainer("cid-ghost", r.InstanceID())
	d.addOrphanVolume("pgbranch-br-orphan-rw", r.InstanceID())

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
		!hasAction(plan, ActionGCVolume, "pgbranch-br-orphan-rw") {
		t.Fatalf("plan missing expected actions: %+v", plan.Actions)
	}
	// nothing mutated: driver log unchanged, registry row still creating,
	// resources still present.
	if len(d.log) != before {
		t.Fatalf("dry-run mutated the driver: %v", d.log[before:])
	}
	if !d.containers["cid-ghost"] || !d.volumes["pgbranch-br-orphan-rw"] || !d.volumes["pgbranch-br-stuck-rw"] {
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
// and volume tagged with a different instance id (a sibling pgbranch sharing
// the daemon) untouched. This is the regression guard for the CI bug where one
// IT package's reconcile deleted another package's live resources.
func TestReconcileIgnoresForeignInstanceResources(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	const foreign = "ffffffffffffffff" // some other registry's instance id

	// our own orphans (tagged with this registry's instance id) -> reclaimed.
	d.addOrphanContainer("cid-mine", r.InstanceID())
	d.addOrphanVolume("pgbranch-br-mine-rw", r.InstanceID())
	// a sibling instance's live resources -> must be left alone.
	d.addOrphanContainer("cid-foreign", foreign)
	d.addOrphanVolume("pgbranch-br-foreign-rw", foreign)

	taken, err := e.ApplyReconcile(context.Background(), time.Now(), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// own resources reclaimed.
	if !hasAction(taken, ActionRemoveOrphanContainer, "cid-mine") {
		t.Fatalf("own orphan container not reclaimed: %+v", taken.Actions)
	}
	if !hasAction(taken, ActionGCVolume, "pgbranch-br-mine-rw") {
		t.Fatalf("own orphan volume not reclaimed: %+v", taken.Actions)
	}
	if d.containers["cid-mine"] || d.volumes["pgbranch-br-mine-rw"] {
		t.Fatal("own orphans survived reconcile")
	}
	// foreign resources untouched in plan and in the driver.
	if hasAction(taken, ActionRemoveOrphanContainer, "cid-foreign") {
		t.Fatalf("foreign container reclaimed: %+v", taken.Actions)
	}
	if hasAction(taken, ActionGCVolume, "pgbranch-br-foreign-rw") {
		t.Fatalf("foreign volume reclaimed: %+v", taken.Actions)
	}
	if !d.containers["cid-foreign"] {
		t.Fatal("foreign instance's container was removed")
	}
	if !d.volumes["pgbranch-br-foreign-rw"] {
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
