package engine

import (
	"context"
	"errors"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/abd-ulbasit/pgoverlay/internal/registry"
)

// flakyDriver wraps the fake driver with scripted failures for the teardown
// steps, and can make StopRemove honour context cancellation the way the
// docker driver does (ContainerRemove returns "context canceled").
type flakyDriver struct {
	*fakeDriver
	stopErrs  []error // returned by successive StopRemove calls (nil entries succeed)
	rmVolErrs []error // returned by successive RemoveVolume calls
	onRmVol   func()  // runs inside RemoveVolume (race injection)
	ctxAware  bool    // StopRemove fails with ctx.Err() once ctx is done
}

func (f *flakyDriver) StopRemove(ctx context.Context, id string) error {
	if f.ctxAware && ctx.Err() != nil {
		return ctx.Err()
	}
	if len(f.stopErrs) > 0 {
		err := f.stopErrs[0]
		f.stopErrs = f.stopErrs[1:]
		if err != nil {
			return err
		}
	}
	return f.fakeDriver.StopRemove(ctx, id)
}

func (f *flakyDriver) RemoveVolume(ctx context.Context, name string) error {
	if f.onRmVol != nil {
		f.onRmVol()
	}
	if len(f.rmVolErrs) > 0 {
		err := f.rmVolErrs[0]
		f.rmVolErrs = f.rmVolErrs[1:]
		if err != nil {
			return err
		}
	}
	return f.fakeDriver.RemoveVolume(ctx, name)
}

func lastTransition(t *testing.T, r *registry.Registry, name string) registry.Transition {
	t.Helper()
	h, err := r.BranchHistory(name)
	if err != nil {
		t.Fatal(err)
	}
	return h[len(h)-1]
}

// #10: a destroy whose teardown fails leaves the row in destroying with the
// cause journaled, and a second destroy finishes the job instead of being
// refused with "illegal branch transition destroying -> destroying".
func TestDestroyRetriesFromDestroying(t *testing.T) {
	for _, tc := range []struct {
		name string
		arm  func(*flakyDriver)
		want string
	}{
		{"container removal fails", func(f *flakyDriver) { f.stopErrs = []error{errors.New("daemon hiccup")} }, "remove container: daemon hiccup"},
		{"volume removal fails", func(f *flakyDriver) { f.rmVolErrs = []error{errors.New("volume is in use")} }, "remove branch layer: volume is in use"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &flakyDriver{fakeDriver: newFake()}
			e, r := testEngine(t, d)
			readySource(t, r)
			ctx := context.Background()
			if _, err := e.CreateBranch(ctx, "pr-1", "main", 0); err != nil {
				t.Fatal(err)
			}
			tc.arm(d)
			err := e.DestroyBranch(ctx, "pr-1")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("first destroy err=%v, want %q", err, tc.want)
			}
			b, err := r.GetBranchByName("pr-1")
			if err != nil || b.State != registry.BranchDestroying {
				t.Fatalf("after failed destroy: %+v err=%v, want destroying", b, err)
			}
			last := lastTransition(t, r, "pr-1")
			if last.FromState != "destroying" || last.ToState != "destroying" || !strings.Contains(last.Reason, tc.want) {
				t.Fatalf("failure not journaled: %+v", last)
			}

			if err := e.DestroyBranch(ctx, "pr-1"); err != nil {
				t.Fatalf("retry destroy = %v, want nil", err)
			}
			if _, err := r.GetBranchByName("pr-1"); !errors.Is(err, registry.ErrNotFound) {
				t.Fatalf("branch still live after retry: %v", err)
			}
			if len(d.containers) != 0 || d.volumes["pgoverlay-br-pr-1-rw"] {
				t.Fatalf("leaked: containers=%v volumes=%v", d.containers, d.volumes)
			}
			// the name and the quota slot are free again
			if _, err := e.CreateBranch(ctx, "pr-1", "main", 0); err != nil {
				t.Fatalf("recreate after retried destroy: %v", err)
			}
		})
	}
}

