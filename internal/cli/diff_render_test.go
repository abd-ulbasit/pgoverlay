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

	// changed-only view: the unknown table has no delta to show
	var changed bytes.Buffer
	if err := renderDiff(&changed, res, false, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(changed.String(), "big") || !strings.Contains(changed.String(), "app.orders") {
		t.Errorf("changed-only view:\n%s", changed.String())
	}
}
