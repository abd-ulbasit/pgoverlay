package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/pgbranch/internal/registry"
	"github.com/abd-ulbasit/pgbranch/internal/runtime"
)

// Unit tests for branch-from-branch (overlay backend): the freeze saga
// ordering (checkpoint -> stop -> swap -> restart parent -> start child),
// the exact chain -> PGBRANCH_LOWERS/mounts construction, failure unwinding
// that restores the parent, layer GC refcounts, and reset-to-own-base.

func lowersEnv(t *testing.T, s runtime.BranchSpec) string {
	t.Helper()
	for _, e := range s.Env {
		if v, ok := strings.CutPrefix(e, "PGBRANCH_LOWERS="); ok {
			return v
		}
	}
	t.Fatalf("spec %q has no PGBRANCH_LOWERS env: %v", s.Name, s.Env)
	return ""
}

func mountAt(t *testing.T, s runtime.BranchSpec, target string) runtime.Mount {
	t.Helper()
	for _, m := range s.Mounts {
		if m.Target == target {
			return m
		}
	}
	t.Fatalf("spec %q has no mount at %s: %+v", s.Name, target, s.Mounts)
	return runtime.Mount{}
}

// assertOverlaySpec checks a branch container spec against the expected
// overlay stack: rw volume at /pgbranch/rw, source ro at lower0, frozen layer
// volumes (newest first) ro at lower1..N, and PGBRANCH_LOWERS listing the
// layers' upper/ subdirs newest-first with the source's data/ last.
func assertOverlaySpec(t *testing.T, s runtime.BranchSpec, rwVol, srcVol string, layerVols []string) {
	t.Helper()
	if got := mountAt(t, s, "/pgbranch/rw"); got.Volume != rwVol || got.ReadOnly {
		t.Errorf("spec %q rw mount = %+v, want rw volume %q", s.Name, got, rwVol)
	}
	if got := mountAt(t, s, "/pgbranch/lower0"); got.Volume != srcVol || !got.ReadOnly {
		t.Errorf("spec %q lower0 mount = %+v, want ro source %q", s.Name, got, srcVol)
	}
	wantLowers := make([]string, 0, len(layerVols)+1)
	for i, lv := range layerVols {
		target := []string{"", "/pgbranch/lower1", "/pgbranch/lower2", "/pgbranch/lower3"}[i+1]
		if got := mountAt(t, s, target); got.Volume != lv || !got.ReadOnly {
			t.Errorf("spec %q %s mount = %+v, want ro layer %q", s.Name, target, got, lv)
		}
		wantLowers = append(wantLowers, target+"/upper")
	}
	wantLowers = append(wantLowers, "/pgbranch/lower0/data")
	if got, want := lowersEnv(t, s), strings.Join(wantLowers, ":"); got != want {
		t.Errorf("spec %q PGBRANCH_LOWERS = %q, want %q", s.Name, got, want)
	}
	if got := len(s.Mounts); got != len(layerVols)+2 {
		t.Errorf("spec %q has %d mounts, want %d: %+v", s.Name, got, len(layerVols)+2, s.Mounts)
	}
}

// indexAfter finds the first log entry containing substr at position > after.
func indexAfter(log []string, substr string, after int) int {
	for i := after + 1; i < len(log); i++ {
		if strings.Contains(log[i], substr) {
			return i
		}
	}
	return -1
}

