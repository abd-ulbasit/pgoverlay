package metrics

import "github.com/prometheus/client_golang/prometheus"

// stateCollector reports pgoverlay_branches_total{state} and
// pgoverlay_sources_total{state} by querying the registry on every scrape, so
// the gauges always reflect current state without a background refresher.
type stateCollector struct {
	sc           StateCounter
	branchesDesc *prometheus.Desc
	sourcesDesc  *prometheus.Desc
}

func newStateCollector(sc StateCounter) *stateCollector {
	return &stateCollector{
		sc: sc,
		branchesDesc: prometheus.NewDesc(
			"pgoverlay_branches_total",
			"Number of branches by state.",
			[]string{"state"}, nil,
		),
		sourcesDesc: prometheus.NewDesc(
			"pgoverlay_sources_total",
			"Number of sources by state.",
			[]string{"state"}, nil,
		),
	}
}

func (c *stateCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.branchesDesc
	ch <- c.sourcesDesc
}

func (c *stateCollector) Collect(ch chan<- prometheus.Metric) {
	if counts, err := c.sc.CountBranchesByState(); err == nil {
		for state, n := range counts {
			ch <- prometheus.MustNewConstMetric(c.branchesDesc, prometheus.GaugeValue, float64(n), state)
		}
	}
	if counts, err := c.sc.CountSourcesByState(); err == nil {
		for state, n := range counts {
			ch <- prometheus.MustNewConstMetric(c.sourcesDesc, prometheus.GaugeValue, float64(n), state)
		}
	}
}

// copyUpModes are the values pgoverlay_cow_copyup_mode reports, one series
// each (cow.CopyUpModes).
var copyUpModes = []string{"clone", "copy", "unknown"}

// copyUpCollector reports pgoverlay_cow_copyup_mode{mode}: 1 for the copy-up
// mode the startup probe found, 0 for the others. It asks mode on every
// scrape, so the gauge follows the probe, which runs in the background.
type copyUpCollector struct {
	mode func() string
	desc *prometheus.Desc
}

func newCopyUpCollector(mode func() string) *copyUpCollector {
	return &copyUpCollector{
		mode: mode,
		desc: prometheus.NewDesc(
			"pgoverlay_cow_copyup_mode",
			"What an OverlayFS copy-up costs on the filesystem holding the branch volumes, as probed at startup: clone (XFS reflink=1, btrfs: extents are shared, block-level copy-on-write), copy (the file's data is copied) or unknown (not probed yet, or the probe failed). 1 for the current mode, 0 for the others.",
			[]string{"mode"}, nil,
		),
	}
}

func (c *copyUpCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *copyUpCollector) Collect(ch chan<- prometheus.Metric) {
	cur := c.mode()
	known := false
	for _, m := range copyUpModes {
		known = known || m == cur
	}
	if !known {
		cur = "unknown"
	}
	for _, m := range copyUpModes {
		v := 0.0
		if m == cur {
			v = 1
		}
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, v, m)
	}
}

// SetCopyUpMode registers pgoverlay_cow_copyup_mode, which reports what mode
// returns on every scrape (clone, copy or unknown). Call once at wire-up
// (branchd, overlay backend). No-op on a nil receiver or a nil mode.
func (m *Metrics) SetCopyUpMode(mode func() string) {
	if m == nil || mode == nil {
		return
	}
	m.reg.MustRegister(newCopyUpCollector(mode))
}
