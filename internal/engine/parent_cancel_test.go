package engine

import (
	"context"
	"testing"

	"github.com/abd-ulbasit/pgoverlay/internal/registry"
)

// cancelDriver cancels the request context while the parent is being stopped
// (a client disconnect or ghook deadline mid-freeze) and, like the docker
// driver, fails StopRemove and Exec calls made on a cancelled context.
type cancelDriver struct {
	*fakeDriver
	cancelOnStop string // container id whose StopRemove triggers cancel
	cancel       context.CancelFunc
}

func (d *cancelDriver) StopRemove(ctx context.Context, id string) error {
	if id == d.cancelOnStop && d.cancel != nil {
		d.cancel()
		d.cancel = nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return d.fakeDriver.StopRemove(ctx, id)
}

func (d *cancelDriver) Exec(ctx context.Context, id string, cmd []string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return d.fakeDriver.Exec(ctx, id, cmd)
}

// LIFECYCLE-05: cancelling a branch-from-branch request while the parent is
// stopped must abort the child, not strand the parent failed: the parent's
// stop and restore run detached from the request.
func TestFreezeCancelledRequestKeepsParentAvailable(t *testing.T) {
	d := &cancelDriver{fakeDriver: newFake(), cancelOnStop: "cid-pgoverlay-br-p"}
	e, r := testEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "p", "main", 0); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.cancel = cancel
	if _, err := e.CreateBranchFrom(ctx, "c", "p", 0); err == nil {
		t.Fatal("want the cancelled child create to fail")
	}
	p, _ := r.GetBranchByName("p")
	if p.State != registry.BranchReady {
		t.Fatalf("parent state=%s after a cancelled child create, want ready (restored)", p.State)
	}
	if p.RWVolume != "pgoverlay-br-p-rw" || !d.volumes["pgoverlay-br-p-rw"] || !d.containers[p.ContainerID] {
		t.Fatalf("parent not restored on its own data: %+v", p)
	}
	if c, _ := r.GetBranchByName("c"); c.State != registry.BranchFailed {
		t.Fatalf("child state=%s want failed", c.State)
	}
}

// The csi clone quiesce: same guarantee — the parent's stop and restart are
// detached, so a cancelled child create leaves the parent ready.
func TestCSICloneCancelledRequestKeepsParentAvailable(t *testing.T) {
	d := &cancelDriver{fakeDriver: newFake(), cancelOnStop: "cid-pgoverlay-br-pr-1"}
	e, r := csiEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.cancel = cancel
	if _, err := e.CreateBranchFrom(ctx, "pr-2", "pr-1", 0); err == nil {
		t.Fatal("want the cancelled child create to fail")
	}
	p, _ := r.GetBranchByName("pr-1")
	if p.State != registry.BranchReady || !d.containers[p.ContainerID] {
		t.Fatalf("parent after a cancelled child create: %+v, want ready and running", p)
	}
	if !d.volumes["pgoverlay-br-pr-1-rw"] {
		t.Fatal("parent PVC removed")
	}
}
