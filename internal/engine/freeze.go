package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/abd-ulbasit/pgbranch/internal/cow"
	"github.com/abd-ulbasit/pgbranch/internal/registry"
)

// CreateBranchFrom creates a branch whose base is another (ready) branch's
// current state — branch-from-branch.
//
// Overlay backend: a freeze saga. The parent's rw volume cannot be shared
// writable, so it is frozen into an immutable layer:
//
//	CHECKPOINT parent -> stop parent -> fresh parent rw volume ->
//	restart parent on [frozen rw, …its old chain…, source] (wait ready) ->
//	start child on the same chain -> commit (layer row + parent rw swap,
//	atomic) -> child ready.
//
// The parent gets a new container (and so possibly a new host port; the wire
// router resolves live, so dbname@parent connections just reconnect). On any
// failure before the commit the parent is restored to its original rw volume
// and chain and restarted; if even that fails it is marked failed — never
// half-frozen. The layer row is committed only after both restarts succeeded.
//
// ZFS backend: block-level CoW — snapshot the parent's clone and clone that.
// No freeze, no stop, no layer rows.
//
// CSI backend: the child's PVC is a clone of the parent's PVC. No freeze or
// layer rows either, but the parent is briefly stopped around the clone for
// crash consistency (see provisionCSI).
func (e *Engine) CreateBranchFrom(ctx context.Context, name, parentName string, ttl time.Duration) (_ *registry.Branch, err error) {
	defer e.observeOp("from_branch", &err)()
	if err := validateBranchName(name); err != nil {
		return nil, err
	}
	if err := e.checkQuota(); err != nil {
		return nil, err
	}
	parent, err := e.reg.GetBranchByName(parentName)
	if err != nil {
		return nil, fmt.Errorf("parent branch %q: %w", parentName, err)
	}
	if parent.State != registry.BranchReady {
		return nil, fmt.Errorf("parent branch %q is %s, not ready", parentName, parent.State)
	}
	src, err := e.reg.GetSourceByID(parent.SourceID)
	if err != nil {
		return nil, err
	}
	expiresAt := e.expiresAtFor(ttl)
	child := &registry.Branch{
		Name: name, SourceID: parent.SourceID, RWVolume: e.planner.BranchLayerName(name),
		SourceVolume: parent.SourceVolume, ExpiresAt: expiresAt, ParentBranchName: parentName,
	}
	if e.zfs() || e.csi() {
		// The child clones the parent's writable layer (zfs: snapshot+clone
		// of the clone dataset; csi: PVC clone). Recording it as the child's
		// SourceVolume makes provisioning, reset (re-clone from the parent)
		// and destroy all operate on it naturally.
		child.SourceVolume = parent.RWVolume
	}
	if err := e.reg.CreateBranchCtx(ctx, child); err != nil {
		return nil, err
	}
	provision := func() error { return e.freezeAndProvision(ctx, child, parent, src) }
	if e.zfs() {
		provision = func() error { return e.provisionZFS(ctx, child, src) }
	}
	if e.csi() {
		provision = func() error { return e.provisionCSI(ctx, child, src) }
	}
	if err := provision(); err != nil {
		e.logCompensationErr("transition", "from_branch: mark child failed after provision failed",
			e.reg.TransitionBranchCtx(ctx, child.ID, registry.BranchFailed, err.Error()), "branch", child.Name, "branch_id", child.ID)
		return nil, err
	}
	return e.reg.GetBranchByName(name)
}