func TestCreateBranchFromFreezeHappyPath(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	d.volumes["pgbranch-src-main"] = true
	if _, err := e.CreateBranch(context.Background(), "p", "main", 0); err != nil {
		t.Fatal(err)
	}

	c, err := e.CreateBranchFrom(context.Background(), "c", "p", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if c.State != registry.BranchReady || c.ParentBranchName != "p" || c.BaseLayerID == "" {
		t.Fatalf("child %+v", c)
	}
	if c.SourceVolume != "pgbranch-src-main" || c.RWVolume != "pgbranch-br-c-rw" {
		t.Fatalf("child volumes: %+v", c)
	}
	if c.ExpiresAt == "" {
		t.Fatal("ttl not recorded on child")
	}
	// parent: frozen rw became a layer; fresh rw volume; ready again
	p, err := r.GetBranchByName("p")
	if err != nil {
		t.Fatal(err)
	}
	if p.State != registry.BranchReady || p.RWVolume != "pgbranch-br-p-rw-g2" || p.BaseLayerID != c.BaseLayerID {
		t.Fatalf("parent after freeze: %+v", p)
	}
	chain, err := r.LayerChain(c.ID)
	if err != nil || len(chain) != 1 || chain[0].Volume != "pgbranch-br-p-rw" {
		t.Fatalf("child chain=%v err=%v want [pgbranch-br-p-rw]", chain, err)
	}
	// the freeze is journaled with the freeze reason on the parent
	if _, err := r.GetBranchByName("p"); err != nil {
		t.Fatal(err)
	}

	// exact saga ordering: CHECKPOINT on the parent -> stop parent ->
	// fresh parent rw volume -> restart parent -> start child
	ci := d.logIndex("exec:psql:cid-pgbranch-br-p")
	si := indexAfter(d.log, "stop:cid-pgbranch-br-p", ci)
	vi := indexAfter(d.log, "volume:pgbranch-br-p-rw-g2", si)
	pi := indexAfter(d.log, "start:pgbranch-br-p", vi)
	chi := indexAfter(d.log, "start:pgbranch-br-c", pi)
	if ci < 0 || si < 0 || vi < 0 || pi < 0 || chi < 0 {
		t.Fatalf("saga order checkpoint(%d) -> stop(%d) -> swap(%d) -> restart parent(%d) -> start child(%d) violated:\n%s",
			ci, si, vi, pi, chi, strings.Join(d.log, "\n"))
	}
	// the checkpoint is a psql CHECKPOINT inside the parent container
	var checkpointed bool
	for _, c := range d.psqlExecs() {
		if c[len(c)-1] == "CHECKPOINT" {
			checkpointed = true
		}
	}
	if !checkpointed {
		t.Fatalf("no psql CHECKPOINT exec: %v", d.execs)
	}

	// container specs: parent restart and child both run on
	// [frozen parent rw, source], rw volumes their own
	if len(d.branches) != 3 {
		t.Fatalf("StartBranch calls = %d want 3 (create p, restart p, start c)", len(d.branches))
	}
	assertOverlaySpec(t, d.branches[0], "pgbranch-br-p-rw", "pgbranch-src-main", nil)
	assertOverlaySpec(t, d.branches[1], "pgbranch-br-p-rw-g2", "pgbranch-src-main", []string{"pgbranch-br-p-rw"})
	assertOverlaySpec(t, d.branches[2], "pgbranch-br-c-rw", "pgbranch-src-main", []string{"pgbranch-br-p-rw"})

	// volumes: source + frozen layer + parent's fresh rw + child rw
	for _, v := range []string{"pgbranch-src-main", "pgbranch-br-p-rw", "pgbranch-br-p-rw-g2", "pgbranch-br-c-rw"} {
		if !d.volumes[v] {
			t.Errorf("volume %q missing after freeze: %v", v, d.volumes)
		}
	}
	if len(d.volumes) != 4 {
		t.Errorf("unexpected volumes: %v", d.volumes)
	}
}

func TestCreateBranchFromGrandchildChains(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "p", "main", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateBranchFrom(context.Background(), "c1", "p", 0); err != nil {
		t.Fatal(err)
	}
	// grandchild: freeze c1; its chain must stack [frozen c1 rw, frozen p rw]
	c2, err := e.CreateBranchFrom(context.Background(), "c2", "c1", 0)
	if err != nil {
		t.Fatal(err)
	}
	chain, err := r.LayerChain(c2.ID)
	if err != nil || len(chain) != 2 {
		t.Fatalf("c2 chain=%v err=%v", chain, err)
	}
	if chain[0].Volume != "pgbranch-br-c1-rw" || chain[1].Volume != "pgbranch-br-p-rw" {
		t.Fatalf("c2 chain order (newest first): %v", chain)
	}
	last := d.branches[len(d.branches)-1]
	assertOverlaySpec(t, last, "pgbranch-br-c2-rw", "pgbranch-src-main", []string{"pgbranch-br-c1-rw", "pgbranch-br-p-rw"})

	// second freeze of p: the new layer (p's g2 rw) chains onto the first
	c3, err := e.CreateBranchFrom(context.Background(), "c3", "p", 0)
	if err != nil {
		t.Fatal(err)
	}
	chain, err = r.LayerChain(c3.ID)
	if err != nil || len(chain) != 2 || chain[0].Volume != "pgbranch-br-p-rw-g2" || chain[1].Volume != "pgbranch-br-p-rw" {
		t.Fatalf("c3 chain=%v err=%v", chain, err)
	}
	p, _ := r.GetBranchByName("p")
	if p.RWVolume != "pgbranch-br-p-rw-g3" {
		t.Fatalf("parent rw after second freeze: %q", p.RWVolume)
	}
	last = d.branches[len(d.branches)-1]
	assertOverlaySpec(t, last, "pgbranch-br-c3-rw", "pgbranch-src-main", []string{"pgbranch-br-p-rw-g2", "pgbranch-br-p-rw"})
}

func TestCreateBranchFromValidation(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)

	// unknown parent
	if _, err := e.CreateBranchFrom(context.Background(), "c", "ghost", 0); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("unknown parent err=%v want ErrNotFound", err)
	}
	// invalid child name
	if _, err := e.CreateBranchFrom(context.Background(), "BAD NAME", "p", 0); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("invalid name err=%v want ErrInvalidName", err)
	}
	// parent not ready
	d.failStart = true
	e.CreateBranch(context.Background(), "p", "main", 0) // -> failed
	d.failStart = false
	_, err := e.CreateBranchFrom(context.Background(), "c", "p", 0)
	if err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("non-ready parent err=%v want 'not ready'", err)
	}
	if live, _ := r.ListLiveBranches(); len(live) != 1 { // only the failed p
		t.Fatalf("validation failures left rows: %v", live)
	}
}

