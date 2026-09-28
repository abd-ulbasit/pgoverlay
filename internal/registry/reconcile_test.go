package registry

import (
	"context"
	"testing"
	"time"
)

// reconcileBranch creates a source and one branch in the given state (via
// legal transitions) and returns the branch.
func reconcileBranch(t *testing.T, r *Registry, state BranchState) *Branch {
	t.Helper()
	s := &Source{Name: "main", PGVersion: "17", Volume: "pgoverlay-src-main"}
	if err := r.CreateSource(s); err != nil {
		t.Fatal(err)
	}
	b := &Branch{Name: "pr-1", SourceID: s.ID, RWVolume: "pgoverlay-br-pr-1-rw"}
	if err := r.CreateBranch(b); err != nil {
		t.Fatal(err)
	}
	if state == BranchCreating {
		return b
	}
	if err := r.MarkBranchReady(b.ID, "c1", "127.0.0.1", 40001); err != nil {
		t.Fatal(err)
	}
	if state != BranchReady {
		if err := r.TransitionBranch(b.ID, state, "test"); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

// TimeString must order exactly like the updated_at column it is compared
// with (strftime('%Y-%m-%dT%H:%M:%fZ')), including within the same second.
func TestTimeStringMatchesColumnFormat(t *testing.T) {
	r := openTest(t)
	b := reconcileBranch(t, r, BranchCreating)
	var updated string
	if err := r.db.QueryRow(`SELECT updated_at FROM branches WHERE id=?`, b.ID).Scan(&updated); err != nil {
		t.Fatal(err)
	}
	if len(updated) != len(TimeString(time.Now())) {
		t.Fatalf("updated_at %q and TimeString %q differ in shape", updated, TimeString(time.Now()))
	}
	if !(TimeString(time.Now().Add(-time.Minute)) < updated && updated < TimeString(time.Now().Add(time.Minute))) {
		t.Fatalf("updated_at %q does not order between TimeString(now±1m)", updated)
	}
}

func TestUpdateBranchEndpointIsCompareAndSwap(t *testing.T) {
	r := openTest(t)
	b := reconcileBranch(t, r, BranchReady)
	ok, err := r.UpdateBranchEndpoint(b.ID, "c1", "c2", "10.0.0.2", 5432)
	if err != nil || !ok {
		t.Fatalf("update = %v, %v", ok, err)
	}
	got, _ := r.GetBranchByName("pr-1")
	if got.ContainerID != "c2" || got.Host != "10.0.0.2" || got.Port != 5432 || got.State != BranchReady {
		t.Fatalf("row = %+v", got)
	}
	// stale expected container: no change
	if ok, err := r.UpdateBranchEndpoint(b.ID, "c1", "c3", "10.0.0.3", 5432); err != nil || ok {
		t.Fatalf("stale container id updated the row: %v, %v", ok, err)
	}
	// not ready: no change
	if err := r.TransitionBranch(b.ID, BranchResetting, "reset"); err != nil {
		t.Fatal(err)
	}
	if ok, err := r.UpdateBranchEndpoint(b.ID, "c2", "c4", "10.0.0.4", 5432); err != nil || ok {
		t.Fatalf("resetting row updated: %v, %v", ok, err)
	}
}

// FailReadyBranch fails a ready branch in one transaction through the legal
// edges, journaling both, and only while the expected container is recorded.
func TestFailReadyBranch(t *testing.T) {
	r := openTest(t)
	b := reconcileBranch(t, r, BranchReady)
	if ok, err := r.FailReadyBranch(context.Background(), b.ID, "other", "x"); err != nil || ok {
		t.Fatalf("wrong container failed the row: %v, %v", ok, err)
	}
	ok, err := r.FailReadyBranch(context.Background(), b.ID, "c1", "restart failed")
	if err != nil || !ok {
		t.Fatalf("FailReadyBranch = %v, %v", ok, err)
	}
	got, _ := r.GetBranchByName("pr-1")
	if got.State != BranchFailed {
		t.Fatalf("state = %q", got.State)
	}
	hist, err := r.BranchHistory("pr-1")
	if err != nil {
		t.Fatal(err)
	}
	n := len(hist)
	if n < 2 || hist[n-2].FromState != "ready" || hist[n-2].ToState != "resetting" ||
		hist[n-1].FromState != "resetting" || hist[n-1].ToState != "failed" || hist[n-1].Reason != "restart failed" {
		t.Fatalf("history tail = %+v", hist[n-2:])
	}
	if ok, err := r.FailReadyBranch(context.Background(), b.ID, "c1", "again"); err != nil || ok {
		t.Fatalf("failed row failed again: %v, %v", ok, err)
	}
}

// FailStuckBranch applies only to a creating/resetting row not updated since
// the cut-off.
func TestFailStuckBranch(t *testing.T) {
	r := openTest(t)
	b := reconcileBranch(t, r, BranchCreating)
	past := TimeString(time.Now().Add(-time.Hour))
	future := TimeString(time.Now().Add(time.Hour))
	if ok, err := r.FailStuckBranch(context.Background(), b.ID, past, "x"); err != nil || ok {
		t.Fatalf("row updated after the cut-off was failed: %v, %v", ok, err)
	}
	if ok, err := r.FailStuckBranch(context.Background(), b.ID, future, "stuck"); err != nil || !ok {
		t.Fatalf("stuck row not failed: %v, %v", ok, err)
	}
	if got, _ := r.GetBranchByName("pr-1"); got.State != BranchFailed {
		t.Fatalf("state = %q", got.State)
	}
	// not transient any more: no-op
	if ok, err := r.FailStuckBranch(context.Background(), b.ID, future, "again"); err != nil || ok {
		t.Fatalf("failed row failed again: %v, %v", ok, err)
	}

	ready := openTest(t)
	rb := reconcileBranch(t, ready, BranchReady)
	if ok, err := ready.FailStuckBranch(context.Background(), rb.ID, future, "x"); err != nil || ok {
		t.Fatalf("ready row failed as stuck: %v, %v", ok, err)
	}
}

func TestListStuckDestroyingBranches(t *testing.T) {
	r := openTest(t)
	reconcileBranch(t, r, BranchDestroying)
	if got, err := r.ListStuckDestroyingBranches(TimeString(time.Now().Add(-time.Hour))); err != nil || len(got) != 0 {
		t.Fatalf("fresh destroying row listed: %v, %v", got, err)
	}
	if got, err := r.ListStuckDestroyingBranches(TimeString(time.Now().Add(time.Hour))); err != nil || len(got) != 1 || got[0].Name != "pr-1" {
		t.Fatalf("stuck destroying row not listed: %v, %v", got, err)
	}
}

// A seeding source whose heartbeat stopped is listed and can be failed once;
// TouchSource (the heartbeat) keeps it off the list, and a ready source is
// never touched.
func TestStuckSources(t *testing.T) {
	r := openTest(t)
	s := &Source{Name: "main", PGVersion: "17", Volume: "pgoverlay-src-main"}
	if err := r.CreateSource(s); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	cut := TimeString(time.Now())
	if got, err := r.ListStuckSources(cut); err != nil || len(got) != 1 || got[0].ID != s.ID {
		t.Fatalf("stuck seeding source not listed: %v, %v", got, err)
	}
	if err := r.TouchSource(s.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.ListStuckSources(cut); len(got) != 0 {
		t.Fatalf("heartbeat did not refresh the source: %v", got)
	}
	if ok, err := r.FailStuckSource(context.Background(), s.ID, cut, "x"); err != nil || ok {
		t.Fatalf("source that heartbeat after the cut-off was failed: %v, %v", ok, err)
	}
	future := TimeString(time.Now().Add(time.Hour))
	if ok, err := r.FailStuckSource(context.Background(), s.ID, future, "no progress"); err != nil || !ok {
		t.Fatalf("FailStuckSource = %v, %v", ok, err)
	}
	if got, _ := r.GetSourceByID(s.ID); got.State != SourceFailed {
		t.Fatalf("state = %q", got.State)
	}
	if ok, err := r.FailStuckSource(context.Background(), s.ID, future, "again"); err != nil || ok {
		t.Fatalf("failed source failed again: %v, %v", ok, err)
	}
}
