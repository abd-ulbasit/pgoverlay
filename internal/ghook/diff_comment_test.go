package ghook

import (
	"strings"
	"testing"

	"github.com/abd-ulbasit/pgoverlay/internal/engine"
)

// The PR diff comment names tables outside public by schema, so same-named
// tables stay distinguishable, and never prints an unknown count as a number.
func TestDiffCommentBodySchemaQualifiedAndUnknown(t *testing.T) {
	body := diffCommentBody("gh-pr-7", &engine.DiffResult{Tables: []engine.TableDelta{
		{Schema: "app", Table: "orders", BaseRows: 10, BranchRows: 12, Delta: 2},
		{Schema: "public", Table: "orders", BaseRows: 5, BranchRows: 4, Delta: -1},
		{Schema: "public", Table: "big", BaseRows: engine.UnknownRows, BranchRows: 7, RowsUnknown: true},
	}})
	for _, want := range []string{"| `app.orders` | 10 | 12 | +2 |", "| `orders` | 5 | 4 | -1 |"} {
		if !strings.Contains(body, want) {
			t.Errorf("comment missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "big") {
		t.Errorf("a table with an unknown count was listed as changed:\n%s", body)
	}
}
