package registry

import "testing"

// updated_at is stored with milliseconds; a whole-second RFC3339 cutoff used
// to compare as later than every timestamp inside its second, so a row
// touched 500ms after the cutoff was reported as stuck.
func TestStuckCutoffComparesMilliseconds(t *testing.T) {
	r := openTest(t)
	s := mkSource(t, r, "main")
	b := mkBranch(t, r, &Branch{Name: "pr-1", SourceID: s.ID})
	if _, err := r.db.Exec(`UPDATE branches SET updated_at='2030-01-01T00:00:00.500Z' WHERE id=?`, b.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := r.ListStuckBranches("2030-01-01T00:00:00Z"); err != nil || len(got) != 0 {
		t.Fatalf("row touched after the cutoff listed as stuck: %v err=%v", got, err)
	}
	if got, _ := r.ListStuckBranches("2030-01-01T00:00:01Z"); len(got) != 1 {
		t.Fatalf("row touched before the cutoff not listed: %v", got)
	}
	if got := msCutoff("not a time"); got != "not a time" {
		t.Fatalf("msCutoff passthrough = %q", got)
	}
	if got := msCutoff("2030-01-01T02:00:00+02:00"); got != "2030-01-01T00:00:00.000Z" {
		t.Fatalf("msCutoff = %q", got)
	}
}
