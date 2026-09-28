package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/abd-ulbasit/pgoverlay/internal/engine"
)

// renderDiff shows schema-qualified names outside public, "?" for unknown
// counts (never a fake 0), and treats unknown counts as neither grown nor
// sampleable.
func TestRenderDiffSchemasAndUnknownCounts(t *testing.T) {
	res := &engine.DiffResult{Tables: []engine.TableDelta{
		{Schema: "app", Table: "orders", BaseRows: 10, BranchRows: 12, Delta: 2,
			SampleRows: []map[string]any{{"id": 11}, {"id": 12}}},
		{Schema: "public", Table: "big", BaseRows: engine.UnknownRows, BranchRows: 99978, RowsUnknown: true},
		{Schema: "public", Table: "orders", BaseRows: 5, BranchRows: 5},
	}}

	var all bytes.Buffer
	if err := renderDiff(&all, res, true, true); err != nil {
		t.Fatal(err)
	}
	out := all.String()
	for _, want := range []string{"app.orders  10    12      +2", "big         ?     99978   ?", "orders      5     5       0", "new rows in app.orders (up to 2):"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "-1") {
		t.Errorf("unknown count rendered as a number:\n%s", out)
	}
	if strings.Contains(out, "no sampleable rows") {
		t.Errorf("unknown-count table listed as grown but unsampled:\n%s", out)
	}

	if strings.Contains(out, "not shown") {
		t.Errorf("--all view claims a hidden table:\n%s", out)
	}

	// changed-only view: the unknown table has no delta to show, so a footer
	// counts it and points at --all
	var changed bytes.Buffer
	if err := renderDiff(&changed, res, false, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(changed.String(), "big") || !strings.Contains(changed.String(), "app.orders") ||
		!strings.Contains(changed.String(), "(1 table with an unknown row count not shown: never analyzed and too large to count exactly; --all lists it)") {
		t.Errorf("changed-only view:\n%s", changed.String())
	}
}

// With no known change, the changed-only view claims "no row-count changes"
// only when no count is unknown either.
func TestRenderDiffNoKnownChanges(t *testing.T) {
	unknown := engine.TableDelta{Table: "big", BaseRows: engine.UnknownRows, BranchRows: engine.UnknownRows, RowsUnknown: true}
	same := engine.TableDelta{Table: "users", BaseRows: 3, BranchRows: 3}

	var buf bytes.Buffer
	if err := renderDiff(&buf, &engine.DiffResult{Tables: []engine.TableDelta{unknown, same, unknown}}, false, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"tables: no known row-count changes", "(2 tables with an unknown row count not shown:"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("output missing %q:\n%s", want, buf.String())
		}
	}

	buf.Reset()
	if err := renderDiff(&buf, &engine.DiffResult{Tables: []engine.TableDelta{same}}, false, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "tables: no row-count changes") || strings.Contains(buf.String(), "unknown") {
		t.Errorf("output for a diff with no changes and no unknown counts:\n%s", buf.String())
	}
}
