package api

import (
	"net/http"
	"testing"

	"github.com/abd-ulbasit/pgbranch/internal/registry"
)

// By default (no leader election) the gate is leader=true, so mutating routes
// behave normally.
func TestLeaderGateDefaultIsLeader(t *testing.T) {
	ts, srv := newTestServerWithLeader(t)
	addSource(t, ts)
	if !srv.LeaderGate().IsLeader() {
		t.Fatal("default leader gate should report leader=true")
	}
	if code, body := do(t, ts, testToken, "POST", "/v1/branches", CreateBranchRequest{Name: "br", Source: "main"}); code != http.StatusCreated {
		t.Errorf("leader POST /v1/branches = %d (%s), want 201", code, body)
	}
}

// When the gate is closed (not leader), mutating routes return 503 "not
// leader" — and the leader-gate check happens regardless of auth role.
func TestLeaderGateBlocksMutations(t *testing.T) {
	ts, srv := newTestServerWithLeader(t)
	addSource(t, ts)
	srv.LeaderGate().Set(false)

	mutating := []struct {
		method, path string
		body         any
	}{
		{"POST", "/v1/branches", CreateBranchRequest{Name: "x", Source: "main"}},
		{"DELETE", "/v1/branches/x", nil},
		{"POST", "/v1/branches/x/reset", nil},
		{"POST", "/v1/sources", CreateSourceRequest{Name: "s2", Host: "h", Port: 5432, User: "u", Database: "d", PGVersion: "17", Password: "p"}},
		{"DELETE", "/v1/sources/main", nil},
		{"POST", "/v1/reconcile", nil},
		{"POST", "/v1/tokens", CreateTokenRequest{Name: "t", Role: registry.RoleViewer}},
		{"DELETE", "/v1/tokens/t", nil},
	}
	for _, tc := range mutating {
		code, body := do(t, ts, testToken, tc.method, tc.path, tc.body)
		if code != http.StatusServiceUnavailable {
			t.Errorf("non-leader %s %s = %d (%s), want 503", tc.method, tc.path, code, body)
		}
	}
}

// Reads, health, readiness and metrics are unaffected by leader state: a
// non-leader still serves them so it can back read-only traffic and probes.
func TestLeaderGateAllowsReadsAndProbes(t *testing.T) {
	ts, srv := newTestServerWithLeader(t)
	addSource(t, ts)
	srv.LeaderGate().Set(false)

	reads := []string{"/v1/branches", "/v1/sources", "/v1/reconcile/plan", "/v1/tokens"}
	for _, p := range reads {
		if code, body := do(t, ts, testToken, "GET", p, nil); code != http.StatusOK {
			t.Errorf("non-leader GET %s = %d (%s), want 200", p, code, body)
		}
	}
	for _, p := range []string{"/healthz", "/readyz", "/metrics"} {
		if code, _ := do(t, ts, "", "GET", p, nil); code != http.StatusOK {
			t.Errorf("non-leader GET %s = %d, want 200", p, code)
		}
	}
}