// freezeAndProvision is the overlay freeze saga (see CreateBranchFrom).
// Compensations run in reverse on failure; after them the parent is restored
// onto its original rw volume + chain (or marked failed). All registry
// effects — the layer row, the parent's rw swap and ready transition, the
// child's base layer — commit atomically at the end via CommitFreeze.
func (e *Engine) freezeAndProvision(ctx context.Context, child, parent *registry.Branch, src *registry.Source) error {
	chain, err := e.reg.LayerChain(parent.ID)
	if err != nil {
		return err
	}
	origPlan := cow.PlanBranch(parent.RWVolume, parent.SourceVolume, layerVolumes(chain))
	// the parent's current rw volume becomes the newest frozen layer
	frozen := append([]string{parent.RWVolume}, layerVolumes(chain)...)
	newRW := cow.BranchRWVolumeNameGen(parent.Name, len(chain)+2)
	parentPlan := cow.PlanBranch(newRW, parent.SourceVolume, frozen)
	childPlan := cow.PlanBranch(child.RWVolume, child.SourceVolume, frozen)
	image := e.image(src.PGVersion)

	if err := e.reg.TransitionBranchCtx(ctx, parent.ID, registry.BranchResetting, "freeze for child "+child.Name); err != nil {
		return err
	}
	bg := context.WithoutCancel(ctx)

	// 1. CHECKPOINT the parent so the frozen layer is a clean snapshot
	// needing minimal WAL replay (in-container psql, no password needed).
	if err := e.drv.Exec(ctx, parent.ContainerID, psqlCmd(src, "CHECKPOINT")); err != nil {
		e.logCompensationErr("transition", "freeze: restore parent to ready after checkpoint failed",
			e.reg.TransitionBranchCtx(ctx, parent.ID, registry.BranchReady, "freeze for child "+child.Name+" aborted: checkpoint failed"),
			"branch", parent.Name, "branch_id", parent.ID)
		return fmt.Errorf("checkpoint parent %q: %w", parent.Name, err)
	}

	// 2. stop the parent: its rw volume must not change while it becomes a
	// layer. The parent container is untouched up to here, so a checkpoint
	// failure above leaves it ready and running.
	if err := e.drv.StopRemove(ctx, parent.ContainerID); err != nil {
		// container state unknown — don't guess; reconcile/destroy can clean
		e.logCompensationErr("transition", "freeze: mark parent failed after stop parent failed",
			e.reg.TransitionBranchCtx(ctx, parent.ID, registry.BranchFailed, "freeze for child "+child.Name+": stop parent failed: "+err.Error()),
			"branch", parent.Name, "branch_id", parent.ID)
		return fmt.Errorf("stop parent %q: %w", parent.Name, err)
	}

	var undo []func()
	fail := func(stepErr error) error {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
		e.restoreParent(bg, parent, src, origPlan, stepErr)
		return stepErr
	}

	// 3. fresh rw volume for the parent (the swap), with the entrypoint
	if err := e.drv.CreateVolume(ctx, newRW, e.instanceLabels(map[string]string{"pgbranch.managed": "true", "pgbranch.branch.id": parent.ID})); err != nil {
		return fail(fmt.Errorf("create parent rw volume: %w", err))
	}
	undo = append(undo, func() {
		e.logCompensationErr("undo", "freeze: remove parent rw volume", e.drv.RemoveVolume(bg, newRW),
			"branch", parent.Name, "volume", newRW)
	})
	if err := e.installOverlayEntrypoint(ctx, newRW); err != nil {
		return fail(fmt.Errorf("install parent entrypoint: %w", err))
	}

	// 4. restart the parent over the frozen chain and wait for it: the
	// parent must come back before the child starts
	parentCID, err := e.startOverlayBranch(ctx, parent.Name, parentPlan, image, e.branchLabels(parent))
	if err != nil {
		return fail(fmt.Errorf("restart parent %q: %w", parent.Name, err))
	}
	undo = append(undo, func() {
		e.logCompensationErr("undo", "freeze: stop/remove restarted parent container", e.drv.StopRemove(bg, parentCID),
			"branch", parent.Name, "container", parentCID)
	})
	e.logCompensationErr("transition", "freeze: own restarted parent container before readiness wait",
		e.reg.SetBranchContainer(parent.ID, parentCID), "branch", parent.Name, "container", parentCID)
	if err := e.waitReady(ctx, parentCID, 90*time.Second); err != nil {
		return fail(fmt.Errorf("parent %q never became ready after freeze: %w", parent.Name, err))
	}
	// freeze checkpoint: the parent is back up. Bump both rows' stuck-timer so a
	// slow child start below does not make either look abandoned to reconcile.
	e.logCompensationErr("transition", "freeze: touch parent stuck-timer", e.reg.TouchBranch(parent.ID),
		"branch", parent.Name, "branch_id", parent.ID)
	e.logCompensationErr("transition", "freeze: touch child stuck-timer", e.reg.TouchBranch(child.ID),
		"branch", child.Name, "branch_id", child.ID)

	// 5. child resources over the same chain
	if err := e.drv.CreateVolume(ctx, childPlan.RWVolume, e.instanceLabels(map[string]string{"pgbranch.managed": "true", "pgbranch.branch.id": child.ID})); err != nil {
		return fail(fmt.Errorf("create rw volume: %w", err))
	}
	undo = append(undo, func() {
		e.logCompensationErr("undo", "freeze: remove child rw volume", e.drv.RemoveVolume(bg, childPlan.RWVolume),
			"branch", child.Name, "volume", childPlan.RWVolume)
	})
	if err := e.installOverlayEntrypoint(ctx, childPlan.RWVolume); err != nil {
		return fail(fmt.Errorf("install entrypoint: %w", err))
	}
	childCID, err := e.startOverlayBranch(ctx, child.Name, childPlan, image, e.branchLabels(child))
	if err != nil {
		return fail(fmt.Errorf("start instance: %w", err))
	}
	undo = append(undo, func() {
		e.logCompensationErr("undo", "freeze: stop/remove child container", e.drv.StopRemove(bg, childCID),
			"branch", child.Name, "container", childCID)
	})
	e.logCompensationErr("transition", "freeze: own child container before readiness wait",
		e.reg.SetBranchContainer(child.ID, childCID), "branch", child.Name, "container", childCID)
	if err := e.waitReady(ctx, childCID, 90*time.Second); err != nil {
		return fail(fmt.Errorf("instance never became ready: %w", err))
	}
	// masking runs on every branch create; the data lineage is already
	// masked (the parent was), so scripts see their own prior output —
	// the documented contract is that mask scripts are idempotent
	if err := e.applyMasking(ctx, childCID, src); err != nil {
		return fail(err)
	}
	// the child gets its own credentials (rotation mode); the parent's
	// restart above deliberately does not re-rotate — its data carries its
	// existing password
	if err := e.rotateBranchCredentials(ctx, childCID, child, src); err != nil {
		return fail(err)
	}
	childInfo, err := e.inspectAddr(ctx, childCID)
	if err != nil {
		return fail(err)
	}
	parentInfo, err := e.inspectAddr(ctx, parentCID)
	if err != nil {
		return fail(err)
	}

	// 6. commit: the old rw volume becomes a layer, the parent swaps onto the
	// fresh one (resetting -> ready, new container/port), the child bases on
	// the new layer — one transaction
	if _, err := e.reg.CommitFreezeCtx(ctx, parent.ID, child.ID, parent.RWVolume, newRW,
		parentCID, parentInfo.Host, parentInfo.Port, "freeze for child "+child.Name+" complete"); err != nil {
		return fail(fmt.Errorf("commit freeze: %w", err))
	}

	// 7. child ready (post-commit registry failures leave the child failed
	// for destroy/reconcile; the freeze itself is already consistent)
	return e.reg.MarkBranchReadyCtx(ctx, child.ID, childCID, childInfo.Host, childInfo.Port)
}