// A failed teardown is a *DestroyError carrying the journaled reason and a
// classification of the cause, so the API can answer 409 or 502 instead of a
// bare 500 (issue #10).
func TestDestroyFailureIsClassified(t *testing.T) {
	for _, tc := range []struct {
		name               string
		arm                func(*flakyDriver)
		inUse, unavailable bool
	}{
		{"volume in use", func(f *flakyDriver) {
			f.rmVolErrs = []error{errors.New("Error response from daemon: remove pgoverlay-br-pr-1-rw: volume is in use - [766ac31a4b2c]")}
		}, true, false},
		{"runtime unreachable", func(f *flakyDriver) {
			f.stopErrs = []error{&net.OpError{Op: "dial", Net: "unix", Err: syscall.ECONNREFUSED}}
		}, false, true},
		{"other", func(f *flakyDriver) { f.rmVolErrs = []error{errors.New("driver failed")} }, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &flakyDriver{fakeDriver: newFake()}
			e, r := testEngine(t, d)
			readySource(t, r)
			ctx := context.Background()
			if _, err := e.CreateBranch(ctx, "pr-1", "main", 0); err != nil {
				t.Fatal(err)
			}
			tc.arm(d)
			err := e.DestroyBranch(ctx, "pr-1")
			var de *DestroyError
			if !errors.As(err, &de) {
				t.Fatalf("DestroyBranch = %v (%T), want a *DestroyError", err, err)
			}
			if de.Branch != "pr-1" || de.InUse != tc.inUse || de.RuntimeUnavailable != tc.unavailable {
				t.Fatalf("DestroyError = %+v, want in use %v, unavailable %v", de, tc.inUse, tc.unavailable)
			}
			if last := lastTransition(t, r, "pr-1"); last.Reason != "destroy failed, destroy again to retry: "+de.Reason {
				t.Fatalf("journaled %q, error reason %q: they must be the same cause", last.Reason, de.Reason)
			}
			if de.Error() != de.Err.Error() || !errors.Is(err, de.Err) {
				t.Fatalf("DestroyError must read and unwrap as the teardown error: %q vs %q", de.Error(), de.Err)
			}
		})
	}
}

