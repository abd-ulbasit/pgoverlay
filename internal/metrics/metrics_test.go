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
	m.IncCompensationFailure("undo")
	m.IncCompensationFailure("undo")
	m.IncCompensationFailure("transition")

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
	if v := testutil.ToFloat64(m.compFailures.WithLabelValues("undo")); v != 2 {
		t.Fatalf("compFailures{undo}=%v want 2", v)
	}
	if v := testutil.ToFloat64(m.compFailures.WithLabelValues("transition")); v != 1 {
		t.Fatalf("compFailures{transition}=%v want 1", v)
	}
}

// TestCompensationFailureRegistered asserts the counter is registered on the
// private registry and gathers under its declared name with the kind label.
func TestCompensationFailureRegistered(t *testing.T) {
	m := New()
	m.IncCompensationFailure("cleanup")
	want := `
# HELP pgbranch_compensation_failures_total Best-effort saga compensation/failure-transition errors by kind (transition|undo|cleanup).
# TYPE pgbranch_compensation_failures_total counter
pgbranch_compensation_failures_total{kind="cleanup"} 1
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(want),
		"pgbranch_compensation_failures_total"); err != nil {
		t.Fatal(err)
	}
}

// TestDiskCollectorReportsFreeBytes asserts the disk-free collector registers
// and reports a plausible (>0) free/total byte count for a real path (the test
// temp dir). It avoids asserting an exact number, which is host-dependent.
func TestDiskCollectorReportsFreeBytes(t *testing.T) {
	m := New()
	m.SetDiskRoot(t.TempDir())

	mfs, err := m.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, mf := range mfs {
		switch mf.GetName() {
		case "pgbranch_disk_bytes_free", "pgbranch_disk_bytes_total":
			ms := mf.GetMetric()
			if len(ms) != 1 {
				t.Fatalf("%s: got %d samples, want 1", mf.GetName(), len(ms))
			}
			got[mf.GetName()] = ms[0].GetGauge().GetValue()
		}
	}
	free, okFree := got["pgbranch_disk_bytes_free"]
	total, okTotal := got["pgbranch_disk_bytes_total"]
	if !okFree || !okTotal {
		t.Fatalf("missing disk gauges: %+v", got)
	}
	if free <= 0 {
		t.Fatalf("pgbranch_disk_bytes_free=%v, want > 0", free)
	}
	if total <= 0 {
		t.Fatalf("pgbranch_disk_bytes_total=%v, want > 0", total)
	}
	if free > total {
		t.Fatalf("free (%v) > total (%v), implausible", free, total)
	}
}

// TestSetDiskRootNoOpOnEmptyPath: an empty path must not register the collector
// (and must not panic), so the disk gauges are simply absent.
func TestSetDiskRootNoOpOnEmptyPath(t *testing.T) {
	m := New()
	m.SetDiskRoot("")
	mfs, err := m.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if n := mf.GetName(); n == "pgbranch_disk_bytes_free" || n == "pgbranch_disk_bytes_total" {
			t.Fatalf("disk gauge %s registered for empty root", n)
		}
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
	m.IncCompensationFailure("undo")
	m.SetStateCounter(fakeStateCounter{})
	m.SetDiskRoot("/")
}
