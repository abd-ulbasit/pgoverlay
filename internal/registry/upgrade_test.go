package registry

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// releasedSchemaVersion is the schema of the last release before the v1
// hardening. v12 (indexes) .. v15 (audit names, tombstone passwords) were all
// added on top of it and must apply to an existing registry in order.
const releasedSchemaVersion = 11

// TestMigrateReleasedToLatest upgrades a registry written by the last release
// (with rows in it) through every later migration and checks each one took:
// v12's indexes, v13's sources.image, v14's pending_volume claims, and v15's
// entity_name column and index, tombstone clear and trigger.
func TestMigrateReleasedToLatest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "released.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:releasedSchemaVersion] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version=%d`, releasedSchemaVersion)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sources (id,name,pg_version,volume,state) VALUES ('s1','main','17','pgoverlay-src-main','ready');
		INSERT INTO branches (id,name,source_id,state,rw_volume,source_volume,password) VALUES
		  ('b-live','pr-1','s1','ready','pgoverlay-br-pr-1-rw','pgoverlay-src-main','livepassword0001'),
		  ('b-dead','old','s1','destroyed','pgoverlay-br-old-rw','pgoverlay-src-main','enc:v1:c2VjcmV0');
		INSERT INTO transitions (entity,entity_id,from_state,to_state,reason,actor) VALUES
		  ('branch','b-live','','creating','created','root'),
		  ('branch','b-live','creating','ready','instance running','root');`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	r, err := Open(path)
	if err != nil {
		t.Fatalf("open a v%d registry: %v", releasedSchemaVersion, err)
	}
	defer r.Close()
	if v := userVersion(t, path); v != currentSchemaVersion() {
		t.Fatalf("user_version=%d want %d", v, currentSchemaVersion())
	}
	// v13: sources.image, empty (the default image) for existing sources
	s, err := r.GetSourceByName("main")
	if err != nil || s.Image != "" || s.Volume != "pgoverlay-src-main" {
		t.Fatalf("source after upgrade: %+v, %v", s, err)
	}
	// v14: both claim columns exist, and a claim counts as live
	if err := r.SetSourcePendingVolume("s1", "pgoverlay-src-main-g2"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetBranchPendingVolume("b-live", "pgoverlay-br-pr-1-rw-g2"); err != nil {
		t.Fatal(err)
	}
	if live, err := r.LiveVolumeSet(); err != nil || !live["pgoverlay-src-main-g2"] || !live["pgoverlay-br-pr-1-rw"] {
		t.Fatalf("live set after upgrade: %v, %v", live, err)
	}
	// v15: tombstone password cleared, live one kept, history by name works
	if raw := rawBranchPassword(t, r, "b-dead"); raw != "" {
		t.Fatalf("destroyed row kept its password: %q", raw)
	}
	if raw := rawBranchPassword(t, r, "b-live"); raw != "livepassword0001" {
		t.Fatalf("live row password changed: %q", raw)
	}
	if h, err := r.BranchHistory("pr-1"); err != nil || len(h) != 2 {
		t.Fatalf("history after upgrade: %v, %v", h, err)
	}
	for _, obj := range []struct{ typ, name string }{
		{"index", "transitions_entity"}, {"index", "branches_name"}, {"index", "branches_state_updated"},
		{"index", "layers_volume"}, {"index", "branches_pending_volume"}, {"index", "transitions_entity_name"},
		{"trigger", "branches_destroyed_forget_password"},
	} {
		var n int
		if err := r.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type=? AND name=?`, obj.typ, obj.name).Scan(&n); err != nil || n != 1 {
			t.Errorf("%s %s missing after upgrade (%v)", obj.typ, obj.name, err)
		}
	}
}

// BranchHistory must stay index lookups for both of its matches: by the
// branch rows' ids (v12) and by the entity_name stamped on removed branches'
// rows (v15). One OR across the two made SQLite scan every branch transition.
func TestBranchHistoryUsesIndexes(t *testing.T) {
	r := openTest(t)
	rows, err := r.db.Query(`EXPLAIN QUERY PLAN `+branchHistoryQuery, "x", "x")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan, "\n")
	for _, want := range []string{"transitions_entity (entity=? AND entity_id=?)", "transitions_entity_name (entity=? AND entity_name=?)"} {
		if !strings.Contains(joined, want) {
			t.Errorf("plan does not use %s:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "SCAN t\n") || strings.HasSuffix(joined, "SCAN t") || strings.Contains(joined, "SCAN transitions") {
		t.Errorf("plan scans transitions:\n%s", joined)
	}
}