func TestCreateBranchFromCheckpointFailureAbortsCleanly(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "p", "main", 0); err != nil {
		t.Fatal(err)
	}
	d.psqlErr = errors.New("could not checkpoint")

	if _, err := e.CreateBranchFrom(context.Background(), "c", "p", 0); err == nil {
		t.Fatal("want error")
	}
	// parent untouched: still ready on its original rw volume and container
	p, _ := r.GetBranchByName("p")
	if p.State != registry.BranchReady || p.RWVolume != "pgbranch-br-p-rw" || p.BaseLayerID != "" {
		t.Fatalf("parent after aborted freeze: %+v", p)
	}
	if !d.containers["cid-pgbranch-br-p"] {
		t.Fatal("parent container was stopped despite checkpoint failure")
	}
	if d.volumes["pgbranch-br-p-rw-g2"] || d.volumes["pgbranch-br-c-rw"] {
		t.Fatalf("aborted freeze leaked volumes: %v", d.volumes)
	}
	c, _ := r.GetBranchByName("c")
	if c.State != registry.BranchFailed {
		t.Fatalf("child state=%q want failed", c.State)
	}
	if chain, _ := r.LayerChain(p.ID); len(chain) != 0 {
		t.Fatalf("aborted freeze committed a layer: %v", chain)
	}
}

