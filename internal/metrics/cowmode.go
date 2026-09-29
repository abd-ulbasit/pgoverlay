package metrics

import "github.com/prometheus/client_golang/prometheus"

// cowModes are the values pgoverlay_branch_cow_mode reports, one series each
// (engine.CowModes).
var cowModes = []string{"lazyrw", "eager", "off", "unknown"}

// cowModeCollector reports pgoverlay_branch_cow_mode{mode}: how many ready
// overlay branches run in each copy-on-write mode. It asks counts on every
// scrape, so the gauge follows branch starts and destroys.
type cowModeCollector struct {
	counts func() map[string]int
	desc   *prometheus.Desc
}

func newCowModeCollector(counts func() map[string]int) *cowModeCollector {
	return &cowModeCollector{
		counts: counts,
		desc: prometheus.NewDesc(
			"pgoverlay_branch_cow_mode",
			"Ready overlay branches by the copy-on-write mode their Postgres started in: lazyrw (the lazyrw shim is active: reads copy nothing, a file is copied into the branch on its first write), eager (the shim is not active although --lazyrw=on, or the branch predates it: every relation file Postgres opens is copied), off (--lazyrw=off) or unknown (not read yet, or unreadable).",
			[]string{"mode"}, nil,
		),
	}
}

func (c *cowModeCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *cowModeCollector) Collect(ch chan<- prometheus.Metric) {
	counts := c.counts()
	if counts == nil {
		return
	}
	for _, m := range cowModes {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(counts[m]), m)
	}
}

// SetCowModes registers pgoverlay_branch_cow_mode, which reports what counts
// returns on every scrape: ready overlay branches by mode (lazyrw, eager, off,
// unknown; a mode counts returns nothing for is 0). A nil map skips the
// scrape. Call once at wire-up (branchd, overlay backend). No-op on a nil
// receiver or a nil counts.
func (m *Metrics) SetCowModes(counts func() map[string]int) {
	if m == nil || counts == nil {
		return
	}
	m.reg.MustRegister(newCowModeCollector(counts))
}
