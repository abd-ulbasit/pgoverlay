package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// fakeStateCounter seeds the collector with fixed per-state counts.
type fakeStateCounter struct {
	branches map[string]int
	sources  map[string]int
}

func (f fakeStateCounter) CountBranchesByState() (map[string]int, error) { return f.branches, nil }
func (f fakeStateCounter) CountSourcesByState() (map[string]int, error)  { return f.sources, nil }

func TestStateCollectorReportsGauges(t *testing.T) {
	m := New()
	m.SetStateCounter(fakeStateCounter{
		branches: map[string]int{"ready": 3, "failed": 1},
		sources:  map[string]int{"ready": 2},
	})

	want := `
# HELP pgbranch_branches_total Number of branches by state.
# TYPE pgbranch_branches_total gauge
pgbranch_branches_total{state="failed"} 1
pgbranch_branches_total{state="ready"} 3
# HELP pgbranch_sources_total Number of sources by state.
# TYPE pgbranch_sources_total gauge
pgbranch_sources_total{state="ready"} 2
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(want),
		"pgbranch_branches_total", "pgbranch_sources_total"); err != nil {
		t.Fatal(err)
	}
}

func TestObserveOpRecordsHistogram(t *testing.T) {
	m := New()
	m.ObserveOp("create", 0.5)
	m.ObserveOp("create", 1.5)
	// the create histogram's sample count reflects both observations
	want := `
# HELP pgbranch_branch_op_duration_seconds Duration of branch operations by op (create|reset|destroy|from_branch|diff).
# TYPE pgbranch_branch_op_duration_seconds histogram
pgbranch_branch_op_duration_seconds_count{op="create"} 2
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(want),
		"pgbranch_branch_op_duration_seconds_count"); err != nil {
		t.Fatal(err)
	}
}

func TestCounterAndGaugeHelpers(t *testing.T) {
	m := New()
	m.IncOpError("reset")
	m.IncReaperRun(2)
	m.IncReaperRun(0)
	m.IncReconcileRun()
	m.IncReconcileAction("gc_layer")
	m.IncReconcileAction("gc_layer")
	m.ObserveMasking(0.1)
	m.IncInflight()
	m.IncInflight()
	m.DecInflight()

	if v := testutil.ToFloat64(m.opErrors.WithLabelValues("reset")); v != 1 {
		t.Fatalf("opErrors=%v want 1", v)
	}
	if v := testutil.ToFloat64(m.reaperRuns); v != 2 {
		t.Fatalf("reaperRuns=%v want 2", v)
	}
	if v := testutil.ToFloat64(m.reaperReaped); v != 2 {
		t.Fatalf("reaperReaped=%v want 2", v)
	}
	if v := testutil.ToFloat64(m.reconcileRuns); v != 1 {
		t.Fatalf("reconcileRuns=%v want 1", v)
	}
	if v := testutil.ToFloat64(m.reconcileActs.WithLabelValues("gc_layer")); v != 2 {
		t.Fatalf("reconcileActs=%v want 2", v)
	}
	if v := testutil.ToFloat64(m.inflight); v != 1 {
		t.Fatalf("inflight=%v want 1", v)
	}
}

// All methods on a nil *Metrics must be no-ops (engine code calls them
// unconditionally; tests build engines without metrics).
func TestNilReceiverIsNoOp(t *testing.T) {
	var m *Metrics
	m.ObserveOp("create", 1)
	m.IncOpError("create")
	m.ObserveMasking(1)
	m.IncReaperRun(3)
	m.IncReconcileRun()
	m.IncReconcileAction("gc_volume")
	m.IncInflight()
	m.DecInflight()
	m.SetStateCounter(fakeStateCounter{})
}
