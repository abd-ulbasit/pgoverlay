package metrics

import "github.com/prometheus/client_golang/prometheus"

// stateCollector reports pgbranch_branches_total{state} and
// pgbranch_sources_total{state} by querying the registry on every scrape, so
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
			"pgbranch_branches_total",
			"Number of branches by state.",
			[]string{"state"}, nil,
		),
		sourcesDesc: prometheus.NewDesc(
			"pgbranch_sources_total",
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
