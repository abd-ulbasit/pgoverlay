package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestSetLeaderGaugeAndTransitions(t *testing.T) {
	m := New()
	m.SetLeader(false) // first observation: sets the gauge, no transition
	m.SetLeader(true)
	m.SetLeader(true) // unchanged
	m.SetLeader(false)
	m.SetLeader(true)

	want := `
# HELP pgoverlay_leader 1 when this branchd replica is the leader (accepts mutations, runs reconcile), else 0. Always 1 without --leader-elect.
# TYPE pgoverlay_leader gauge
pgoverlay_leader 1
# HELP pgoverlay_leader_transitions_total Number of times this replica gained or lost leadership.
# TYPE pgoverlay_leader_transitions_total counter
pgoverlay_leader_transitions_total 3
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(want),
		"pgoverlay_leader", "pgoverlay_leader_transitions_total"); err != nil {
		t.Fatal(err)
	}

	var nilM *Metrics
	nilM.SetLeader(true) // nil-safe like every other method
}
