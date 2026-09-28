package registry

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tests for the branch/source state machines, the atomic write paths and the
// lifecycle helpers (destroy retry, recovery guards, volume-name reuse).

func mkSource(t *testing.T, r *Registry, name string) *Source {
	t.Helper()
	s := &Source{Name: name, PGVersion: "17", Volume: "pgoverlay-src-" + name}
	if err := r.CreateSource(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func mkBranch(t *testing.T, r *Registry, b *Branch) *Branch {
	t.Helper()
	if b.RWVolume == "" {
		b.RWVolume = "pgoverlay-br-" + b.Name + "-rw"
	}
	if err := r.CreateBranch(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// history returns the (from, to, reason) triples journaled for a branch id.
func history(t *testing.T, r *Registry, id string) [][3]string {
	t.Helper()
	rows, err := r.db.Query(`SELECT from_state, to_state, reason FROM transitions WHERE entity='branch' AND entity_id=? ORDER BY id`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out [][3]string
	for rows.Next() {
		var h [3]string
		if err := rows.Scan(&h[0], &h[1], &h[2]); err != nil {
			t.Fatal(err)
		}
		out = append(out, h)
	}
	return out
}

func TestBranchStateMachine(t *testing.T) {
	legal := map[[2]BranchState]bool{
		{BranchCreating, BranchReady}:       true,
		{BranchCreating, BranchFailed}:      true,
		{BranchReady, BranchResetting}:      true,
		{BranchReady, BranchDestroying}:     true,
		{BranchResetting, BranchReady}:      true,
		{BranchResetting, BranchFailed}:     true,
		{BranchFailed, BranchResetting}:     true, // reset / recover a failed branch
		{BranchFailed, BranchDestroying}:    true,
		{BranchDestroying, BranchDestroyed}: true,
	}
	all := []BranchState{BranchCreating, BranchReady, BranchFailed, BranchResetting, BranchDestroying, BranchDestroyed}
	for _, from := range all {
		for _, to := range all {
			if got, want := legalBranchTransition(from, to), legal[[2]BranchState{from, to}]; got != want {
				t.Errorf("%s -> %s legal=%v want %v", from, to, got, want)
			}
		}
	}
}

func TestFailedBranchCanBeReset(t *testing.T) {
	r := openTest(t)
	s := mkSource(t, r, "main")
	b := mkBranch(t, r, &Branch{Name: "pr-1", SourceID: s.ID})
	if err := r.TransitionBranch(b.ID, BranchFailed, "boom"); err != nil {
		t.Fatal(err)
	}
	if err := r.TransitionBranch(b.ID, BranchResetting, "recover requested"); err != nil {
		t.Fatalf("failed -> resetting: %v", err)
	}
	if err := r.MarkBranchReady(b.ID, "cid", "127.0.0.1", 5432); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.GetBranchByName("pr-1"); got.State != BranchReady {
		t.Fatalf("state=%s want ready", got.State)
	}
}

func TestIllegalTransitionErrorMatchesSentinel(t *testing.T) {
	r := openTest(t)
	s := mkSource(t, r, "main")
	b := mkBranch(t, r, &Branch{Name: "pr-1", SourceID: s.ID})
	err := r.TransitionBranch(b.ID, BranchDestroyed, "")
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("err=%v, want ErrIllegalTransition", err)
	}
	if want := "illegal branch transition creating -> destroyed"; err.Error() != want {
		t.Fatalf("err=%q want %q (the API maps on this text)", err, want)
	}
}

// MarkBranchReady folds the container/host/port write into the state
// compare-and-swap: a branch that was failed meanwhile keeps its columns.
func TestMarkBranchReadyLeavesColumnsWhenCASLoses(t *testing.T) {
	r := openTest(t)
	s := mkSource(t, r, "main")
	b := mkBranch(t, r, &Branch{Name: "pr-1", SourceID: s.ID})
	if err := r.SetBranchContainer(b.ID, "cid-old"); err != nil {
		t.Fatal(err)
	}
	if err := r.TransitionBranch(b.ID, BranchFailed, "reconcile: stuck creating"); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkBranchReady(b.ID, "cid-new", "10.0.0.9", 9999); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("MarkBranchReady on failed row: err=%v, want illegal transition", err)
	}
	got, _ := r.GetBranchByName("pr-1")
	if got.ContainerID != "cid-old" || got.Port != 0 || got.State != BranchFailed {
		t.Fatalf("row after losing CAS: %+v (columns must be untouched)", got)
	}
}

func TestCreateBranchDuplicateNameNamesHolder(t *testing.T) {
	r := openTest(t)
	s := mkSource(t, r, "main")
	b := mkBranch(t, r, &Branch{Name: "pr-9", SourceID: s.ID})
	if err := r.TransitionBranch(b.ID, BranchFailed, "boom"); err != nil {
		t.Fatal(err)
	}
	err := r.CreateBranch(&Branch{Name: "pr-9", SourceID: s.ID, RWVolume: "x"})
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("err=%v want ErrAlreadyExists", err)
	}
	for _, want := range []string{`"pr-9"`, "state failed", "destroy it first"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q does not mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "UNIQUE") {
		t.Errorf("err %q leaks the raw SQLite constraint", err)
	}
	// the refused insert journaled nothing
	var n int
	if err := r.db.QueryRow(`SELECT count(*) FROM transitions WHERE entity='branch'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 { // created + failed of the first row only
		t.Fatalf("transitions=%d want 2", n)
	}
}

func TestGetByNameErrorsNameTheSubject(t *testing.T) {
	r := openTest(t)
	_, err := r.GetBranchByName("nope")
	if !errors.Is(err, ErrNotFound) || err.Error() != `branch "nope" not found` {
		t.Fatalf("GetBranchByName: %v", err)
	}
	_, err = r.GetSourceByName("gone")
	if !errors.Is(err, ErrNotFound) || err.Error() != `source "gone" not found` {
		t.Fatalf("GetSourceByName: %v", err)
	}
}

func TestSourceStateMachine(t *testing.T) {
	r := openTest(t)
	s := mkSource(t, r, "main")
	if err := r.SetSourceState(s.ID, "bogus", "x"); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("seeding -> bogus: err=%v", err)
	}
	if err := r.SetSourceState(s.ID, SourceReady, "seed complete"); err != nil {
		t.Fatal(err)
	}
	for _, to := range []SourceState{SourceSeeding, SourceFailed, SourceReady} {
		if err := r.SetSourceState(s.ID, to, "x"); !errors.Is(err, ErrIllegalTransition) {
			t.Errorf("ready -> %s: err=%v, want illegal", to, err)
		}
	}
	if got, _ := r.GetSourceByName("main"); got.State != SourceReady {
		t.Fatalf("state=%s", got.State)
	}
	// exactly one journal row per accepted change: created, seeding->ready
	var n int
	if err := r.db.QueryRow(`SELECT count(*) FROM transitions WHERE entity='source' AND entity_id=?`, s.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("source transitions=%d want 2", n)
	}
	if err := r.SetSourceState("missing", SourceReady, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
}

// A retry of a failed `source add` replaces the failed attempt instead of
// piling up same-named failed rows (only the latest attempt is listed).
func TestCreateSourceReplacesFailedAttempts(t *testing.T) {
	r := openTest(t)
	for i := 0; i < 3; i++ {
		s := mkSource(t, r, "again")
		if err := r.SetMaskScripts(s.ID, []MaskScript{{Name: "m", SQL: "SELECT 1"}}); err != nil {
			t.Fatal(err)
		}
		if err := r.SetSourceState(s.ID, SourceFailed, "seed failed"); err != nil {
			t.Fatal(err)
		}
	}
	list, err := r.ListSources()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].State != SourceFailed {
		t.Fatalf("sources after 3 failed attempts: %+v, want one failed row", list)
	}
	var orphanMasks int
	if err := r.db.QueryRow(`SELECT count(*) FROM mask_scripts WHERE source_id NOT IN (SELECT id FROM sources)`).Scan(&orphanMasks); err != nil {
		t.Fatal(err)
	}
	if orphanMasks != 0 {
		t.Fatalf("%d mask scripts outlived their failed source rows", orphanMasks)
	}
	ok := mkSource(t, r, "again")
	if err := r.SetSourceState(ok.ID, SourceReady, "seed complete"); err != nil {
		t.Fatal(err)
	}
	if list, _ = r.ListSources(); len(list) != 1 || list[0].ID != ok.ID {
		t.Fatalf("sources after a successful retry: %+v, want only the ready row", list)
	}
	// a live row still refuses a duplicate, naming its state
	err = r.CreateSource(&Source{Name: "again", PGVersion: "17", Volume: "v"})
	if !errors.Is(err, ErrAlreadyExists) || !strings.Contains(err.Error(), "state ready") {
		t.Fatalf("duplicate live source: %v", err)
	}
}

func TestDeleteFailedSources(t *testing.T) {
	r := openTest(t)
	s := mkSource(t, r, "x")
	if err := r.SetSourceState(s.ID, SourceFailed, "boom"); err != nil {
		t.Fatal(err)
	}
	live := mkSource(t, r, "y")
	n, err := r.DeleteFailedSources("x")
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if n, _ := r.DeleteFailedSources("y"); n != 0 {
		t.Fatalf("deleted %d live rows", n)
	}
	if _, err := r.GetSourceByID(live.ID); err != nil {
		t.Fatal(err)
	}
}

func TestNoteBranchJournalsWithoutStateChange(t *testing.T) {
	r := openTest(t)
	s := mkSource(t, r, "main")
	b := mkBranch(t, r, &Branch{Name: "pr-1", SourceID: s.ID})
	if err := r.TransitionBranch(b.ID, BranchFailed, "x"); err != nil {
		t.Fatal(err)
	}
	if err := r.TransitionBranch(b.ID, BranchDestroying, "destroy requested"); err != nil {
		t.Fatal(err)
	}
	// make the row old, then note a failed teardown attempt
	if _, err := r.db.Exec(`UPDATE branches SET updated_at='2000-01-01T00:00:00Z' WHERE id=?`, b.ID); err != nil {
		t.Fatal(err)
	}
	before := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	if got, _ := r.ListDestroyingBranches(before); len(got) != 1 || got[0].ID != b.ID {
		t.Fatalf("ListDestroyingBranches before note = %v, want [pr-1]", got)
	}
	if err := r.NoteBranchCtx(context.Background(), b.ID, "destroy failed: volume is in use"); err != nil {
		t.Fatal(err)
	}
	h := history(t, r, b.ID)
	last := h[len(h)-1]
	if last != [3]string{"destroying", "destroying", "destroy failed: volume is in use"} {
		t.Fatalf("last journal row = %v", last)
	}
	got, _ := r.GetBranchByName("pr-1")
	if got.State != BranchDestroying {
		t.Fatalf("state=%s", got.State)
	}
	// the note bumped updated_at: no longer older than the cutoff
	if got, _ := r.ListDestroyingBranches(before); len(got) != 0 {
		t.Fatalf("ListDestroyingBranches after note = %v, want none (backoff)", got)
	}
	if err := r.NoteBranchCtx(context.Background(), "missing", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
}

func TestGetBranchByIDIncludesTombstones(t *testing.T) {
	r := openTest(t)
	s := mkSource(t, r, "main")
	b := mkBranch(t, r, &Branch{Name: "pr-1", SourceID: s.ID})
	for _, to := range []BranchState{BranchFailed, BranchDestroying, BranchDestroyed} {
		if err := r.TransitionBranch(b.ID, to, ""); err != nil {
			t.Fatal(err)
		}
	}
	got, err := r.GetBranchByID(b.ID)
	if err != nil || got.State != BranchDestroyed {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

// VolumeNameUsed covers every place a volume name can live, destroyed rows
// included: a recreated branch must never reuse any of them.
func TestVolumeNameUsed(t *testing.T) {
	r := openTest(t)
	s := mkSource(t, r, "main")
	gone := mkBranch(t, r, &Branch{Name: "gone", SourceID: s.ID, SourceVolume: s.Volume})
	for _, to := range []BranchState{BranchFailed, BranchDestroying, BranchDestroyed} {
		if err := r.TransitionBranch(gone.ID, to, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.CreateLayer(&Layer{SourceID: s.ID, Volume: "pgoverlay-br-p-rw"}); err != nil {
		t.Fatal(err)
	}
	for vol, want := range map[string]bool{
		"pgoverlay-br-gone-rw": true, // destroyed tombstone's rw volume
		"pgoverlay-src-main":   true, // source volume
		"pgoverlay-br-p-rw":    true, // frozen layer
		"pgoverlay-br-new-rw":  false,
	} {
		got, err := r.VolumeNameUsed(vol)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("VolumeNameUsed(%q)=%v want %v", vol, got, want)
		}
	}
}

// CountLiveBranchesReferencingRW is precise in every state: an uncommitted
// overlay freeze child (no base layer yet) and a clone child (source_volume)
// count; a committed overlay child (bases on a frozen layer) does not.
func TestCountLiveBranchesReferencingRWIgnoresCommittedChildren(t *testing.T) {
	r := openTest(t)
	s := mkSource(t, r, "main")
	p := mkBranch(t, r, &Branch{Name: "p", SourceID: s.ID, SourceVolume: s.Volume})
	committed := mkBranch(t, r, &Branch{Name: "c1", SourceID: s.ID, SourceVolume: s.Volume, ParentBranchName: "p"})
	l := &Layer{SourceID: s.ID, Volume: "pgoverlay-br-p-rw"}
	if err := r.CreateLayer(l); err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.Exec(`UPDATE branches SET base_layer_id=?, rw_volume='pgoverlay-br-p-rw-g2' WHERE id=?`, l.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.Exec(`UPDATE branches SET base_layer_id=? WHERE id=?`, l.ID, committed.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkBranchReady(committed.ID, "cid-c1", "127.0.0.1", 1); err != nil {
		t.Fatal(err)
	}
	if n, err := r.CountLiveBranchesReferencingRW("p", "pgoverlay-br-p-rw-g2"); err != nil || n != 0 {
		t.Fatalf("committed child only: n=%d err=%v, want 0", n, err)
	}
	mkBranch(t, r, &Branch{Name: "c2", SourceID: s.ID, SourceVolume: s.Volume, ParentBranchName: "p"}) // in flight
	if n, _ := r.CountLiveBranchesReferencingRW("p", "pgoverlay-br-p-rw-g2"); n != 1 {
		t.Fatalf("in-flight child: n=%d want 1", n)
	}
	mkBranch(t, r, &Branch{Name: "z", SourceID: s.ID, SourceVolume: "pgoverlay-br-p-rw-g2"}) // clone child
	if n, _ := r.CountLiveBranchesReferencingRW("p", "pgoverlay-br-p-rw-g2"); n != 2 {
		t.Fatalf("plus clone child: n=%d want 2", n)
	}
	kids, err := r.InFlightChildren("p")
	if err != nil || len(kids) != 1 || kids[0] != "c2" {
		t.Fatalf("InFlightChildren=%v err=%v want [c2]", kids, err)
	}
}

// Two registry handles on one file stand in for two processes (HA replicas,
// or local pgb next to branchd). With deferred transactions the CAS in
// TransitionBranch reads, loses the snapshot to the other handle's commit,
// and fails at once with SQLITE_BUSY_SNAPSHOT; BEGIN IMMEDIATE (the DSN's
// _txlock) makes it wait for the lock instead.
func TestConcurrentHandlesDoNotFailWithBusySnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.db")
	r1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r1.Close()
	r2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	s := mkSource(t, r1, "main")
	b := mkBranch(t, r1, &Branch{Name: "pr-1", SourceID: s.ID})
	if err := r1.MarkBranchReady(b.ID, "c", "127.0.0.1", 1); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 400)
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			errs <- r1.TransitionBranch(b.ID, BranchResetting, "reset")
			errs <- r1.MarkBranchReady(b.ID, "c", "127.0.0.1", 1)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			errs <- r2.TouchBranch(b.ID)
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent write failed: %v", err)
		}
	}
}
