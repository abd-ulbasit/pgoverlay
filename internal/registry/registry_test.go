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
