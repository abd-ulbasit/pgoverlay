package deploy

import (
	"strings"
	"testing"
)

// TestPodReadiness pins the gate the kube ITs put in front of every
// port-forward (waitPodReady). It needs no cluster: the input is what
// `kubectl get pod -o json` prints.
func TestPodReadiness(t *testing.T) {
	cases := []struct {
		name    string
		pod     string
		ready   bool
		whyHas  string
		wantErr bool
	}{
		{
			// The CI 30340851419 failure: the replacement pod already held the
			// Lease while the API server still showed it ContainerCreating.
			name: "pending, container creating",
			pod: `{"status":{"phase":"Pending","conditions":[
				{"type":"PodScheduled","status":"True"},
				{"type":"Ready","status":"False","reason":"ContainersNotReady"}],
				"containerStatuses":[{"name":"branchd","ready":false,
				"state":{"waiting":{"reason":"ContainerCreating"}}}]}}`,
			whyHas: "phase Pending",
		},
		{
			name: "running, readiness probe not passed yet",
			pod: `{"status":{"phase":"Running","conditions":[
				{"type":"Ready","status":"False","reason":"ContainersNotReady"}]}}`,
			whyHas: "Ready=False (ContainersNotReady)",
		},
		{
			name:   "running, no conditions reported yet",
			pod:    `{"status":{"phase":"Running"}}`,
			whyHas: "no Ready condition",
		},
		{
			name: "running and ready",
			pod: `{"status":{"phase":"Running","conditions":[
				{"type":"Initialized","status":"True"},
				{"type":"Ready","status":"True"}]}}`,
			ready: true,
		},
		{
			// Still Ready for a moment after `kubectl delete pod`, but the
			// forward would die with it.
			name: "ready but terminating",
			pod: `{"metadata":{"deletionTimestamp":"2026-07-28T08:10:30Z"},
				"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}}`,
			whyHas: "terminating",
		},
		{
			name:   "succeeded",
			pod:    `{"status":{"phase":"Succeeded"}}`,
			whyHas: "phase Succeeded",
		},
		{
			name:    "not json",
			pod:     `Error from server (NotFound): pods "x" not found`,
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ready, why, err := podReadiness([]byte(tc.pod))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if ready != tc.ready {
				t.Errorf("ready = %v, want %v (why %q)", ready, tc.ready, why)
			}
			if !strings.Contains(why, tc.whyHas) {
				t.Errorf("why = %q, want it to contain %q", why, tc.whyHas)
			}
		})
	}
}
