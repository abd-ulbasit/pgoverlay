package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sys/unix"
)

// diskCollector reports free/total bytes on the filesystem that holds the
// storage root (all copy-on-write branch volumes plus the SQLite registry).
// It calls statfs on every scrape so the values are always fresh; a failing
// statfs (e.g. the root vanished) is silently skipped rather than reported as
// zero, so an alert on "free < threshold" never fires spuriously on a transient
// error.
//
// statfs is satisfied by golang.org/x/sys/unix on both Linux (where branchd
// runs) and Darwin (dev), so no build tags are needed.
type diskCollector struct {
	root      string
	freeDesc  *prometheus.Desc
	totalDesc *prometheus.Desc
}

func newDiskCollector(root string) *diskCollector {
	return &diskCollector{
		root: root,
		freeDesc: prometheus.NewDesc(
			"pgbranch_disk_bytes_free",
			"Free bytes on the storage-root filesystem (all branch volumes + the registry share it; ENOSPC here fails every branch).",
			nil, nil,
		),
		totalDesc: prometheus.NewDesc(
			"pgbranch_disk_bytes_total",
			"Total bytes on the storage-root filesystem.",
			nil, nil,
		),
	}
}

func (c *diskCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.freeDesc
	ch <- c.totalDesc
}

func (c *diskCollector) Collect(ch chan<- prometheus.Metric) {
	var st unix.Statfs_t
	if err := unix.Statfs(c.root, &st); err != nil {
		// Transient error (root unmounted/removed): skip rather than report 0,
		// which would look like a full disk and trip free-space alerts.
		return
	}
	bsize := float64(st.Bsize)
	ch <- prometheus.MustNewConstMetric(c.freeDesc, prometheus.GaugeValue, float64(st.Bavail)*bsize)
	ch <- prometheus.MustNewConstMetric(c.totalDesc, prometheus.GaugeValue, float64(st.Blocks)*bsize)
}
