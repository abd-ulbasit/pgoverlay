package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/abd-ulbasit/pgoverlay/internal/cow"
	"github.com/abd-ulbasit/pgoverlay/internal/registry"
	"github.com/abd-ulbasit/pgoverlay/internal/runtime"
)

// ErrNotRecoverable reports a recover request that the branch's state or its
// data cannot satisfy (not failed, or its volumes are gone). The API maps it
// to 409; ResetBranch or DestroyBranch are the ways forward then.
var ErrNotRecoverable = errors.New("branch not recoverable")

// RecoverBranch restarts a failed branch on the data it still has: a new
// container over its recorded writable layer and base (source volume plus
// frozen layer chain), with no re-clone, no masking and no credential
// rotation — the data is already masked and carries the branch's password
// (failed -> resetting -> ready).
//
// This is the way back for a branch that failed with its data intact: a
// freeze or clone parent interrupted by a crash, restart or cancelled request
// (reconcile fails such a parent but keeps its volume), or a branch whose
// container was lost. It refuses, changing nothing, when the branch is not
// failed, when a child is still being created from it, or when its writable
// layer or base volumes are gone (reset or destroy it then). A failed restart
// removes the new container and returns the branch to failed; the data is
// never touched.
func (e *Engine) RecoverBranch(ctx context.Context, name string) (_ *registry.Branch, err error) {
	defer e.observeOp("recover", &err)()
	b, err := e.reg.GetBranchByName(name)
	if err != nil {
		return nil, err
	}
	if b.State != registry.BranchFailed {
		return nil, fmt.Errorf("%w: branch %q is %s; only a failed branch can be recovered", ErrNotRecoverable, name, b.State)
	}
	src, err := e.reg.GetSourceByID(b.SourceID)
	if err != nil {
		return nil, err
	}
	if err := e.checkChildrenAllowReprovision(b); err != nil {
		return nil, err
	}
	chain, err := e.reg.LayerChain(b.ID)
	if err != nil {
		return nil, err
	}
	if err := e.checkBranchData(ctx, b, chain); err != nil {
		return nil, err
	}
	if err := e.reg.TransitionBranchCtx(ctx, b.ID, registry.BranchResetting, "recover requested: restart on existing data"); err != nil {
		return nil, err
	}
	defer e.keepAlive(b.ID)()
	bg := context.WithoutCancel(ctx)
	fail := func(stepErr error) (*registry.Branch, error) {
		e.logCompensationErr("transition", "recover: mark branch failed after recover failed",
			e.reg.TransitionBranchCtx(bg, b.ID, registry.BranchFailed, "recover failed: "+failureReason(stepErr)),
			"branch", b.Name, "branch_id", b.ID)
		return nil, stepErr
	}
	// the recorded container may still exist (e.g. a restore that started it
	// but could not mark the branch ready): it must go before a same-named
	// container can start
	if b.ContainerID != "" {
		if err := e.drv.StopRemove(bg, b.ContainerID); err != nil {
			return fail(fmt.Errorf("remove old container: %w", err))
		}
	}
	if !e.zfs() && !e.csi() {
		// rewrite the overlay entrypoint (the current version); its mkdir -p
		// of upper/work is a no-op on intact data
		if err := e.installOverlayEntrypoint(ctx, b.RWVolume); err != nil {
			return fail(fmt.Errorf("install entrypoint: %w", err))
		}
	}
	if err := e.restartAndMarkReady(ctx, b, src, chain); err != nil {
		return fail(err)
	}
	return e.reg.GetBranchByName(name)
}

