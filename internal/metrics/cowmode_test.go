package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// Every mode is reported, 0 when no branch is in it, so an alert on
// eager > 0 has a series to evaluate from the first scrape.
func TestCowModeCollectorReportsEveryMode(t *testing.T) {
	m := New()
	counts := map[string]int{"lazyrw": 3, "eager": 1}
	m.SetCowModes(func() map[string]int { return counts })
	want := `
# HELP pgoverlay_branch_cow_mode Ready overlay branches by the copy-on-write mode their Postgres started in: lazyrw (the lazyrw shim is active: reads copy nothing, a file is copied into the branch on its first write), eager (the shim is not active although --lazyrw=on, or the branch predates it: every relation file Postgres opens is copied), off (--lazyrw=off) or unknown (not read yet, or unreadable).
# TYPE pgoverlay_branch_cow_mode gauge
pgoverlay_branch_cow_mode{mode="eager"} 1
pgoverlay_branch_cow_mode{mode="lazyrw"} 3
pgoverlay_branch_cow_mode{mode="off"} 0
pgoverlay_branch_cow_mode{mode="unknown"} 0
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(want), "pgoverlay_branch_cow_mode"); err != nil {
		t.Fatal(err)
	}
	// the gauge follows the source on every scrape
	counts = map[string]int{"lazyrw": 4}
	if got := testutil.CollectAndCount(newCowModeCollector(func() map[string]int { return counts })); got != 4 {
		t.Fatalf("series = %d, want one per mode (4)", got)
	}
}

// A scrape the engine cannot answer (registry unreadable) reports nothing
// rather than zeros that would read as "no eager branches".
func TestCowModeCollectorSkipsAnUnansweredScrape(t *testing.T) {
	m := New()
	m.SetCowModes(func() map[string]int { return nil })
	if n, err := testutil.GatherAndCount(m.Registry(), "pgoverlay_branch_cow_mode"); err != nil || n != 0 {
		t.Fatalf("series = %d (err %v), want none", n, err)
	}
	var nilM *Metrics
	nilM.SetCowModes(func() map[string]int { return nil }) // nil-safe
	New().SetCowModes(nil)                                 // nil counts: no-op
}
