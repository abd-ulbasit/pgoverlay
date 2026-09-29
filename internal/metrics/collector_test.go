package metrics

import (
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func copyUpWant(clone, copy, unknown int) string {
	return `
# HELP pgoverlay_cow_copyup_mode What an OverlayFS copy-up costs on the filesystem holding the branch volumes, as probed at startup: clone (XFS reflink=1, btrfs: extents are shared, block-level copy-on-write), copy (the file's data is copied) or unknown (not probed yet, or the probe failed). 1 for the current mode, 0 for the others.
# TYPE pgoverlay_cow_copyup_mode gauge
pgoverlay_cow_copyup_mode{mode="clone"} ` + strconv.Itoa(clone) + `
pgoverlay_cow_copyup_mode{mode="copy"} ` + strconv.Itoa(copy) + `
pgoverlay_cow_copyup_mode{mode="unknown"} ` + strconv.Itoa(unknown) + `
`
}

// The gauge follows the mode function on every scrape: unknown until the
// background probe answers, then the probed mode.
func TestCopyUpModeFollowsTheProbe(t *testing.T) {
	m := New()
	mode := "unknown"
	m.SetCopyUpMode(func() string { return mode })
	for _, tc := range []struct {
		mode             string
		clone, copy, unk int
	}{
		{"unknown", 0, 0, 1},
		{"clone", 1, 0, 0},
		{"copy", 0, 1, 0},
		{"something-new", 0, 0, 1}, // an unexpected value reads as unknown
	} {
		mode = tc.mode
		if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(copyUpWant(tc.clone, tc.copy, tc.unk)),
			"pgoverlay_cow_copyup_mode"); err != nil {
			t.Fatalf("mode %q: %v", tc.mode, err)
		}
	}
}

func TestSetCopyUpModeNilSafe(t *testing.T) {
	var m *Metrics
	m.SetCopyUpMode(func() string { return "clone" }) // must not panic
	New().SetCopyUpMode(nil)                          // no collector, no panic
}
