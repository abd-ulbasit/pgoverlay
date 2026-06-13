package ha

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeGate is a minimal Gate implementation for the callback tests (the real
// one lives in internal/api).
type fakeGate struct{ leader atomic.Bool }

func (g *fakeGate) Set(v bool)     { g.leader.Store(v) }
func (g *fakeGate) IsLeader() bool { return g.leader.Load() }

// OnStartedLeading flips the gate to leader=true and starts the reconcile loop
// (which runs until its context is cancelled).
func TestOnStartedLeadingFlipsGateAndStartsLoop(t *testing.T) {
	gate := &fakeGate{}
	var started, stopped atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	run := func(ctx context.Context) {
		defer wg.Done()
		started.Store(true)
		<-ctx.Done() // loop lives until leadership is lost
		stopped.Store(true)
	}
	c := NewCallbacks(gate, run)

	ctx, cancel := context.WithCancel(context.Background())
	go c.OnStartedLeading(ctx)

	waitFor(t, func() bool { return started.Load() }, "reconcile loop to start")
	if !gate.IsLeader() {
		t.Fatal("OnStartedLeading did not set the gate to leader=true")
	}

	// Losing the leadership context cancels the loop.
	cancel()
	wg.Wait()
	if !stopped.Load() {
		t.Fatal("reconcile loop did not stop after its context was cancelled")
	}
}

// OnStoppedLeading flips the gate closed (so the API mutating gate rejects).
func TestOnStoppedLeadingFlipsGateClosed(t *testing.T) {
	gate := &fakeGate{}
	gate.Set(true)
	c := NewCallbacks(gate, func(ctx context.Context) { <-ctx.Done() })

	c.OnStoppedLeading()
	if gate.IsLeader() {
		t.Fatal("OnStoppedLeading did not set the gate to leader=false")
	}
}

// Gaining then losing leadership flips the gate both ways and starts/stops the
// loop exactly once per cycle — the failover contract.
func TestLeadershipCycle(t *testing.T) {
	gate := &fakeGate{}
	var runs atomic.Int32
	c := NewCallbacks(gate, func(ctx context.Context) {
		runs.Add(1)
		<-ctx.Done()
	})

	ctx, cancel := context.WithCancel(context.Background())
	go c.OnStartedLeading(ctx)
	waitFor(t, func() bool { return runs.Load() == 1 && gate.IsLeader() }, "first leadership term")

	cancel()             // lose the term's context
	c.OnStoppedLeading() // election library calls this on stop
	waitFor(t, func() bool { return !gate.IsLeader() }, "gate to close after losing leadership")

	// regain
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go c.OnStartedLeading(ctx2)
	waitFor(t, func() bool { return runs.Load() == 2 && gate.IsLeader() }, "second leadership term")
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
