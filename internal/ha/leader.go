// Package ha implements branchd's optional high-availability leader election.
//
// With --leader-elect (kube only) several branchd replicas share a
// coordination.k8s.io Lease named LeaseName in the pod namespace; exactly one
// is the leader. The leader runs the reconcile loop and accepts mutating /v1
// requests (its API LeaderGate is open); non-leaders keep serving /healthz,
// /readyz, /metrics and reads, and reject mutations with 503. Every replica
// opens the same read-write registry: the gate, not the handle, is what keeps
// followers from writing. Losing the Lease closes the gate within the renew
// deadline, which cancels the reconcile loop and every mutation still in
// flight; gaining it opens the gate and runs an immediate reconcile pass to
// converge any drift from the gap.
//
// So that clients reach the leader without knowing which pod it is, the
// leader labels its own pod LeaderLabel=true (PodLabeler) and the chart's API
// Service selects on that label. Followers stay Ready (Deployment rollouts and
// `helm --wait` are unaffected) but receive no API traffic through the Service.
//
// The election orchestration is split so it is unit-testable without a real
// apiserver: Callbacks holds the gate + a reconcile runnable (+ an optional
// Marker) and exposes the OnStartedLeading / OnStoppedLeading hooks the
// client-go elector drives; Run builds the real leaderelection.LeaderElector
// around those callbacks.
package ha

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// LeaseName is the coordination.k8s.io Lease all branchd replicas contend for.
const LeaseName = "pgoverlay-branchd"

// LeaderLabel is the pod label ("true") the current leader carries. The
// chart's API Service selects on it so /v1 traffic reaches only the leader.
const LeaderLabel = "pgoverlay.leader"

// Default lease timings. LeaseDuration > RenewDeadline > RetryPeriod is required
// by client-go; failover happens within roughly LeaseDuration after a leader
// dies.
const (
	defaultLeaseDuration = 15 * time.Second
	defaultRenewDeadline = 10 * time.Second
	defaultRetryPeriod   = 2 * time.Second
)

// Marker timings: the leader re-asserts its mark (and clears stale ones) every
// markInterval, retrying every markRetry after a failure; removing the mark on
// losing leadership is bounded by unmarkTimeout.
const (
	markInterval  = 30 * time.Second
	markRetry     = defaultRetryPeriod
	unmarkTimeout = 5 * time.Second
)

// leaderlessCheck is how often each replica checks that SOME replica holds a
// live Lease, warning when none does (see watchLeaderless).
const leaderlessCheck = time.Minute

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

// Marker publishes leadership outside the process so traffic can be routed to
// the leader (PodLabeler in-cluster).
type Marker interface {
	// Mark records that this replica leads and clears marks other replicas
	// left behind. The leader calls it periodically for its whole term.
	Mark(ctx context.Context) error
	// Unmark removes this replica's mark.
	Unmark(ctx context.Context) error
}

// Callbacks adapts a Gate + Reconcile runnable (+ an optional Marker) to the
// client-go election callbacks. OnStartedLeading/OnStoppedLeading are the
// unit-testable seam.
type Callbacks struct {
	gate      Gate
	run       Reconcile
	marker    Marker
	markEvery time.Duration

	mu       sync.Mutex
	cancel   context.CancelFunc // cancels the current term's loops
	markDone chan struct{}      // closed when the current term's marker loop exits
}

// Option configures NewCallbacks.
type Option func(*Callbacks)

// WithMarker makes the leader keep m marked for its whole term and unmark it
// when the term ends.
func WithMarker(m Marker) Option {
	return func(c *Callbacks) { c.marker = m }
}

// NewCallbacks builds the election callbacks around a gate and reconcile loop.
func NewCallbacks(gate Gate, run Reconcile, opts ...Option) *Callbacks {
	c := &Callbacks{gate: gate, run: run, markEvery: markInterval}
	for _, o := range opts {
		o(c)
	}
	return c
}

// OnStartedLeading opens the gate, starts the marker loop and runs the
// reconcile loop for this leadership term. The client-go elector invokes it in
// its own goroutine and cancels leaderCtx when the term ends; we run the loop
// inline so returning from this callback coincides with the loop stopping.
//
// Because it runs in a goroutine, the term may already be over when it starts
// (client-go cancels leaderCtx and then calls OnStoppedLeading); in that case
// it must not reopen the gate, or a non-leader would accept writes.
func (c *Callbacks) OnStartedLeading(leaderCtx context.Context) {
	c.mu.Lock()
	if leaderCtx.Err() != nil {
		c.mu.Unlock()
		return
	}
	loopCtx, cancel := context.WithCancel(leaderCtx)
	c.cancel = cancel
	c.gate.Set(true)
	if c.marker != nil {
		done := make(chan struct{})
		c.markDone = done
		go func() {
			defer close(done)
			keepMarked(loopCtx, c.marker, c.markEvery)
		}()
	}
	c.mu.Unlock()
	slog.Info("ha: acquired leadership")
	c.run(loopCtx) // blocks until leaderCtx (or an explicit stop) cancels it
}

