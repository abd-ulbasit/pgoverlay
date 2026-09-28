package registry

import (
	"strings"
	"testing"
)

// LIFECYCLE-12: the queries run per API call or per reconcile tick must not
// full-scan branches/transitions, which keep every destroyed branch and its
// history forever. EXPLAIN QUERY PLAN reports "SCAN <table>" for a full scan
// and "SEARCH <table> USING INDEX …" for an index lookup.
func TestHotQueriesUseIndexes(t *testing.T) {
	r := openTest(t)
	for _, tc := range []struct {
		name, query string
		args        []any
		tables      []string // tables that must be searched, not scanned
	}{
		{"branch history", `SELECT t.from_state FROM transitions t
			JOIN branches b ON b.id = t.entity_id AND t.entity = 'branch'
			WHERE b.name = ? ORDER BY t.id ASC`, []any{"pr-1"}, []string{"branches", "t"}},
		{"expired branches", `SELECT id FROM branches WHERE state IN ('ready','failed') AND expires_at != '' AND expires_at < ?`,
			[]any{"2030-01-01T00:00:00Z"}, []string{"branches"}},
		{"stuck branches", `SELECT id FROM branches WHERE state IN ('creating','resetting') AND updated_at < ?`,
			[]any{"2030-01-01T00:00:00Z"}, []string{"branches"}},
		{"destroying branches", `SELECT id FROM branches WHERE state='destroying' AND updated_at < ?`,
			[]any{"2030-01-01T00:00:00Z"}, []string{"branches"}},
		{"volume name used", volumeNameUsedQuery, []any{"v"}, []string{"branches", "layers"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := r.db.Query(`EXPLAIN QUERY PLAN `+tc.query, tc.args...)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var plan []string
			for rows.Next() {
				var id, parent, notused int
				var detail string
				if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
					t.Fatal(err)
				}
				plan = append(plan, detail)
			}
			joined := strings.Join(plan, "\n")
			for _, tbl := range tc.tables {
				for _, step := range plan {
					if strings.HasPrefix(step, "SCAN "+tbl+" ") || step == "SCAN "+tbl {
						t.Fatalf("full scan of %s:\n%s", tbl, joined)
					}
				}
			}
		})
	}
}