// A destroying row keeps its source busy (RemoveSource refuses), and the
// retry is what frees it — no SQLite surgery.
func TestDestroyRetryUnblocksSourceRemoval(t *testing.T) {
	d := &flakyDriver{fakeDriver: newFake()}
	e, r := testEngine(t, d)
	readySource(t, r)
	ctx := context.Background()
	if _, err := e.CreateBranch(ctx, "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}
	d.rmVolErrs = []error{errors.New("volume is in use")}
	if err := e.DestroyBranch(ctx, "pr-1"); err == nil {
		t.Fatal("want first destroy to fail")
	}
	if err := e.RemoveSource(ctx, "main"); err == nil || !strings.Contains(err.Error(), "live branch") {
		t.Fatalf("RemoveSource with a destroying branch = %v, want refusal", err)
	}
	if err := e.DestroyBranch(ctx, "pr-1"); err != nil {
		t.Fatal(err)
	}
	if err := e.RemoveSource(ctx, "main"); err != nil {
		t.Fatalf("RemoveSource after retry: %v", err)
	}
}

// The teardown runs detached from the caller's context: a client that goes
// away mid-destroy (or a ghook deadline, or shutdown) must not abandon the
// teardown half-done.
func TestDestroySurvivesCancelledContext(t *testing.T) {
	d := &flakyDriver{fakeDriver: newFake(), ctxAware: true}
	e, r := testEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := e.DestroyBranch(ctx, "pr-1"); err != nil {
		t.Fatalf("destroy with a cancelled request context = %v, want nil", err)
	}
	if _, err := r.GetBranchByName("pr-1"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("branch not destroyed: %v", err)
	}
}

// Two destroys of one row racing (a user retry and reconcile): the loser of
// the final compare-and-swap sees the row already destroyed and succeeds.
func TestDestroyRaceLoserSucceeds(t *testing.T) {
	d := &flakyDriver{fakeDriver: newFake()}
	e, r := testEngine(t, d)
	readySource(t, r)
	ctx := context.Background()
	b, err := e.CreateBranch(ctx, "pr-1", "main", 0)
	if err != nil {
		t.Fatal(err)
	}
	d.onRmVol = func() {
		d.onRmVol = nil
		// the concurrent destroy finishes while this one is mid-teardown
		if err := r.TransitionBranch(b.ID, registry.BranchDestroyed, "the other destroy"); err != nil {
			t.Errorf("simulated concurrent destroy: %v", err)
		}
	}
	if err := e.DestroyBranch(ctx, "pr-1"); err != nil {
		t.Fatalf("destroy that lost the final CAS to a concurrent destroy = %v, want nil", err)
	}
}

// ListStuckDestroyingBranches is the hook for a periodic retry (reconcile): a row
// left in destroying shows up once it is older than the cutoff, and a
// DestroyBranch on it completes the destroy.
func TestStuckDestroyingIsListedAndRetryable(t *testing.T) {
	d := &flakyDriver{fakeDriver: newFake()}
	e, r := testEngine(t, d)
	readySource(t, r)
	ctx := context.Background()
	if _, err := e.CreateBranch(ctx, "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}
	d.stopErrs = []error{errors.New("boom")}
	if err := e.DestroyBranch(ctx, "pr-1"); err == nil {
		t.Fatal("want first destroy to fail")
	}
	stuck, err := r.ListStuckDestroyingBranches(time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	if err != nil || len(stuck) != 1 || stuck[0].Name != "pr-1" {
		t.Fatalf("ListStuckDestroyingBranches=%v err=%v", stuck, err)
	}
	if err := e.DestroyBranch(ctx, stuck[0].Name); err != nil {
		t.Fatal(err)
	}
	if stuck, _ := r.ListStuckDestroyingBranches(time.Now().Add(time.Hour).UTC().Format(time.RFC3339)); len(stuck) != 0 {
		t.Fatalf("still listed after retry: %v", stuck)
	}
}

// A retried destroy of a freeze parent forced out of resetting keeps the
// parent's rw volume while an uncommitted child still names it, exactly like
// the first attempt: the retry never deletes data the first attempt kept.
func TestDestroyRetryKeepsReferencedParentVolume(t *testing.T) {
	d := &flakyDriver{fakeDriver: newFake()}
	e, r := testEngine(t, d)
	readySource(t, r)
	srcID := mustSource(t, r).ID
	parent := &registry.Branch{Name: "parent", SourceID: srcID, RWVolume: "pgoverlay-br-parent-rw", SourceVolume: "pgoverlay-src-main"}
	if err := r.CreateBranch(parent); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkBranchReady(parent.ID, "cid-parent", "127.0.0.1", 1); err != nil {
		t.Fatal(err)
	}
	if err := r.TransitionBranch(parent.ID, registry.BranchResetting, "freeze for child child"); err != nil {
		t.Fatal(err)
	}
	d.volumes["pgoverlay-br-parent-rw"] = true
	child := &registry.Branch{Name: "child", SourceID: srcID, RWVolume: "pgoverlay-br-child-rw",
		SourceVolume: "pgoverlay-src-main", ParentBranchName: "parent"}
	if err := r.CreateBranch(child); err != nil {
		t.Fatal(err)
	}
	d.stopErrs = []error{errors.New("boom")}
	if err := e.DestroyBranch(context.Background(), "parent"); err == nil {
		t.Fatal("want first destroy to fail")
	}
	if err := e.DestroyBranch(context.Background(), "parent"); err != nil {
		t.Fatal(err)
	}
	if !d.volumes["pgoverlay-br-parent-rw"] {
		t.Fatal("retried destroy removed a parent rw volume an in-flight child still references")
	}
}