// restoreParent puts a parent back on its original rw volume and chain after
// a failed freeze (the compensations already removed the fresh rw volume and
// any new containers). If the restoration restart itself fails the parent is
// marked failed — its data (the original rw volume) is always preserved.
func (e *Engine) restoreParent(ctx context.Context, parent *registry.Branch, src *registry.Source, origPlan cow.Plan, cause error) {
	failed := func(err error) {
		e.logCompensationErr("transition", "restoreParent: mark parent failed after restore failed",
			e.reg.TransitionBranchCtx(ctx, parent.ID, registry.BranchFailed,
				fmt.Sprintf("freeze failed (%v); parent restore failed: %v", cause, err)),
			"branch", parent.Name, "branch_id", parent.ID)
	}
	cid, err := e.startOverlayBranch(ctx, parent.Name, origPlan, e.image(src.PGVersion), e.branchLabels(parent))
	if err == nil {
		e.logCompensationErr("transition", "restoreParent: own restored parent container before readiness wait",
			e.reg.SetBranchContainer(parent.ID, cid), "branch", parent.Name, "container", cid) // own the in-flight container before the readiness wait
	}
	if err != nil {
		failed(err)
		return
	}
	if err := e.waitReady(ctx, cid, 90*time.Second); err != nil {
		e.logCompensationErr("undo", "restoreParent: stop/remove parent container after readiness wait failed",
			e.drv.StopRemove(ctx, cid), "branch", parent.Name, "container", cid)
		failed(err)
		return
	}
	info, err := e.inspectAddr(ctx, cid)
	if err != nil {
		failed(err)
		return
	}
	e.logCompensationErr("transition", "restoreParent: mark parent ready after restore",
		e.reg.MarkBranchReadyCtx(ctx, parent.ID, cid, info.Host, info.Port), "branch", parent.Name, "branch_id", parent.ID)
}