// checkBranchData verifies that the volumes a restart on existing data would
// mount still exist, so a recover never boots an empty auto-created volume
// (docker named volumes and hostPath dirs are created on first mount) and
// pretends the data came back. Overlay: the rw volume, every frozen layer and
// the source volume; csi: the branch's PVC; zfs: the clone dataset.
func (e *Engine) checkBranchData(ctx context.Context, b *registry.Branch, chain []registry.Layer) error {
	if e.zfs() {
		if err := e.runZFS(ctx, zfsHelperSpec(e.planner.ZFSUsed(b.RWVolume))); err != nil {
			return fmt.Errorf("%w: branch %q: its writable dataset %s is not available (%v); reset or destroy it", ErrNotRecoverable, b.Name, b.RWVolume, err)
		}
		return nil
	}
	need := []string{b.RWVolume}
	if !e.csi() {
		need = append(need, layerVolumes(chain)...)
		need = append(need, b.SourceVolume)
	}
	vols, err := e.drv.ListManagedVolumes(ctx, e.reg.InstanceID())
	if err != nil {
		return fmt.Errorf("list volumes: %w", err)
	}
	have := make(map[string]bool, len(vols))
	for _, v := range vols {
		have[v.Name] = true
	}
	var missing []string
	for _, v := range need {
		if !have[v] {
			missing = append(missing, v)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: branch %q: volume(s) %s not found among this instance's managed volumes; reset or destroy it",
			ErrNotRecoverable, b.Name, strings.Join(missing, ", "))
	}
	return nil
}

// restartOnOwnData starts a branch's container on the data its row records —
// its writable layer over its base — records it on the row, waits for
// readiness and returns the container and its address. Nothing is re-cloned,
// masked or rotated. On failure the new container is removed; the data is
// never touched. The caller owns the state transitions.
func (e *Engine) restartOnOwnData(ctx context.Context, b *registry.Branch, src *registry.Source, chain []registry.Layer) (string, runtime.ContainerInfo, error) {
	image := e.image(src)
	var cid string
	var err error
	switch {
	case e.zfs():
		cid, err = e.startZFSBranch(ctx, b, image)
	case e.csi():
		cid, err = e.startDirectBranch(ctx, b.Name, b.RWVolume, image, e.branchLabels(b))
	default:
		plan := cow.PlanBranch(b.RWVolume, b.SourceVolume, layerVolumes(chain))
		cid, err = e.startOverlayBranch(ctx, b.Name, plan, image, e.branchLabels(b))
	}
	if err != nil {
		return "", runtime.ContainerInfo{}, fmt.Errorf("start instance: %w", err)
	}
	bg := context.WithoutCancel(ctx)
	cleanup := func() {
		e.logCompensationErr("undo", "restart: stop/remove container after restart failed",
			e.drv.StopRemove(bg, cid), "branch", b.Name, "container", cid)
	}
	// own the in-flight container before the readiness wait (reconcile-safe)
	e.logCompensationErr("transition", "restart: own restarted container before readiness wait",
		e.reg.SetBranchContainer(b.ID, cid), "branch", b.Name, "container", cid)
	if err := e.waitReady(ctx, cid, 90*time.Second); err != nil {
		cleanup()
		return "", runtime.ContainerInfo{}, fmt.Errorf("instance never became ready: %w", err)
	}
	info, err := e.inspectAddr(ctx, cid)
	if err != nil {
		cleanup()
		return "", runtime.ContainerInfo{}, err
	}
	return cid, info, nil
}

// restartAndMarkReady is restartOnOwnData followed by the transition to
// ready. If the transition fails (the row moved meanwhile, e.g. reconcile
// failed it) the new container is removed again.
func (e *Engine) restartAndMarkReady(ctx context.Context, b *registry.Branch, src *registry.Source, chain []registry.Layer) error {
	cid, info, err := e.restartOnOwnData(ctx, b, src, chain)
	if err != nil {
		return err
	}
	if err := e.reg.MarkBranchReadyCtx(ctx, b.ID, cid, info.Host, info.Port); err != nil {
		e.logCompensationErr("undo", "restart: stop/remove container after mark ready failed",
			e.drv.StopRemove(context.WithoutCancel(ctx), cid), "branch", b.Name, "container", cid)
		return fmt.Errorf("mark ready: %w", err)
	}
	return nil
}
