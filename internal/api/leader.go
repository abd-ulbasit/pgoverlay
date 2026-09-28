package api

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"
)

// LeaderGate is the HA mutating-route gate: the leadership flag the
// leader-election orchestration flips. It defaults to leader=true so that with
// leader election OFF (docker/local, single instance) every instance is always
// the leader and mutating routes behave normally. When false, mutating /v1
// routes return 503 "not leader" while reads, /healthz, /readyz and /metrics
// keep serving.
//
// Each stretch of leadership is a term with its own context. Mutations admitted
// during a term run on a context derived from it, so losing leadership cancels
// every saga the deposed leader still has in flight (their compensations run on
// a detached context) instead of letting it keep writing next to the new leader.
type LeaderGate struct {
	mu        sync.Mutex
	leader    bool
	term      context.Context
	endTerm   context.CancelFunc
	observers []func(leader bool)
}

// newLeaderGate returns a gate that starts as the leader (single-instance
// default).
func newLeaderGate() *LeaderGate {
	g := &LeaderGate{}
	g.Set(true)
	return g
}

// IsLeader reports whether this instance currently holds leadership.
func (g *LeaderGate) IsLeader() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.leader
}

// Set flips the leadership flag (called from the election callbacks). Opening
// the gate starts a new term; closing it ends the current term, cancelling the
// mutations admitted during it. Setting the current value is a no-op.
func (g *LeaderGate) Set(leader bool) {
	g.mu.Lock()
	if leader == g.leader && g.term != nil {
		g.mu.Unlock()
		return
	}
	if leader {
		g.term, g.endTerm = context.WithCancel(context.Background())
	} else if g.endTerm != nil {
		g.endTerm()
	}
	g.leader = leader
	for _, fn := range g.observers {
		fn(leader)
	}
	g.mu.Unlock()
}

// Observe registers fn to be called with the new value on every leadership
// change, and once immediately with the current value (branchd wires the
// pgoverlay_leader gauge here). fn runs under the gate's lock, in order, and
// must not call back into the gate.
func (g *LeaderGate) Observe(fn func(leader bool)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.observers = append(g.observers, fn)
	fn(g.leader)
}

// admit returns the current term's context when this instance is the leader.
func (g *LeaderGate) admit() (context.Context, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.term, g.leader
}

// mutations tracks the sagas the API has in flight so shutdown can wait for
// them, refuse new ones, and as a last resort cancel the stragglers.
type mutations struct {
	mu       sync.Mutex
	wg       sync.WaitGroup
	draining bool
	ctx      context.Context // parent of every mutation; cancelled by cancelAll
	cancel   context.CancelFunc
}

func newMutations() *mutations {
	ctx, cancel := context.WithCancel(context.Background())
	return &mutations{ctx: ctx, cancel: cancel}
}

// begin admits one mutation. The returned context keeps the request's values
// (the audit actor, the role) but not its cancellation: a client that
// disconnects, times out or sits behind a load balancer with a short idle
// timeout must not abort a half-done saga. It is cancelled instead when the
// leadership term ends, when shutdown gives up waiting (cancelAll), or after
// timeout (0 = no deadline). done must be called when the handler returns.
func (m *mutations) begin(r *http.Request, term context.Context, timeout time.Duration) (ctx context.Context, done func(), ok bool) {
	m.mu.Lock()
	if m.draining {
		m.mu.Unlock()
		return nil, nil, false
	}
	m.wg.Add(1)
	m.mu.Unlock()

	base := context.WithoutCancel(r.Context())
	cancelTimeout := context.CancelFunc(func() {})
	if timeout > 0 {
		base, cancelTimeout = context.WithTimeoutCause(base, timeout, errMutationTimeout)
	}
	ctx, cancel := context.WithCancelCause(base)
	stopTerm := context.AfterFunc(term, func() { cancel(errLeadershipLost) })
	stopAll := context.AfterFunc(m.ctx, func() { cancel(errShuttingDown) })
	return ctx, func() {
		stopTerm()
		stopAll()
		cancel(nil)
		cancelTimeout()
		m.wg.Done()
	}, true
}

// Causes recorded on a mutation's context when something other than the saga
// itself ends it; writeEngineError turns them into a retryable status.
var (
	errLeadershipLost  = errors.New("leadership moved to another replica while the operation ran")
	errShuttingDown    = errors.New("branchd is shutting down")
	errMutationTimeout = errors.New("operation exceeded the stuck timeout")
)

// stopAdmitting makes every later begin fail; in-flight mutations continue.
func (m *mutations) stopAdmitting() {
	m.mu.Lock()
	m.draining = true
	m.mu.Unlock()
}

// cancelAll cancels every in-flight mutation (cause errShuttingDown).
func (m *mutations) cancelAll() { m.cancel() }

// wait blocks until every admitted mutation has returned or ctx is done.
func (m *mutations) wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// requireLeader wraps a mutating handler: a non-leader answers 503 "not
// leader", a draining server 503 "shutting down"; otherwise the handler runs on
// a tracked context detached from the client connection (see mutations.begin).
// It always sits INSIDE requireRole, so an unauthenticated or under-privileged
// caller gets 401/403 before leadership is revealed.
func (s *Server) requireLeader(timeout time.Duration, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		term, ok := s.leader.admit()
		if !ok {
			writeError(w, http.StatusServiceUnavailable, "not leader")
			return
		}
		ctx, done, ok := s.muts.begin(r, term, timeout)
		if !ok {
			writeError(w, http.StatusServiceUnavailable, "shutting down")
			return
		}
		defer done()
		next(w, r.WithContext(ctx))
	}
}

// mutate composes the role check with the leader gate for a mutating route:
// authentication and the minimum role are checked first (401/403 win over
// 503), then leadership. The mutation runs until it finishes, the leadership
// term ends, or shutdown cancels it.
func (s *Server) mutate(min string, next http.HandlerFunc) http.HandlerFunc {
	return s.requireRole(min, s.requireLeader(0, next))
}

// mutateBranch is mutate for branch sagas (create, reset, destroy, diff,
// reconcile): they are additionally bounded by the stuck timeout, the age at
// which the reconcile loop fails a creating/resetting row anyway — so the saga
// stops and compensates itself before reconcile races it for the same row.
func (s *Server) mutateBranch(min string, next http.HandlerFunc) http.HandlerFunc {
	return s.requireRole(min, s.requireLeader(s.stuckTimeout, next))
}

// StopAdmitting makes the API refuse new mutations with 503 "shutting down";
// mutations already in flight continue. branchd calls it on SIGTERM.
func (s *Server) StopAdmitting() { s.muts.stopAdmitting() }

// WaitMutations blocks until every in-flight mutation has returned or ctx is
// done (then it returns ctx.Err()).
func (s *Server) WaitMutations(ctx context.Context) error { return s.muts.wait(ctx) }

// CancelMutations cancels every in-flight mutation's context. The sagas then
// run their compensations (on a detached context) and return; use
// WaitMutations to wait for that. branchd calls it when the shutdown drain
// budget runs out.
func (s *Server) CancelMutations() { s.muts.cancelAll() }
