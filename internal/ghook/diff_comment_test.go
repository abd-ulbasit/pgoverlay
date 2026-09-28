package ghook

import (
	"strings"
	"testing"

	"github.com/abd-ulbasit/pgoverlay/internal/engine"
)

// The PR diff comment names tables outside public by schema, so same-named
// tables stay distinguishable, and never prints an unknown count as a number.
// A table with an unknown count has no delta to list, so a footer counts it
// instead: the comment must not read as if nothing changed.
func TestDiffCommentBodySchemaQualifiedAndUnknown(t *testing.T) {
	body := diffCommentBody("gh-pr-7", &engine.DiffResult{Tables: []engine.TableDelta{
		{Schema: "app", Table: "orders", BaseRows: 10, BranchRows: 12, Delta: 2},
		{Schema: "public", Table: "orders", BaseRows: 5, BranchRows: 4, Delta: -1},
		{Schema: "public", Table: "big", BaseRows: engine.UnknownRows, BranchRows: 7, RowsUnknown: true},
	}})
	for _, want := range []string{
		"| `app.orders` | 10 | 12 | +2 |",
		"| `orders` | 5 | 4 | -1 |",
		"_(1 table with an unknown row count is not listed: never analyzed and too large to count exactly. `pgb diff --all` lists it.)_",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("comment missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "big") {
		t.Errorf("a table with an unknown count was listed as changed:\n%s", body)
	}
}

// With no known change, the comment only claims "no row-count changes" when
// no count is unknown either.
func TestDiffCommentBodyNoKnownChanges(t *testing.T) {
	unknown := engine.TableDelta{Table: "big", BaseRows: engine.UnknownRows, BranchRows: engine.UnknownRows, RowsUnknown: true}
	same := engine.TableDelta{Table: "users", BaseRows: 3, BranchRows: 3}

	body := diffCommentBody("gh-pr-7", &engine.DiffResult{Tables: []engine.TableDelta{unknown, same, unknown}})
	for _, want := range []string{"Tables: no known row-count changes.", "_(2 tables with an unknown row count are not listed:"} {
		if !strings.Contains(body, want) {
			t.Errorf("comment missing %q:\n%s", want, body)
		}
	}

	body = diffCommentBody("gh-pr-7", &engine.DiffResult{Tables: []engine.TableDelta{same}})
	if !strings.Contains(body, "Tables: no row-count changes.") || strings.Contains(body, "unknown") {
		t.Errorf("comment for a diff with no changes and no unknown counts:\n%s", body)
	}
}
