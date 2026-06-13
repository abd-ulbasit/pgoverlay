package api

import (
	"net/http"
	"sync/atomic"
)

// LeaderGate is the HA mutating-route gate: an atomic leadership flag the
// leader-election orchestration flips. It defaults to leader=true so that with
// leader election OFF (docker/local, single instance) every instance is always
// the leader and mutating routes behave normally. When false, mutating /v1
// routes return 503 "not leader" while reads, /healthz, /readyz and /metrics
// keep serving (a non-leader backs read traffic and probes off its own
// read-only registry handle).
type LeaderGate struct {
	leader atomic.Bool
}

// newLeaderGate returns a gate that starts as the leader (single-instance
// default).
func newLeaderGate() *LeaderGate {
	g := &LeaderGate{}
	g.leader.Store(true)
	return g
}

// IsLeader reports whether this instance currently holds leadership.
func (g *LeaderGate) IsLeader() bool { return g.leader.Load() }

// Set flips the leadership flag (called from the election callbacks).
func (g *LeaderGate) Set(leader bool) { g.leader.Store(leader) }

// requireLeader wraps a mutating handler so it returns 503 when this instance
// is not the leader. It is composed with requireRole on the same mutating
// routes; reads/probes never get this wrapper.
func (s *Server) requireLeader(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.leader.IsLeader() {
			writeError(w, http.StatusServiceUnavailable, "not leader")
			return
		}
		next(w, r)
	}
}

// mutate composes the leader gate with the role check for a mutating route:
// the leader gate is checked first (a non-leader rejects before doing any auth
// work), then the minimum-role bearer check.
func (s *Server) mutate(min string, next http.HandlerFunc) http.HandlerFunc {
	return s.requireLeader(s.requireRole(min, next))
}
