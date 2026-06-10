package registry

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
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
	if err := r.MarkBranchReady(b.ID, "cid123", "10.1.2.3", 54321); err != nil {
		t.Fatal(err)
	}
	got, _ := r.GetBranchByName("pr-1")
	if got.State != BranchReady || got.ContainerID != "cid123" || got.Host != "10.1.2.3" || got.Port != 54321 {
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

// schemaV1Fixture is the Phase 1 schema exactly as shipped, used to build a
// pre-migration database file for upgrade tests.
const schemaV1Fixture = `
CREATE TABLE IF NOT EXISTS sources (
  id TEXT PRIMARY KEY,
  name TEXT UNIQUE NOT NULL,
  pg_version TEXT NOT NULL,
  volume TEXT NOT NULL,
  conn_host TEXT NOT NULL DEFAULT '',
  conn_port INTEGER NOT NULL DEFAULT 0,
  conn_user TEXT NOT NULL DEFAULT '',
  conn_db   TEXT NOT NULL DEFAULT '',
  network   TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE TABLE IF NOT EXISTS branches (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  source_id TEXT NOT NULL REFERENCES sources(id),
  state TEXT NOT NULL,
  container_id TEXT NOT NULL DEFAULT '',
  rw_volume TEXT NOT NULL,
  port INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE UNIQUE INDEX IF NOT EXISTS branches_live_name
  ON branches(name) WHERE state != 'destroyed';
CREATE TABLE IF NOT EXISTS transitions (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  entity TEXT NOT NULL,
  entity_id TEXT NOT NULL,
  from_state TEXT NOT NULL,
  to_state TEXT NOT NULL,
  reason TEXT NOT NULL DEFAULT '',
  at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
`

func TestMigrateV1ToLatest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schemaV1Fixture); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sources (id,name,pg_version,volume,state) VALUES ('src1','main','17','pgbranch-src-main','ready')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO branches (id,name,source_id,state,container_id,rw_volume,port)
		VALUES ('br1','pr-1','src1','ready','cid1','pgbranch-br-pr-1-rw',54321)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })

	var v int
	if err := r.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 4 {
		t.Fatalf("user_version=%d want 4", v)
	}
	s, err := r.GetSourceByName("main")
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != "src1" || s.Volume != "pgbranch-src-main" || s.State != SourceReady {
		t.Fatalf("source row not preserved: %+v", s)
	}
	if s.Generation != 1 {
		t.Fatalf("Generation=%d want 1", s.Generation)
	}
	b, err := r.GetBranchByName("pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if b.ID != "br1" || b.ContainerID != "cid1" || b.Port != 54321 || b.State != BranchReady {
		t.Fatalf("branch row not preserved: %+v", b)
	}
	if b.SourceVolume != "pgbranch-src-main" {
		t.Fatalf("SourceVolume=%q want backfill from source", b.SourceVolume)
	}
	if b.ExpiresAt != "" {
		t.Fatalf("ExpiresAt=%q want empty", b.ExpiresAt)
	}
	// v3: host column backfilled with the docker default
	if b.Host != "127.0.0.1" {
		t.Fatalf("Host=%q want default backfill 127.0.0.1", b.Host)
	}
	// v4: mask_scripts table exists and is usable on the upgraded DB
	if err := r.SetMaskScripts("src1", []MaskScript{{Name: "m.sql", SQL: "SELECT 1"}}); err != nil {
		t.Fatalf("v4 mask_scripts unusable after upgrade: %v", err)
	}
	if ms, err := r.GetMaskScripts("src1"); err != nil || len(ms) != 1 {
		t.Fatalf("mask scripts after upgrade: %v err=%v", ms, err)
	}
	// re-opening an already-migrated DB is a no-op
	r2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	r2.Close()
}

