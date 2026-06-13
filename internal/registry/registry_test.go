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
	if v != 9 {
		t.Fatalf("user_version=%d want 9", v)
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
	// v5: pre-existing branches base directly on the source volume (no layer
	// chain) and carry no display parent
	if b.BaseLayerID != "" || b.ParentBranchName != "" {
		t.Fatalf("v5 backfill: BaseLayerID=%q ParentBranchName=%q want empty", b.BaseLayerID, b.ParentBranchName)
	}
	if chain, err := r.LayerChain(b.ID); err != nil || len(chain) != 0 {
		t.Fatalf("v5 chain of pre-existing branch: %v err=%v", chain, err)
	}
	// v5: layers table exists and is usable on the upgraded DB
	if err := r.CreateLayer(&Layer{SourceID: "src1", Volume: "frozen-v"}); err != nil {
		t.Fatalf("v5 layers unusable after upgrade: %v", err)
	}
	// v6: pre-existing sources were seeded with pg_basebackup, whole database
	if s.SeedVia != SeedViaBasebackup {
		t.Fatalf("v6 backfill: SeedVia=%q want %q", s.SeedVia, SeedViaBasebackup)
	}
	if len(s.DumpSchemas) != 0 {
		t.Fatalf("v6 backfill: DumpSchemas=%v want empty", s.DumpSchemas)
	}
	// v7: pre-existing branches inherit credentials (no per-branch password)
	if b.Password != "" {
		t.Fatalf("v7 backfill: Password=%q want empty", b.Password)
	}
	// v9: api_tokens table exists and is usable on the upgraded DB
	if _, err := r.CreateAPIToken("ci", RoleOperator); err != nil {
		t.Fatalf("v9 api_tokens unusable after upgrade: %v", err)
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
	if n, _ := r.CountLiveBranchesByRWVolume("rw"); n != 1 {
		t.Fatalf("live by rw volume=%d want 1", n)
	}
	if n, _ := r.CountLiveBranchesByRWVolume("pgbranch-src-main"); n != 0 {
		t.Fatalf("live by rw volume (source vol)=%d want 0", n)
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

// layerFixture builds source "main" with a two-deep layer chain:
//
//	source volume <- L1 <- L2
//	b-src  bases on the source directly (no chain)
//	b-l1   bases on L1 (chain [L1])
//	b-l2   bases on L2 (chain [L2, L1])
//
// All branches are live (creating).
func layerFixture(t *testing.T, r *Registry) (src *Source, l1, l2 *Layer, bSrc, bL1, bL2 *Branch) {
	t.Helper()
	src = &Source{Name: "main", PGVersion: "17", Volume: "pgbranch-src-main"}
	if err := r.CreateSource(src); err != nil {
		t.Fatal(err)
	}
	l1 = &Layer{SourceID: src.ID, Volume: "pgbranch-br-p-rw"}
	if err := r.CreateLayer(l1); err != nil {
		t.Fatal(err)
	}
	l2 = &Layer{SourceID: src.ID, Volume: "pgbranch-br-p-rw-g2", ParentLayerID: l1.ID}
	if err := r.CreateLayer(l2); err != nil {
		t.Fatal(err)
	}
	mk := func(name, base string) *Branch {
		b := &Branch{Name: name, SourceID: src.ID, RWVolume: name + "-rw", SourceVolume: src.Volume, BaseLayerID: base}
		if err := r.CreateBranch(b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	return src, l1, l2, mk("b-src", ""), mk("b-l1", l1.ID), mk("b-l2", l2.ID)
}

func TestLayerCRUDAndChain(t *testing.T) {
	r := openTest(t)
	_, l1, l2, bSrc, bL1, bL2 := layerFixture(t, r)
	if l1.ID == "" || l2.ID == "" {
		t.Fatalf("CreateLayer assigned no ID: %+v %+v", l1, l2)
	}
	got, err := r.GetLayer(l2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Volume != "pgbranch-br-p-rw-g2" || got.ParentLayerID != l1.ID {
		t.Fatalf("GetLayer: %+v", got)
	}
	if _, err := r.GetLayer("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetLayer(nope) err=%v want ErrNotFound", err)
	}

	// chains, topmost (newest) first
	if chain, err := r.LayerChain(bSrc.ID); err != nil || len(chain) != 0 {
		t.Fatalf("chain(b-src)=%v err=%v want empty", chain, err)
	}
	chain, err := r.LayerChain(bL1.ID)
	if err != nil || len(chain) != 1 || chain[0].ID != l1.ID {
		t.Fatalf("chain(b-l1)=%v err=%v want [L1]", chain, err)
	}
	chain, err = r.LayerChain(bL2.ID)
	if err != nil || len(chain) != 2 || chain[0].ID != l2.ID || chain[1].ID != l1.ID {
		t.Fatalf("chain(b-l2)=%v err=%v want [L2, L1]", chain, err)
	}
	if _, err := r.LayerChain("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LayerChain(nope) err=%v want ErrNotFound", err)
	}

	// delete: children before parents (FK)
	if err := r.DeleteLayer(l1.ID); err == nil {
		t.Fatal("deleting a layer still referenced by a child layer must fail (FK)")
	}
	if err := r.DeleteLayer(l2.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteLayer(l1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetLayer(l1.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("layer survives delete: %v", err)
	}
}

func TestCountBranchesReferencingLayer(t *testing.T) {
	r := openTest(t)
	_, l1, l2, _, bL1, bL2 := layerFixture(t, r)

	// L2 is referenced by b-l2 only; L1 by both b-l1 (directly) and b-l2
	// (through L2's parent chain). b-src references no layer.
	if n, err := r.CountBranchesReferencingLayer(l2.ID); err != nil || n != 1 {
		t.Fatalf("refs(L2)=%d err=%v want 1", n, err)
	}
	if n, err := r.CountBranchesReferencingLayer(l1.ID); err != nil || n != 2 {
		t.Fatalf("refs(L1)=%d err=%v want 2", n, err)
	}
	// destroyed branches do not count
	destroy := func(b *Branch) {
		t.Helper()
		if err := r.TransitionBranch(b.ID, BranchFailed, "x"); err != nil {
			t.Fatal(err)
		}
		if err := r.TransitionBranch(b.ID, BranchDestroying, ""); err != nil {
			t.Fatal(err)
		}
		if err := r.TransitionBranch(b.ID, BranchDestroyed, ""); err != nil {
			t.Fatal(err)
		}
	}
	destroy(bL2)
	if n, _ := r.CountBranchesReferencingLayer(l2.ID); n != 0 {
		t.Fatalf("refs(L2) after destroy=%d want 0", n)
	}
	if n, _ := r.CountBranchesReferencingLayer(l1.ID); n != 1 {
		t.Fatalf("refs(L1) after destroy=%d want 1", n)
	}
	destroy(bL1)
	if n, _ := r.CountBranchesReferencingLayer(l1.ID); n != 0 {
		t.Fatalf("refs(L1) after both destroyed=%d want 0", n)
	}
}

func TestListLayersBySource(t *testing.T) {
	r := openTest(t)
	src, l1, l2, _, _, _ := layerFixture(t, r)
	layers, err := r.ListLayersBySource(src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(layers) != 2 {
		t.Fatalf("layers=%v want 2", layers)
	}
	ids := map[string]bool{layers[0].ID: true, layers[1].ID: true}
	if !ids[l1.ID] || !ids[l2.ID] {
		t.Fatalf("layers=%v want L1+L2", layers)
	}
	if layers, _ := r.ListLayersBySource("nope"); len(layers) != 0 {
		t.Fatalf("layers of unknown source=%v want none", layers)
	}
}

// CommitFreeze is the registry half of the freeze saga: in one transaction
// the parent's old rw volume becomes a new layer, the parent moves to a fresh
// rw volume + new container (resetting -> ready), and the child branch bases
// on the new layer.
func TestCommitFreeze(t *testing.T) {
	r := openTest(t)
	src := &Source{Name: "main", PGVersion: "17", Volume: "pgbranch-src-main"}
	if err := r.CreateSource(src); err != nil {
		t.Fatal(err)
	}
	parent := &Branch{Name: "p", SourceID: src.ID, RWVolume: "pgbranch-br-p-rw", SourceVolume: src.Volume}
	if err := r.CreateBranch(parent); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkBranchReady(parent.ID, "cid-p1", "127.0.0.1", 1001); err != nil {
		t.Fatal(err)
	}
	child := &Branch{Name: "c", SourceID: src.ID, RWVolume: "pgbranch-br-c-rw", SourceVolume: src.Volume, ParentBranchName: "p"}
	if err := r.CreateBranch(child); err != nil {
		t.Fatal(err)
	}

	// freeze requires the parent mid-transition (resetting)
	if _, err := r.CommitFreeze(parent.ID, child.ID, "pgbranch-br-p-rw", "pgbranch-br-p-rw-g2", "cid-p2", "127.0.0.1", 1002, "freeze for child c"); err == nil {
		t.Fatal("CommitFreeze on a ready (not resetting) parent must fail")
	}
	if err := r.TransitionBranch(parent.ID, BranchResetting, "freeze for child c"); err != nil {
		t.Fatal(err)
	}
	l, err := r.CommitFreeze(parent.ID, child.ID, "pgbranch-br-p-rw", "pgbranch-br-p-rw-g2", "cid-p2", "127.0.0.1", 1002, "freeze for child c")
	if err != nil {
		t.Fatal(err)
	}
	if l.ID == "" || l.SourceID != src.ID || l.Volume != "pgbranch-br-p-rw" || l.ParentLayerID != "" {
		t.Fatalf("layer: %+v", l)
	}
	p, err := r.GetBranchByName("p")
	if err != nil {
		t.Fatal(err)
	}
	if p.State != BranchReady || p.RWVolume != "pgbranch-br-p-rw-g2" || p.BaseLayerID != l.ID ||
		p.ContainerID != "cid-p2" || p.Port != 1002 {
		t.Fatalf("parent after freeze: %+v", p)
	}
	c, err := r.GetBranchByName("c")
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseLayerID != l.ID || c.State != BranchCreating {
		t.Fatalf("child after freeze: %+v", c)
	}
	if chain, _ := r.LayerChain(c.ID); len(chain) != 1 || chain[0].ID != l.ID {
		t.Fatalf("child chain: %v", chain)
	}

	// second freeze of the same parent chains the new layer onto the first
	child2 := &Branch{Name: "c2", SourceID: src.ID, RWVolume: "pgbranch-br-c2-rw", SourceVolume: src.Volume, ParentBranchName: "p"}
	if err := r.CreateBranch(child2); err != nil {
		t.Fatal(err)
	}
	if err := r.TransitionBranch(parent.ID, BranchResetting, "freeze for child c2"); err != nil {
		t.Fatal(err)
	}
	l2, err := r.CommitFreeze(parent.ID, child2.ID, "pgbranch-br-p-rw-g2", "pgbranch-br-p-rw-g3", "cid-p3", "127.0.0.1", 1003, "freeze for child c2")
	if err != nil {
		t.Fatal(err)
	}
	if l2.ParentLayerID != l.ID {
		t.Fatalf("second layer parent=%q want %q", l2.ParentLayerID, l.ID)
	}
	if chain, _ := r.LayerChain(child2.ID); len(chain) != 2 || chain[0].ID != l2.ID || chain[1].ID != l.ID {
		t.Fatalf("child2 chain: %v", chain)
	}
	if chain, _ := r.LayerChain(parent.ID); len(chain) != 2 || chain[0].ID != l2.ID {
		t.Fatalf("parent chain after two freezes: %v", chain)
	}
	// the freeze transitions are journaled with the freeze reason
	var n int
	if err := r.db.QueryRow(`SELECT count(*) FROM transitions WHERE entity_id=? AND reason LIKE 'freeze for child%'`, parent.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n < 4 { // 2x ready->resetting + 2x resetting->ready
		t.Fatalf("freeze transitions journaled=%d want >=4", n)
	}
	// unknown child rolls everything back
	if err := r.TransitionBranch(parent.ID, BranchResetting, "freeze for child ghost"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CommitFreeze(parent.ID, "ghost", "x", "y", "cid", "127.0.0.1", 1, "r"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("CommitFreeze(unknown child) err=%v want ErrNotFound", err)
	}
	if p, _ := r.GetBranchByName("p"); p.RWVolume != "pgbranch-br-p-rw-g3" {
		t.Fatalf("failed CommitFreeze mutated parent: %+v", p)
	}
}

// v7: per-branch rotated credentials live on the branch row. Fresh branches
// have no password (inherit mode); SetBranchPassword stores the rotated one
// and it round-trips through every read path.
func TestBranchPasswordRoundTrip(t *testing.T) {
	r := openTest(t)
	src := &Source{Name: "main", PGVersion: "17", Volume: "v"}
	if err := r.CreateSource(src); err != nil {
		t.Fatal(err)
	}
	b := &Branch{Name: "pr-1", SourceID: src.ID, RWVolume: "rw", SourceVolume: "v"}
	if err := r.CreateBranch(b); err != nil {
		t.Fatal(err)
	}
	got, err := r.GetBranchByName("pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Password != "" {
		t.Fatalf("fresh branch Password=%q want empty", got.Password)
	}
	if err := r.SetBranchPassword(b.ID, "a1b2c3d4"); err != nil {
		t.Fatal(err)
	}
	got, err = r.GetBranchByName("pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Password != "a1b2c3d4" {
		t.Fatalf("Password=%q want a1b2c3d4", got.Password)
	}
	// list path carries it too
	live, err := r.ListLiveBranches()
	if err != nil || len(live) != 1 || live[0].Password != "a1b2c3d4" {
		t.Fatalf("list: %v err=%v", live, err)
	}
	// rotation on reset overwrites
	if err := r.SetBranchPassword(b.ID, "ffff0000"); err != nil {
		t.Fatal(err)
	}
	if got, _ = r.GetBranchByName("pr-1"); got.Password != "ffff0000" {
		t.Fatalf("Password=%q want ffff0000 after overwrite", got.Password)
	}
	// unknown branch id
	if err := r.SetBranchPassword("nope", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetBranchPassword(nope) err=%v want ErrNotFound", err)
	}
}

func TestCreateBranchPersistsParentBranchName(t *testing.T) {
	r := openTest(t)
	src := &Source{Name: "main", PGVersion: "17", Volume: "v"}
	if err := r.CreateSource(src); err != nil {
		t.Fatal(err)
	}
	b := &Branch{Name: "c", SourceID: src.ID, RWVolume: "rw", SourceVolume: "v", ParentBranchName: "p"}
	if err := r.CreateBranch(b); err != nil {
		t.Fatal(err)
	}
	got, err := r.GetBranchByName("c")
	if err != nil {
		t.Fatal(err)
	}
	if got.ParentBranchName != "p" {
		t.Fatalf("ParentBranchName=%q want p", got.ParentBranchName)
	}
}

func TestDeleteSourceRemovesLayers(t *testing.T) {
	r := openTest(t)
	src, l1, _, bSrc, bL1, bL2 := layerFixture(t, r)
	for _, b := range []*Branch{bSrc, bL1, bL2} {
		if err := r.TransitionBranch(b.ID, BranchFailed, "x"); err != nil {
			t.Fatal(err)
		}
		if err := r.TransitionBranch(b.ID, BranchDestroying, ""); err != nil {
			t.Fatal(err)
		}
		if err := r.TransitionBranch(b.ID, BranchDestroyed, ""); err != nil {
			t.Fatal(err)
		}
	}
	// layer rows (a self-referencing chain) go with the source
	if err := r.DeleteSource(src.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetLayer(l1.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("layer survives source delete: %v", err)
	}
}

func TestSourceSeedViaAndDumpSchemasRoundTrip(t *testing.T) {
	r := openTest(t)
	// dump-seeded source round-trips method and schema scope
	s := &Source{Name: "dumped", PGVersion: "17", Volume: "v",
		SeedVia: SeedViaDump, DumpSchemas: []string{"public", "audit"}}
	if err := r.CreateSource(s); err != nil {
		t.Fatal(err)
	}
	got, err := r.GetSourceByName("dumped")
	if err != nil {
		t.Fatal(err)
	}
	if got.SeedVia != SeedViaDump {
		t.Fatalf("SeedVia=%q want %q", got.SeedVia, SeedViaDump)
	}
	if len(got.DumpSchemas) != 2 || got.DumpSchemas[0] != "public" || got.DumpSchemas[1] != "audit" {
		t.Fatalf("DumpSchemas=%v want [public audit]", got.DumpSchemas)
	}
	// empty SeedVia defaults to basebackup, empty schemas stay empty
	s2 := &Source{Name: "plain", PGVersion: "17", Volume: "v2"}
	if err := r.CreateSource(s2); err != nil {
		t.Fatal(err)
	}
	got2, err := r.GetSourceByName("plain")
	if err != nil {
		t.Fatal(err)
	}
	if got2.SeedVia != SeedViaBasebackup || len(got2.DumpSchemas) != 0 {
		t.Fatalf("default source: SeedVia=%q DumpSchemas=%v", got2.SeedVia, got2.DumpSchemas)
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

func TestInstanceIDStableAcrossOpens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inst.db")
	r1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id1 := r1.InstanceID()
	if len(id1) != 16 {
		t.Fatalf("instance id %q is not 16 hex chars", id1)
	}
	if err := r1.Close(); err != nil {
		t.Fatal(err)
	}

	r2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r2.Close() })
	if id2 := r2.InstanceID(); id2 != id1 {
		t.Fatalf("instance id changed across opens of the same file: %q != %q", id2, id1)
	}
}

func TestInstanceIDDistinctAcrossFiles(t *testing.T) {
	ra, err := Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ra.Close() })
	rb, err := Open(filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rb.Close() })
	if ra.InstanceID() == rb.InstanceID() {
		t.Fatalf("distinct registry files share an instance id: %q", ra.InstanceID())
	}
}