// OnStoppedLeading closes the gate (which cancels the mutations admitted
// during the term), cancels the reconcile and marker loops, then removes this
// replica's mark. It may be called without a preceding OnStartedLeading (per
// client-go's contract, e.g. a follower shutting down), so every step is
// nil-safe; unmarking then clears a label a previous container of this pod may
// have left.
func (c *Callbacks) OnStoppedLeading() {
	c.mu.Lock()
	wasLeader := c.cancel != nil
	c.gate.Set(false)
	cancel, markDone := c.cancel, c.markDone
	c.cancel, c.markDone = nil, nil
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if wasLeader {
		slog.Info("ha: lost leadership")
	}
	if c.marker == nil {
		return
	}
	if markDone != nil {
		<-markDone // no Mark may land after the Unmark below
	}
	ctx, cancelUnmark := context.WithTimeout(context.Background(), unmarkTimeout)
	defer cancelUnmark()
	if err := c.marker.Unmark(ctx); err != nil {
		slog.Warn("ha: could not remove the leader label from this pod; the next leader clears it", "err", err)
	}
}

// keepMarked calls m.Mark now and then every `every` until ctx ends, retrying
// sooner after a failure.
func keepMarked(ctx context.Context, m Marker, every time.Duration) {
	for {
		wait := every
		if err := m.Mark(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("ha: could not label this pod as the leader; the API Service does not route to it until this succeeds", "err", err)
			wait = markRetry
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// PodLabeler is the in-cluster Marker: LeaderLabel=true on this replica's own
// pod (Namespace/Pod), removed from every other pod. It needs get, list and
// patch on pods in the namespace.
type PodLabeler struct {
	Client    kubernetes.Interface
	Namespace string
	Pod       string
}

// Mark labels this pod and clears the label from any other pod: a previous
// leader that lost the Lease without reaching the apiserver (partition, crash)
// cannot clear its own. The Lease is per namespace, so every labelled pod in
// the namespace is a competitor for the same leadership.
func (p *PodLabeler) Mark(ctx context.Context) error {
	pods := p.Client.CoreV1().Pods(p.Namespace)
	self, err := pods.Get(ctx, p.Pod, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get own pod %s/%s: %w", p.Namespace, p.Pod, err)
	}
	if self.Labels[LeaderLabel] != "true" {
		if err := p.setLabel(ctx, p.Pod, "true"); err != nil {
			return err
		}
	}
	labelled, err := pods.List(ctx, metav1.ListOptions{LabelSelector: LeaderLabel + "=true"})
	if err != nil {
		return fmt.Errorf("list pods labelled %s: %w", LeaderLabel, err)
	}
	var errs []error
	for _, other := range labelled.Items {
		if other.Name == p.Pod {
			continue
		}
		if err := p.setLabel(ctx, other.Name, nil); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Unmark removes the label from this pod (a pod that is already gone is fine).
func (p *PodLabeler) Unmark(ctx context.Context) error {
	if err := p.setLabel(ctx, p.Pod, nil); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// setLabel merge-patches LeaderLabel on pod: a string sets it, nil removes it.
func (p *PodLabeler) setLabel(ctx context.Context, pod string, value any) error {
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"labels": map[string]any{LeaderLabel: value}},
	})
	if err != nil {
		return err
	}
	if _, err := p.Client.CoreV1().Pods(p.Namespace).Patch(ctx, pod, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("label pod %s/%s: %w", p.Namespace, pod, err)
	}
	return nil
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
			OnNewLeader: func(holder string) {
				if holder != identity {
					slog.Info("ha: following leader", "leader", holder)
				}
			},
		},
	})
	if err != nil {
		return fmt.Errorf("build leader elector: %w", err)
	}
	go watchLeaderless(ctx, lock, leaderlessCheck, 2*defaultLeaseDuration, slog.Default())
	// Run returns when ctx is cancelled or leadership is lost; loop so a
	// non-leader keeps contending for the Lease for the process's lifetime.
	for {
		le.Run(ctx)
		if ctx.Err() != nil {
			return nil
		}
	}
}

// watchLeaderless warns, every `every`, while no replica holds a live Lease
// (no holder, or no renewal for longer than stale) or while this replica
// cannot read the Lease at all (e.g. the leases RBAC rule is missing). In that
// state every replica refuses mutations, which looks healthy from the outside.
func watchLeaderless(ctx context.Context, lock resourcelock.Interface, every, stale time.Duration, log *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		rec, _, err := lock.Get(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			log.Warn("ha: cannot read the leader Lease; this replica cannot become leader", "lease", LeaseName, "err", err)
		case rec.HolderIdentity == "" || time.Since(rec.RenewTime.Time) > stale:
			log.Warn("ha: no replica holds a live leader Lease; every replica refuses mutations",
				"lease", LeaseName, "holder", rec.HolderIdentity, "last_renew", rec.RenewTime.Time)
		}
	}
}