func TestMaskScriptsSetGetReplace(t *testing.T) {
	r := openTest(t)
	s := &Source{Name: "main", PGVersion: "17", Volume: "v"}
	if err := r.CreateSource(s); err != nil {
		t.Fatal(err)
	}
	// empty before any set
	if ms, err := r.GetMaskScripts(s.ID); err != nil || len(ms) != 0 {
		t.Fatalf("fresh source mask scripts: %v err=%v", ms, err)
	}
	// set preserves order
	in := []MaskScript{
		{Name: "b.sql", SQL: "UPDATE t SET x=1"},
		{Name: "a.sql", SQL: "UPDATE t SET y=2"},
	}
	if err := r.SetMaskScripts(s.ID, in); err != nil {
		t.Fatal(err)
	}
	got, err := r.GetMaskScripts(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != in[0] || got[1] != in[1] {
		t.Fatalf("got %v want %v (order must be preserved)", got, in)
	}
	// set replaces all previous scripts
	if err := r.SetMaskScripts(s.ID, []MaskScript{{Name: "only.sql", SQL: "DELETE FROM t"}}); err != nil {
		t.Fatal(err)
	}
	got, err = r.GetMaskScripts(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "only.sql" {
		t.Fatalf("replace: got %v", got)
	}
	// empty set clears
	if err := r.SetMaskScripts(s.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got, err = r.GetMaskScripts(s.ID); err != nil || len(got) != 0 {
		t.Fatalf("clear: got %v err=%v", got, err)
	}
}

func TestSourceNameReusableAfterFailure(t *testing.T) {
	r := openTest(t)
	s1 := &Source{Name: "main", PGVersion: "17", Volume: "v1"}
	if err := r.CreateSource(s1); err != nil {
		t.Fatal(err)
	}
	if err := r.SetSourceState(s1.ID, SourceFailed, "seed failed"); err != nil {
		t.Fatal(err)
	}
	// failed row must not block re-creating the source under the same name
	s2 := &Source{Name: "main", PGVersion: "17", Volume: "v2"}
	if err := r.CreateSource(s2); err != nil {
		t.Fatalf("recreate after failure: %v", err)
	}
	// but a live row still blocks duplicates
	if err := r.CreateSource(&Source{Name: "main", PGVersion: "17", Volume: "v3"}); err == nil {
		t.Fatal("want duplicate-name error while a live source exists")
	}
}

func TestListExpiredBranches(t *testing.T) {
	r := openTest(t)
	s := &Source{Name: "main", PGVersion: "17", Volume: "v"}
	if err := r.CreateSource(s); err != nil {
		t.Fatal(err)
	}
	mk := func(name, expiresAt string) *Branch {
		t.Helper()
		b := &Branch{Name: name, SourceID: s.ID, RWVolume: name + "-rw", SourceVolume: "v", ExpiresAt: expiresAt}
		if err := r.CreateBranch(b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	expired := mk("old", "2026-01-01T00:00:00Z")
	if err := r.MarkBranchReady(expired.ID, "c1", "127.0.0.1", 1); err != nil {
		t.Fatal(err)
	}
	forever := mk("forever", "")
	if err := r.MarkBranchReady(forever.ID, "c2", "127.0.0.1", 2); err != nil {
		t.Fatal(err)
	}
	future := mk("future", "2027-01-01T00:00:00Z")
	if err := r.MarkBranchReady(future.ID, "c3", "127.0.0.1", 3); err != nil {
		t.Fatal(err)
	}
	mk("stuck-creating", "2026-01-01T00:00:00Z") // creating: not reaped

	got, err := r.ListExpiredBranches("2026-06-10T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "old" {
		t.Fatalf("expired=%v", got)
	}
	// failed branches with a past expiry are reaped too
	if err := r.TransitionBranch(future.ID, BranchDestroying, ""); err != nil {
		t.Fatal(err)
	}
	if err := r.TransitionBranch(future.ID, BranchDestroyed, ""); err != nil {
		t.Fatal(err)
	}
	got, err = r.ListExpiredBranches("2028-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "old" {
		t.Fatalf("expired=%v (destroyed/creating/never must be excluded)", got)
	}
}

func TestBumpSourceGenerationAndCounts(t *testing.T) {
	r := openTest(t)
	s := &Source{Name: "main", PGVersion: "17", Volume: "pgbranch-src-main"}
	if err := r.CreateSource(s); err != nil {
		t.Fatal(err)
	}
	if s2, _ := r.GetSourceByName("main"); s2.Generation != 1 {
		t.Fatalf("fresh source generation=%d want 1", s2.Generation)
	}
	b := &Branch{Name: "pr-1", SourceID: s.ID, RWVolume: "rw", SourceVolume: "pgbranch-src-main"}
	if err := r.CreateBranch(b); err != nil {
		t.Fatal(err)
	}
	if err := r.BumpSourceGeneration(s.ID, "pgbranch-src-main-g2"); err != nil {
		t.Fatal(err)
	}
	s2, err := r.GetSourceByName("main")
	if err != nil {
		t.Fatal(err)
	}
	if s2.Generation != 2 || s2.Volume != "pgbranch-src-main-g2" {
		t.Fatalf("after bump: %+v", s2)
	}
	if n, _ := r.CountLiveBranchesBySource(s.ID); n != 1 {
		t.Fatalf("live by source=%d want 1", n)
	}
	if n, _ := r.CountLiveBranchesByVolume("pgbranch-src-main"); n != 1 {
		t.Fatalf("live by volume=%d want 1", n)
	}
	if n, _ := r.CountLiveBranchesByVolume("pgbranch-src-main-g2"); n != 0 {
		t.Fatalf("live by g2 volume=%d want 0", n)
	}
	// destroyed branches don't count
	if err := r.MarkBranchReady(b.ID, "c", "127.0.0.1", 1); err != nil {
		t.Fatal(err)
	}
	if err := r.TransitionBranch(b.ID, BranchDestroying, ""); err != nil {
		t.Fatal(err)
	}
	if err := r.TransitionBranch(b.ID, BranchDestroyed, ""); err != nil {
		t.Fatal(err)
	}
	if n, _ := r.CountLiveBranchesBySource(s.ID); n != 0 {
		t.Fatalf("live by source after destroy=%d want 0", n)
	}
	if n, _ := r.CountLiveBranchesByVolume("pgbranch-src-main"); n != 0 {
		t.Fatalf("live by volume after destroy=%d want 0", n)
	}
}

func TestDeleteSource(t *testing.T) {
	r := openTest(t)
	s := &Source{Name: "main", PGVersion: "17", Volume: "v"}
	if err := r.CreateSource(s); err != nil {
		t.Fatal(err)
	}
	b := &Branch{Name: "pr-1", SourceID: s.ID, RWVolume: "rw", SourceVolume: "v"}
	if err := r.CreateBranch(b); err != nil {
		t.Fatal(err)
	}
	if err := r.TransitionBranch(b.ID, BranchFailed, "x"); err != nil {
		t.Fatal(err)
	}
	if err := r.TransitionBranch(b.ID, BranchDestroying, ""); err != nil {
		t.Fatal(err)
	}
	if err := r.TransitionBranch(b.ID, BranchDestroyed, ""); err != nil {
		t.Fatal(err)
	}
	if err := r.SetMaskScripts(s.ID, []MaskScript{{Name: "m.sql", SQL: "SELECT 1"}}); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteSource(s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetSourceByName("main"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	// mask scripts go with the source
	if ms, err := r.GetMaskScripts(s.ID); err != nil || len(ms) != 0 {
		t.Fatalf("mask scripts orphaned after source delete: %v err=%v", ms, err)
	}
	// name immediately reusable
	if err := r.CreateSource(&Source{Name: "main", PGVersion: "17", Volume: "v2"}); err != nil {
		t.Fatalf("name not reusable after delete: %v", err)
	}
}

func TestResettingTransitions(t *testing.T) {
	r := openTest(t)
	s := &Source{Name: "main", PGVersion: "17", Volume: "v"}
	if err := r.CreateSource(s); err != nil {
		t.Fatal(err)
	}
	b := &Branch{Name: "pr-1", SourceID: s.ID, RWVolume: "rw", SourceVolume: "v"}
	if err := r.CreateBranch(b); err != nil {
		t.Fatal(err)
	}
	// creating -> resetting is illegal
	if err := r.TransitionBranch(b.ID, BranchResetting, ""); err == nil {
		t.Fatal("want illegal transition error from creating")
	}
	if err := r.MarkBranchReady(b.ID, "c", "127.0.0.1", 1); err != nil {
		t.Fatal(err)
	}
	if err := r.TransitionBranch(b.ID, BranchResetting, "reset requested"); err != nil {
		t.Fatal(err)
	}
	// resetting -> ready (via MarkBranchReady, new container/port)
	if err := r.MarkBranchReady(b.ID, "c2", "127.0.0.1", 2); err != nil {
		t.Fatal(err)
	}
	got, _ := r.GetBranchByName("pr-1")
	if got.State != BranchReady || got.ContainerID != "c2" || got.Port != 2 {
		t.Fatalf("after reset: %+v", got)
	}
	// resetting -> failed
	if err := r.TransitionBranch(b.ID, BranchResetting, ""); err != nil {
		t.Fatal(err)
	}
	if err := r.TransitionBranch(b.ID, BranchFailed, "boom"); err != nil {
		t.Fatal(err)
	}
}

// pg_version must be a supported major (14-18) or empty (defaults to the
// engine's image, postgres:17). Minors and out-of-range majors are rejected:
// PG < 14 lacks recovery_init_sync_method=syncfs, which branch startup needs.
func TestCreateSourcePGVersionValidation(t *testing.T) {
	r := openTest(t)
	for i, v := range []string{"", "14", "15", "16", "17", "18"} {
		s := &Source{Name: fmt.Sprintf("ok-%d", i), PGVersion: v, ConnHost: "h", ConnPort: 5432, ConnUser: "u"}
		if err := r.CreateSource(s); err != nil {
			t.Errorf("pg_version %q: unexpected error %v", v, err)
		}
	}
	for _, v := range []string{"13", "19", "17.2", "9.6", "fourteen", " 17"} {
		s := &Source{Name: "bad", PGVersion: v, ConnHost: "h", ConnPort: 5432, ConnUser: "u"}
		err := r.CreateSource(s)
		if !errors.Is(err, ErrUnsupportedPGVersion) {
			t.Errorf("pg_version %q: err = %v, want ErrUnsupportedPGVersion", v, err)
		}
		if err == nil {
			continue
		}
		if !strings.Contains(err.Error(), v) || !strings.Contains(err.Error(), "14") {
			t.Errorf("pg_version %q: error %q should name the version and the supported range", v, err)
		}
	}
}