// assertParentRestored: parent ready on its ORIGINAL rw volume and chain,
// container running, no layer rows, no leaked freeze volumes, child failed.
func assertParentRestored(t *testing.T, d *fakeDriver, r *registry.Registry) {
	t.Helper()
	p, err := r.GetBranchByName("p")
	if err != nil {
		t.Fatal(err)
	}
	if p.State != registry.BranchReady || p.RWVolume != "pgbranch-br-p-rw" || p.BaseLayerID != "" {
		t.Fatalf("parent not restored: %+v", p)
	}
	if !d.containers["cid-pgbranch-br-p"] {
		t.Fatal("parent container not restarted")
	}
	// the restore restart runs the original stack: own rw, no layers
	last := d.branches[len(d.branches)-1]
	if last.Name != "pgbranch-br-p" {
		t.Fatalf("last started container = %q, want the restored parent", last.Name)
	}
	assertOverlaySpec(t, last, "pgbranch-br-p-rw", "pgbranch-src-main", nil)
	if d.volumes["pgbranch-br-p-rw-g2"] || d.volumes["pgbranch-br-c-rw"] {
		t.Fatalf("failed freeze leaked volumes: %v", d.volumes)
	}
	if !d.volumes["pgbranch-br-p-rw"] {
		t.Fatal("parent rw volume lost in failed freeze")
	}
	c, err := r.GetBranchByName("c")
	if err != nil {
		t.Fatal(err)
	}
	if c.State != registry.BranchFailed {
		t.Fatalf("child state=%q want failed", c.State)
	}
	if chain, _ := r.LayerChain(p.ID); len(chain) != 0 {
		t.Fatalf("failed freeze committed a layer: %v", chain)
	}
}

