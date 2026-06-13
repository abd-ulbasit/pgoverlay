package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/abd-ulbasit/pgbranch/internal/cow"
	"github.com/abd-ulbasit/pgbranch/internal/registry"
	"github.com/abd-ulbasit/pgbranch/internal/runtime"
)

// CSI backend: the branch's writable layer is a PVC clone of its base volume
// (driver CloneVolume — a CSI dataSource clone or snapshot restore on the
// kube driver) and the branch pod runs the direct entrypoint straight on the
// clone. No overlay, no SYS_ADMIN, no node pinning, no layer rows: every
// clone is an independent volume, mirroring the zfs shape.

func (e *Engine) csi() bool { return e.planner.Backend == cow.BackendCSI }

// provisionCSI is provision's layer half for the csi backend: clone the base
// volume (the source PVC, or — for branch-from-branch and child resets — the
// parent branch's PVC) into the branch's own PVC and run the container on it.
//
// Cloning a live parent's PVC is not crash-safe (the CSI spec leaves clones
// of in-use volumes driver-defined), so the parent is briefly stopped:
//
//	CHECKPOINT parent -> stop parent -> CloneVolume -> install entrypoint
//	(the helper is the clone's first consumer, forcing provisioning to
//	complete while the parent is down) -> restart parent (wait ready) ->
//	start child.
//
// On failure the clone is removed and the parent restarted on its own PVC; a
// parent that cannot be restarted is marked failed — its PVC is untouched
// either way, never half-cloned.
func (e *Engine) provisionCSI(ctx context.Context, b *registry.Branch, src *registry.Source) error {
	parent, err := e.csiQuiesceTarget(b)
	if err != nil {
		return err
	}
	bg := context.WithoutCancel(ctx)

	if parent != nil {
		if err := e.reg.TransitionBranch(parent.ID, registry.BranchResetting, "stop for clone to "+b.Name); err != nil {
			return err
		}
		// CHECKPOINT so the clone is a clean snapshot needing minimal WAL
		// replay. The parent container is untouched up to here, so a failure
		// leaves it ready and running.
		if err := e.drv.Exec(ctx, parent.ContainerID, psqlCmd(src, "CHECKPOINT")); err != nil {
			e.reg.TransitionBranch(parent.ID, registry.BranchReady, "clone to "+b.Name+" aborted: checkpoint failed")
			return fmt.Errorf("checkpoint parent %q: %w", parent.Name, err)
		}
		if err := e.drv.StopRemove(ctx, parent.ContainerID); err != nil {
			// container state unknown — don't guess; reconcile/destroy can clean
			e.reg.TransitionBranch(parent.ID, registry.BranchFailed, "stop for clone to "+b.Name+" failed: "+err.Error())
			return fmt.Errorf("stop parent %q: %w", parent.Name, err)
		}
	}

	var undo []func()
	fail := func(stepErr error) error {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
		if parent != nil {
			e.restartCSIBranch(bg, parent, src, stepErr)
		}
		return stepErr
	}

	// 1. clone the base PVC into the branch's own PVC
	if err := e.drv.CloneVolume(ctx, b.SourceVolume, b.RWVolume,
		e.instanceLabels(map[string]string{"pgbranch.managed": "true", "pgbranch.branch.id": b.ID})); err != nil {
		return fail(fmt.Errorf("clone volume: %w", err))
	}
	undo = append(undo, func() { e.drv.RemoveVolume(bg, b.RWVolume) })

	// 2. direct entrypoint into the clone (also its first consumer)
	if err := e.installDirectEntrypoint(ctx, b.RWVolume); err != nil {
		return fail(fmt.Errorf("install entrypoint: %w", err))
	}

	// 3. parent back up before the child starts
	if parent != nil {
		p := parent
		parent = nil // restarting now; fail() must not restart again
		if err := e.restartCSIBranch(ctx, p, src, nil); err != nil {
			return fail(fmt.Errorf("restart parent %q: %w", p.Name, err))
		}
	}

	// 4. branch container on the clone
	cid, err := e.startDirectBranch(ctx, b.Name, b.RWVolume, e.image(src.PGVersion), e.branchLabels(b))
	if err != nil {
		return fail(fmt.Errorf("start instance: %w", err))
	}
	undo = append(undo, func() { e.drv.StopRemove(bg, cid) })

	if err := e.awaitAndMark(ctx, b, src, cid); err != nil {
		return fail(err)
	}
	return nil
}

