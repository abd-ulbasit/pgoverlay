// Package ha implements branchd's optional high-availability leader election.
//
// With --leader-elect (kube only) several branchd replicas share a
// coordination.k8s.io Lease named LeaseName in the pod namespace; exactly one
// is the leader. The leader runs the reconcile loop and accepts mutating /v1
// requests (its API LeaderGate is open); non-leaders keep serving reads,
// /healthz, /readyz and /metrics off their own read-only registry handle and
// reject mutations with 503. Losing the Lease cancels the reconcile loop and
// closes the gate within the renew deadline; gaining it opens the gate and runs
// an immediate reconcile pass to converge any drift from the gap.
//
// The election orchestration is split so it is unit-testable without a real
// apiserver: Callbacks holds the gate + a reconcile runnable and exposes the
// OnStartedLeading / OnStoppedLeading hooks the client-go elector drives; Run
// builds the real leaderelection.LeaderElector around those callbacks.
package ha

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// LeaseName is the coordination.k8s.io Lease all branchd replicas contend for.
const LeaseName = "pgbranch-branchd"

// Default lease timings. LeaseDuration > RenewDeadline > RetryPeriod is required
// by client-go; failover happens within roughly LeaseDuration after a leader
// dies.
const (
	defaultLeaseDuration = 15 * time.Second
	defaultRenewDeadline = 10 * time.Second
	defaultRetryPeriod   = 2 * time.Second
)

// Gate is the leadership flag the callbacks flip; internal/api's *LeaderGate
// satisfies it. Kept as an interface so the callback logic is testable without
// the api package.
type Gate interface {
	Set(leader bool)
	IsLeader() bool
}

// Reconcile is the leader-only work the callbacks start on gaining leadership
// and cancel on losing it (branchd passes a closure around engine.RunReconcile).
// It must run until ctx is cancelled.
type Reconcile func(ctx context.Context)

// Callbacks adapts a Gate + Reconcile runnable to the client-go election
// callbacks. OnStartedLeading/OnStoppedLeading are the unit-testable seam.
type Callbacks struct {
	gate Gate
	run  Reconcile

	mu     sync.Mutex
	cancel context.CancelFunc // cancels the current reconcile loop
}

// NewCallbacks builds the election callbacks around a gate and reconcile loop.
func NewCallbacks(gate Gate, run Reconcile) *Callbacks {
	return &Callbacks{gate: gate, run: run}
}

// OnStartedLeading opens the gate and runs the reconcile loop for this
// leadership term. The client-go elector invokes it in its own goroutine and
// cancels leaderCtx when the term ends; we run the loop inline so returning
// from this callback coincides with the loop stopping.
func (c *Callbacks) OnStartedLeading(leaderCtx context.Context) {
	loopCtx, cancel := context.WithCancel(leaderCtx)
	c.mu.Lock()
	c.cancel = cancel
	c.mu.Unlock()
	c.gate.Set(true)
	c.run(loopCtx) // blocks until leaderCtx (or an explicit stop) cancels it
}

// OnStoppedLeading closes the gate and cancels the reconcile loop. It may be
// called without a preceding OnStartedLeading (per client-go's contract), so
// the cancel is nil-safe.
func (c *Callbacks) OnStoppedLeading() {
	c.gate.Set(false)
	c.mu.Lock()
	cancel := c.cancel
	c.cancel = nil
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Identity returns this replica's election identity: POD_NAME when set
// (Deployment wires it via fieldRef), else the hostname.
func Identity() string {
	if p := os.Getenv("POD_NAME"); p != "" {
		return p
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "branchd"
}

// Run blocks running leader election against the Lease in namespace until ctx
// is cancelled, driving cb's callbacks. It is the production path (a real
// apiserver); unit tests drive the callbacks directly. ReleaseOnCancel hands
// the Lease off promptly on graceful shutdown so a peer takes over fast.
func Run(ctx context.Context, cs kubernetes.Interface, namespace, identity string, cb *Callbacks) error {
	if namespace == "" {
		return fmt.Errorf("leader election requires a namespace")
	}
	if identity == "" {
		return fmt.Errorf("leader election requires an identity")
	}
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: LeaseName, Namespace: namespace},
		Client:     cs.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: identity},
	}
	le, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:            lock,
		ReleaseOnCancel: true,
		LeaseDuration:   defaultLeaseDuration,
		RenewDeadline:   defaultRenewDeadline,
		RetryPeriod:     defaultRetryPeriod,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: cb.OnStartedLeading,
			OnStoppedLeading: cb.OnStoppedLeading,
		},
	})
	if err != nil {
		return fmt.Errorf("build leader elector: %w", err)
	}
	// Run returns when ctx is cancelled or leadership is lost; loop so a
	// non-leader keeps contending for the Lease for the process's lifetime.
	for {
		le.Run(ctx)
		if ctx.Err() != nil {
			return nil
		}
	}
}
