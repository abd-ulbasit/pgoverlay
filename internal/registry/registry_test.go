package registry

import (
	"path/filepath"
	"testing"
)

func openTest(t *testing.T) *Registry {
	t.Helper()
	r, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func TestSourceLifecycle(t *testing.T) {
	r := openTest(t)
	s := &Source{
		Name: "main", PGVersion: "17", Volume: "pgbranch-src-main",
		ConnHost: "db.example.com", ConnPort: 5432, ConnUser: "postgres", ConnDB: "app",
		Network: "",
	}
	if err := r.CreateSource(s); err != nil {
		t.Fatal(err)
	}
	if s.ID == "" || s.State != SourceSeeding {
		t.Fatalf("ID=%q State=%q", s.ID, s.State)
	}
	if err := r.SetSourceState(s.ID, SourceReady, "seed complete"); err != nil {
		t.Fatal(err)
	}
	got, err := r.GetSourceByName("main")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != SourceReady || got.Volume != "pgbranch-src-main" {
		t.Fatalf("got %+v", got)
	}
	// duplicate name rejected
	if err := r.CreateSource(&Source{Name: "main", PGVersion: "17", Volume: "x"}); err == nil {
		t.Fatal("want duplicate-name error")
	}
	list, err := r.ListSources()
	if err != nil || len(list) != 1 {
		t.Fatalf("list=%v err=%v", list, err)
	}
}

func TestBranchLifecycleAndTransitions(t *testing.T) {
	r := openTest(t)
	s := &Source{Name: "main", PGVersion: "17", Volume: "v"}
	if err := r.CreateSource(s); err != nil {
		t.Fatal(err)
	}
	b := &Branch{Name: "pr-1", SourceID: s.ID, RWVolume: "pgbranch-br-pr-1-rw"}
	if err := r.CreateBranch(b); err != nil {
		t.Fatal(err)
	}
	if b.State != BranchCreating {
		t.Fatalf("state=%q", b.State)
	}
	// illegal: creating -> destroyed
	if err := r.TransitionBranch(b.ID, BranchDestroyed, ""); err == nil {
		t.Fatal("want illegal transition error")
	}
	if err := r.MarkBranchReady(b.ID, "cid123", 54321); err != nil {
		t.Fatal(err)
	}
	got, _ := r.GetBranchByName("pr-1")
	if got.State != BranchReady || got.ContainerID != "cid123" || got.Port != 54321 {
		t.Fatalf("got %+v", got)
	}
	if err := r.TransitionBranch(b.ID, BranchDestroying, "user destroy"); err != nil {
		t.Fatal(err)
	}
	if err := r.TransitionBranch(b.ID, BranchDestroyed, ""); err != nil {
		t.Fatal(err)
	}
	// name reusable after destroy
	if err := r.CreateBranch(&Branch{Name: "pr-1", SourceID: s.ID, RWVolume: "v2"}); err != nil {
		t.Fatalf("name not reusable: %v", err)
	}
	// live list excludes destroyed
	live, err := r.ListLiveBranches()
	if err != nil || len(live) != 1 {
		t.Fatalf("live=%v err=%v", live, err)
	}
}