// csiQuiesceTarget resolves the live parent branch whose PVC b is about to
// clone (branch-from-branch create and child reset). nil when b clones the
// source PVC, or the parent is gone/stopped (the clone then fails on its own
// if the parent's PVC no longer exists).
func (e *Engine) csiQuiesceTarget(b *registry.Branch) (*registry.Branch, error) {
	if b.ParentBranchName == "" {
		return nil, nil
	}
	p, err := e.reg.GetBranchByName(b.ParentBranchName)
	if errors.Is(err, registry.ErrNotFound) {
		return nil, nil // parent destroyed
	}
	if err != nil {
		return nil, err
	}
	if p.RWVolume != b.SourceVolume || p.ContainerID == "" {
		return nil, nil
	}
	if p.State != registry.BranchReady {
		return nil, fmt.Errorf("parent branch %q is %s, not ready", p.Name, p.State)
	}
	return p, nil
}

// restartCSIBranch starts a stopped csi branch's pod back on its own PVC,
// waits for readiness and records the new container/address. On failure the
// branch is marked failed (cause, when non-nil, is the saga error that
// triggered the restore); its PVC — the data — is always preserved.
func (e *Engine) restartCSIBranch(ctx context.Context, b *registry.Branch, src *registry.Source, cause error) error {
	failed := func(err error) error {
		msg := "restart failed: " + err.Error()
		if cause != nil {
			msg = fmt.Sprintf("clone failed (%v); restart failed: %v", cause, err)
		}
		e.reg.TransitionBranch(b.ID, registry.BranchFailed, msg)
		return err
	}
	cid, err := e.startDirectBranch(ctx, b.Name, b.RWVolume, e.image(src.PGVersion), e.branchLabels(b))
	if err != nil {
		return failed(err)
	}
	if err := e.waitReady(ctx, cid, 90*time.Second); err != nil {
		e.drv.StopRemove(ctx, cid)
		return failed(err)
	}
	info, err := e.inspectAddr(ctx, cid)
	if err != nil {
		return failed(err)
	}
	return e.reg.MarkBranchReady(b.ID, cid, info.Host, info.Port)
}

// installDirectEntrypoint writes the direct (no-overlay) entrypoint into a
// cloned volume, next to its data/ dir (plain unprivileged helper).
func (e *Engine) installDirectEntrypoint(ctx context.Context, volume string) error {
	_, err := e.drv.RunHelper(ctx, runtime.HelperSpec{
		Image:  "alpine:3.21",
		Cmd:    []string{"sh", "-c", `printf '%s' "$PGBRANCH_ENTRYPOINT" > /pgbranch/rw/entrypoint.sh && chmod 0755 /pgbranch/rw/entrypoint.sh`},
		Env:    []string{"PGBRANCH_ENTRYPOINT=" + cow.EntrypointScriptDirect},
		Mounts: []runtime.Mount{{Volume: volume, Target: cow.RWPath}},
	})
	return err
}

// startDirectBranch starts a branch container running postgres directly on
// its writable volume mounted at RWPath (PGDATA = its data/ subdir).
func (e *Engine) startDirectBranch(ctx context.Context, name, volume, image string, labels map[string]string) (string, error) {
	return e.drv.StartBranch(ctx, runtime.BranchSpec{
		Name:       "pgbranch-br-" + name,
		Image:      image,
		Env:        []string{"PGDATA=" + cow.DirectDataPath},
		Mounts:     []runtime.Mount{{Volume: volume, Target: cow.RWPath}},
		Entrypoint: []string{"/bin/sh", cow.RWPath + "/entrypoint.sh"},
		Labels:     labels,
	})
}