func TestCreateBranchFromParentRestartFailureRestoresParent(t *testing.T) {
	d := newFake()
	// attempts: 1 = create p, 2 = restart p on frozen chain (fails),
	// 3 = restore p on original chain (succeeds)
	d.failStartAt = map[int]bool{2: true}
	e, r := testEngine(t, d)
	readySource(t, r)
	d.volumes["pgbranch-src-main"] = true
	if _, err := e.CreateBranch(context.Background(), "p", "main", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateBranchFrom(context.Background(), "c", "p", 0); err == nil {
		t.Fatal("want error")
	}
	assertParentRestored(t, d, r)
}

func TestCreateBranchFromChildStartFailureRestoresParent(t *testing.T) {
	d := newFake()
	// attempts: 1 = create p, 2 = restart p (ok), 3 = start child (fails),
	// 4 = restore p (ok)
	d.failStartAt = map[int]bool{3: true}
	e, r := testEngine(t, d)
	readySource(t, r)
	d.volumes["pgbranch-src-main"] = true
	if _, err := e.CreateBranch(context.Background(), "p", "main", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateBranchFrom(context.Background(), "c", "p", 0); err == nil {
		t.Fatal("want error")
	}
	assertParentRestored(t, d, r)
}

func TestCreateBranchFromRestoreFailureMarksParentFailed(t *testing.T) {
	d := newFake()
	// both the frozen-chain restart and the restoration restart fail
	d.failStartAt = map[int]bool{2: true, 3: true}
	e, r := testEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "p", "main", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateBranchFrom(context.Background(), "c", "p", 0); err == nil {
		t.Fatal("want error")
	}
	p, _ := r.GetBranchByName("p")
	if p.State != registry.BranchFailed {
		t.Fatalf("parent state=%q want failed (never half-frozen)", p.State)
	}
	// the parent's data (its original rw volume) must never be deleted
	if !d.volumes["pgbranch-br-p-rw"] {
		t.Fatal("parent rw volume deleted during failed freeze")
	}
	if d.volumes["pgbranch-br-p-rw-g2"] {
		t.Fatalf("fresh rw volume leaked: %v", d.volumes)
	}
}

func TestDestroyBranchGCsLayersUpChain(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	src := readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "p", "main", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateBranchFrom(context.Background(), "c1", "p", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateBranchFrom(context.Background(), "c2", "c1", 0); err != nil {
		t.Fatal(err)
	}
	// layers: L1 = pgbranch-br-p-rw (base of p, c1, c2), L2 = pgbranch-br-c1-rw (base of c1, c2)

	// destroy the parent FIRST: children keep every layer alive
	if err := e.DestroyBranch(context.Background(), "p"); err != nil {
		t.Fatal(err)
	}
	if d.volumes["pgbranch-br-p-rw-g2"] {
		t.Fatal("destroyed parent's own rw volume must go")
	}
	if !d.volumes["pgbranch-br-p-rw"] || !d.volumes["pgbranch-br-c1-rw"] {
		t.Fatalf("layers GC'd while still referenced: %v", d.volumes)
	}

	// destroy the grandchild: c1 still references both layers
	if err := e.DestroyBranch(context.Background(), "c2"); err != nil {
		t.Fatal(err)
	}
	if !d.volumes["pgbranch-br-p-rw"] || !d.volumes["pgbranch-br-c1-rw"] {
		t.Fatalf("layers GC'd while c1 references them: %v", d.volumes)
	}

	// destroy the last referencing branch: the whole chain is GC'd
	if err := e.DestroyBranch(context.Background(), "c1"); err != nil {
		t.Fatal(err)
	}
	if d.volumes["pgbranch-br-p-rw"] || d.volumes["pgbranch-br-c1-rw"] {
		t.Fatalf("zero-ref layers not GC'd: %v", d.volumes)
	}
	if layers, err := r.ListLayersBySource(src.ID); err != nil || len(layers) != 0 {
		t.Fatalf("layer rows after GC: %v err=%v", layers, err)
	}
	// nothing left but the registry history
	if len(d.containers) != 0 {
		t.Fatalf("containers leaked: %v", d.containers)
	}
}

func TestResetBranchUsesOwnBaseChain(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "p", "main", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateBranchFrom(context.Background(), "c", "p", 0); err != nil {
		t.Fatal(err)
	}

	// resetting the child re-clones from ITS base chain (the frozen layer),
	// not from the bare source
	if _, err := e.ResetBranch(context.Background(), "c"); err != nil {
		t.Fatal(err)
	}
	last := d.branches[len(d.branches)-1]
	if last.Name != "pgbranch-br-c" {
		t.Fatalf("last start = %q", last.Name)
	}
	assertOverlaySpec(t, last, "pgbranch-br-c-rw", "pgbranch-src-main", []string{"pgbranch-br-p-rw"})

	// resetting the frozen parent re-clones onto its own base chain too,
	// keeping its post-freeze rw volume name
	if _, err := e.ResetBranch(context.Background(), "p"); err != nil {
		t.Fatal(err)
	}
	last = d.branches[len(d.branches)-1]
	if last.Name != "pgbranch-br-p" {
		t.Fatalf("last start = %q", last.Name)
	}
	assertOverlaySpec(t, last, "pgbranch-br-p-rw-g2", "pgbranch-src-main", []string{"pgbranch-br-p-rw"})
	if b, _ := r.GetBranchByName("p"); b.State != registry.BranchReady {
		t.Fatalf("parent after reset: %+v", b)
	}
}

func TestRemoveSourceGCsOrphanLayers(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	src := readySource(t, r)
	d.volumes["pgbranch-src-main"] = true
	// an orphaned zero-ref layer (e.g. an earlier best-effort GC failed)
	l := &registry.Layer{SourceID: src.ID, Volume: "pgbranch-br-old-rw"}
	if err := r.CreateLayer(l); err != nil {
		t.Fatal(err)
	}
	d.volumes["pgbranch-br-old-rw"] = true

	if err := e.RemoveSource(context.Background(), "main"); err != nil {
		t.Fatal(err)
	}
	if d.volumes["pgbranch-br-old-rw"] || d.volumes["pgbranch-src-main"] {
		t.Fatalf("RemoveSource left volumes: %v", d.volumes)
	}
	if _, err := r.GetSourceByName("main"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("source survives: %v", err)
	}
}
