// Package metrics owns pgbranch's Prometheus instrumentation: a private
// *prometheus.Registry (never the global default) holding the operation,
// reaper and reconcile series, plus a Collector that, on every scrape, reports
// the live branch/source counts by state from the registry.
//
// Every method on *Metrics is nil-safe: a nil receiver is a no-op, so engine
// code can call m.ObserveOp(...) unconditionally and unit tests that build an
// engine without metrics keep working.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// StateCounter reports current entity counts grouped by state. The engine's
// registry satisfies it; the collector queries it on each scrape so the
// branches/sources gauges never go stale.
type StateCounter interface {
	CountBranchesByState() (map[string]int, error)
	CountSourcesByState() (map[string]int, error)
}

// Metrics is pgbranch's metric set over a private registry.
type Metrics struct {
	reg *prometheus.Registry

	opDuration    *prometheus.HistogramVec // pgbranch_branch_op_duration_seconds{op}
	opErrors      *prometheus.CounterVec   // pgbranch_branch_op_errors_total{op}
	maskingDur    prometheus.Histogram     // pgbranch_masking_duration_seconds
	reaperRuns    prometheus.Counter       // pgbranch_reaper_runs_total
	reaperReaped  prometheus.Counter       // pgbranch_reaper_reaped_total
	reconcileRuns prometheus.Counter       // pgbranch_reconcile_runs_total
	reconcileActs *prometheus.CounterVec   // pgbranch_reconcile_actions_total{action}
	inflight      prometheus.Gauge         // pgbranch_inflight_ops
	compFailures  *prometheus.CounterVec   // pgbranch_compensation_failures_total{kind}
}

// New builds a Metrics over its own registry. The branch/source gauges are
// reported by the state collector registered later via SetStateCounter.
func New() *Metrics {
	m := &Metrics{
		reg: prometheus.NewRegistry(),
		opDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "pgbranch_branch_op_duration_seconds",
			Help:    "Duration of branch operations by op (create|reset|destroy|from_branch|diff).",
			Buckets: prometheus.DefBuckets,
		}, []string{"op"}),
		opErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "pgbranch_branch_op_errors_total",
			Help: "Failed branch operations by op.",
		}, []string{"op"}),
		maskingDur: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "pgbranch_masking_duration_seconds",
			Help:    "Duration of applying a source's masking scripts inside a branch.",
			Buckets: prometheus.DefBuckets,
		}),
		reaperRuns: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "pgbranch_reaper_runs_total",
			Help: "Number of TTL reaper passes.",
		}),
		reaperReaped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "pgbranch_reaper_reaped_total",
			Help: "Number of expired branches destroyed by the reaper.",
		}),
		reconcileRuns: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "pgbranch_reconcile_runs_total",
			Help: "Number of reconcile passes.",
		}),
		reconcileActs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "pgbranch_reconcile_actions_total",
			Help: "Reconcile actions taken by action (fail_stuck|remove_orphan_container|gc_layer|gc_volume).",
		}, []string{"action"}),
		inflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "pgbranch_inflight_ops",
			Help: "Branch operations currently in flight.",
		}),
		compFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "pgbranch_compensation_failures_total",
			Help: "Best-effort saga compensation/failure-transition errors by kind (transition|undo|cleanup).",
		}, []string{"kind"}),
	}
	m.reg.MustRegister(
		m.opDuration, m.opErrors, m.maskingDur,
		m.reaperRuns, m.reaperReaped, m.reconcileRuns, m.reconcileActs, m.inflight,
		m.compFailures,
	)
	return m
}

// SetStateCounter registers the branches/sources state-count collector backed
// by sc. Call once at wire-up (branchd). No-op on a nil receiver.
func (m *Metrics) SetStateCounter(sc StateCounter) {
	if m == nil || sc == nil {
		return
	}
	m.reg.MustRegister(newStateCollector(sc))
}

// SetDiskRoot registers the disk-free collector for the storage-root path
// (the filesystem holding all branch CoW volumes plus the SQLite registry).
// It reports pgbranch_disk_bytes_free / _total via statfs on every scrape, so
// the values are always fresh. Call once at wire-up (branchd). No-op on a nil
// receiver or an empty path.
func (m *Metrics) SetDiskRoot(root string) {
	if m == nil || root == "" {
		return
	}
	m.reg.MustRegister(newDiskCollector(root))
}

// Handler serves the private registry over promhttp. Never nil-safe: callers
// (branchd) always build a real Metrics for the API mux.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// Registry exposes the private registry for tests (prometheus/testutil).
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }

// ObserveOp records a branch operation's duration in seconds.
func (m *Metrics) ObserveOp(op string, seconds float64) {
	if m == nil {
		return
	}
	m.opDuration.WithLabelValues(op).Observe(seconds)
}

// IncOpError counts a failed branch operation.
func (m *Metrics) IncOpError(op string) {
	if m == nil {
		return
	}
	m.opErrors.WithLabelValues(op).Inc()
}

// ObserveMasking records the masking-script duration in seconds.
func (m *Metrics) ObserveMasking(seconds float64) {
	if m == nil {
		return
	}
	m.maskingDur.Observe(seconds)
}

// IncReaperRun counts one reaper pass; reaped is how many branches it destroyed.
func (m *Metrics) IncReaperRun(reaped int) {
	if m == nil {
		return
	}
	m.reaperRuns.Inc()
	if reaped > 0 {
		m.reaperReaped.Add(float64(reaped))
	}
}

// IncReconcileRun counts one reconcile pass.
func (m *Metrics) IncReconcileRun() {
	if m == nil {
		return
	}
	m.reconcileRuns.Inc()
}

// IncReconcileAction counts one reconcile action by kind.
func (m *Metrics) IncReconcileAction(action string) {
	if m == nil {
		return
	}
	m.reconcileActs.WithLabelValues(action).Inc()
}

// IncInflight / DecInflight bracket a saga entry point.
func (m *Metrics) IncInflight() {
	if m == nil {
		return
	}
	m.inflight.Inc()
}

func (m *Metrics) DecInflight() {
	if m == nil {
		return
	}
	m.inflight.Dec()
}

// IncCompensationFailure counts one best-effort compensation/failure-transition
// error by kind (transition|undo|cleanup). These are swallowed at the call site
// (cleanup proceeds best-effort) but surfaced here for alerting on leaks.
func (m *Metrics) IncCompensationFailure(kind string) {
	if m == nil {
		return
	}
	m.compFailures.WithLabelValues(kind).Inc()
}
