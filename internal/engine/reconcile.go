package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/abd-ulbasit/pgbranch/internal/registry"
)

// ActionKind enumerates the convergence steps a reconcile pass can take. The
// values double as the {action} label on pgbranch_reconcile_actions_total.
type ActionKind string

const (
	// ActionReap destroys a branch whose TTL has passed.
	ActionReap ActionKind = "reap"
	// ActionFailStuck fails a branch wedged in creating/resetting past the
	// stuck timeout and cleans its half-built resources.
	ActionFailStuck ActionKind = "fail_stuck"
	// ActionRemoveOrphanContainer removes a managed container/pod with no live
	// registry row.
	ActionRemoveOrphanContainer ActionKind = "remove_orphan_container"
	// ActionGCLayer removes a frozen layer (volume + row) whose refcount is 0.
	ActionGCLayer ActionKind = "gc_layer"
	// ActionGCVolume removes a managed volume owned by no live branch/source.
	ActionGCVolume ActionKind = "gc_volume"
)

// Action is one intended convergence step. Target is the branch name, container
// id, layer volume or volume name the action operates on; Reason is a
// human-readable justification. ReconcilePlan is a list of these.
type Action struct {
	Kind   ActionKind `json:"kind"`
	Target string     `json:"target"`
	Reason string     `json:"reason"`
}

// ReconcilePlan is the set of convergence steps a pass intends (or, after
// apply, took). It is computed read-only and can be reported (pgb doctor /
// GET /v1/reconcile/plan) or applied (pgb gc / POST /v1/reconcile). Drift
// reports true when the plan is non-empty.
type ReconcilePlan struct {
	Actions []Action `json:"actions"`
}

// Drift reports whether the plan found anything to converge.
func (p ReconcilePlan) Drift() bool { return len(p.Actions) > 0 }

func (p *ReconcilePlan) add(kind ActionKind, target, reason string) {
	p.Actions = append(p.Actions, Action{Kind: kind, Target: target, Reason: reason})
}

// PlanReconcile computes the convergence plan WITHOUT mutating anything: it is
// the read-only half of reconcile, backing pgb doctor and GET
// /v1/reconcile/plan. now and stuckTimeout drive the TTL-reap and stuck-row
// detection; the rest is pure registry-vs-reality drift.
func (e *Engine) PlanReconcile(ctx context.Context, now time.Time, stuckTimeout time.Duration) (ReconcilePlan, error) {
	var plan ReconcilePlan

	// (a) TTL-expired branches → reap.
	expired, err := e.reg.ListExpiredBranches(now.UTC().Format(time.RFC3339))
	if err != nil {
		return plan, err
	}
	for _, b := range expired {
		plan.add(ActionReap, b.Name, "ttl expired at "+b.ExpiresAt)
	}

	// (b) branches wedged in creating/resetting past the stuck timeout → fail.
	stuck, err := e.reg.ListStuckBranches(now.Add(-stuckTimeout).UTC().Format(time.RFC3339))
	if err != nil {
		return plan, err
	}
	for _, b := range stuck {
		plan.add(ActionFailStuck, b.Name,
			fmt.Sprintf("stuck in %s longer than %s", b.State, stuckTimeout))
	}

	// (c) managed containers with no live registry row → remove.
	known := map[string]bool{}
	live, err := e.reg.ListLiveBranches()
	if err != nil {
		return plan, err
	}
	for _, b := range live {
		if b.ContainerID != "" {
			known[b.ContainerID] = true
		}
	}
	managed, err := e.drv.ListManaged(ctx)
	if err != nil {
		return plan, err
	}
	for _, c := range managed {
		if !known[c.ID] {
			plan.add(ActionRemoveOrphanContainer, c.ID, "managed container with no live branch row")
		}
	}

	// (d) frozen layers with refcount 0 → GC.
	layers, err := e.reg.ListLayers()
	if err != nil {
		return plan, err
	}
	for _, l := range layers {
		n, err := e.reg.CountBranchesReferencingLayer(l.ID)
		if err != nil {
			return plan, err
		}
		if n == 0 {
			plan.add(ActionGCLayer, l.Volume, "frozen layer referenced by 0 live branches")
		}
	}

	// (d) managed volumes owned by no live branch/source → GC. The zfs backend
	// manages datasets, not driver volumes, so its driver reports no volumes
	// and this is a no-op (zfs orphans are GC'd via the layer/branch paths).
	vols, err := e.drv.ListManagedVolumes(ctx)
	if err != nil {
		return plan, err
	}
	if len(vols) > 0 {
		liveVols, err := e.reg.LiveVolumeSet()
		if err != nil {
			return plan, err
		}
		// layer volumes scheduled for GC above are not "live" either, but we
		// already emit a gc_layer action for them; don't double-count.
		planned := map[string]bool{}
		for _, a := range plan.Actions {
			if a.Kind == ActionGCLayer {
				planned[a.Target] = true
			}
		}
		for _, v := range vols {
			if !liveVols[v] && !planned[v] {
				plan.add(ActionGCVolume, v, "managed volume owned by no live branch or source")
			}
		}
	}

	return plan, nil
}

// ApplyReconcile computes a plan and executes it, returning the actions taken.
// It re-checks every destructive action against the live registry immediately
// before acting (safety: a branch may have been provisioned, a layer
// referenced, a volume claimed between planning and applying) and only ever
// touches pgbranch-managed resources. Best-effort: an action that fails is
// recorded as an error but does not abort the pass.
func (e *Engine) ApplyReconcile(ctx context.Context, now time.Time, stuckTimeout time.Duration) (ReconcilePlan, error) {
	e.metrics.IncReconcileRun()
	plan, err := e.PlanReconcile(ctx, now, stuckTimeout)
	if err != nil {
		return ReconcilePlan{}, err
	}
	var taken ReconcilePlan
	var errs []error
	reaped := 0
	for _, a := range plan.Actions {
		applied, err := e.applyAction(ctx, a)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %s: %w", a.Kind, a.Target, err))
			continue
		}
		if !applied {
			continue // re-check said skip (no longer drift)
		}
		taken.Actions = append(taken.Actions, a)
		if a.Kind == ActionReap {
			reaped++
		} else {
			e.metrics.IncReconcileAction(string(a.Kind))
		}
	}
	// reaps flow through the reaper counters (they ARE destroys) so the
	// dashboards line up with the standalone reaper history.
	if reaped > 0 {
		e.metrics.IncReaperRun(reaped)
	} else {
		e.metrics.IncReaperRun(0)
	}
	return taken, errors.Join(errs...)
}

// applyAction executes one planned action after re-validating it against the
// live registry. Returns applied=false (no error) when the re-check shows the
// drift is gone (the resource became legitimate since planning).
func (e *Engine) applyAction(ctx context.Context, a Action) (applied bool, err error) {
	switch a.Kind {
	case ActionReap:
		b, err := e.reg.GetBranchByName(a.Target)
		if err != nil {
			if errors.Is(err, registry.ErrNotFound) {
				return false, nil // already gone
			}
			return false, err
		}
		if b.ExpiresAt == "" || b.State == registry.BranchDestroyed {
			return false, nil
		}
		slog.Info("reconcile: reaping expired branch", "branch", b.Name, "expires_at", b.ExpiresAt)
		if err := e.DestroyBranch(ctx, b.Name); err != nil {
			return false, err
		}
		return true, nil

	case ActionFailStuck:
		b, err := e.reg.GetBranchByName(a.Target)
		if err != nil {
			if errors.Is(err, registry.ErrNotFound) {
				return false, nil
			}
			return false, err
		}
		// re-check: only act if it is STILL stuck in a transient state.
		if b.State != registry.BranchCreating && b.State != registry.BranchResetting {
			return false, nil
		}
		slog.Warn("reconcile: failing stuck branch", "branch", b.Name, "state", b.State, "rw_volume", b.RWVolume)
		if b.ContainerID != "" {
			e.drv.StopRemove(ctx, b.ContainerID)
		}
		if err := e.removeBranchLayer(ctx, b); err != nil {
			slog.Warn("reconcile: remove stuck branch layer failed", "branch", b.Name, "rw_volume", b.RWVolume, "err", err)
		}
		if err := e.reg.TransitionBranch(b.ID, registry.BranchFailed, "reconcile: stuck "+string(b.State)); err != nil {
			return false, err
		}
		return true, nil

	case ActionRemoveOrphanContainer:
		// re-check: the container must still have no live registry row.
		live, err := e.reg.ListLiveBranches()
		if err != nil {
			return false, err
		}
		for _, b := range live {
			if b.ContainerID == a.Target {
				return false, nil // a branch claimed it since planning
			}
		}
		slog.Info("reconcile: removing orphan container", "container", a.Target)
		if err := e.drv.StopRemove(ctx, a.Target); err != nil {
			return false, err
		}
		return true, nil

	case ActionGCLayer:
		// re-check: find the layer by volume and confirm refcount still 0.
		layers, err := e.reg.ListLayers()
		if err != nil {
			return false, err
		}
		var layer *registry.Layer
		for _, l := range layers {
			if l.Volume == a.Target {
				layer = l
				break
			}
		}
		if layer == nil {
			return false, nil // already gone
		}
		n, err := e.reg.CountBranchesReferencingLayer(layer.ID)
		if err != nil {
			return false, err
		}
		if n > 0 {
			return false, nil // got referenced since planning — keep it
		}
		slog.Info("reconcile: gc frozen layer", "volume", layer.Volume)
		if err := e.removeSourceLayer(ctx, layer.Volume); err != nil {
			return false, err
		}
		if err := e.reg.DeleteLayer(layer.ID); err != nil {
			// FK: a child layer still chains onto it — keep the volume, leave
			// the row for the next pass after the child is GC'd.
			return false, err
		}
		return true, nil

	case ActionGCVolume:
		// re-check ownership immediately before deleting: never remove a volume
		// that any live branch's layer chain / rw / source volume now references.
		liveVols, err := e.reg.LiveVolumeSet()
		if err != nil {
			return false, err
		}
		if liveVols[a.Target] {
			return false, nil // claimed since planning — keep it
		}
		slog.Info("reconcile: gc orphan volume", "volume", a.Target)
		if err := e.drv.RemoveVolume(ctx, a.Target); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, fmt.Errorf("unknown reconcile action %q", a.Kind)
}

// Reconcile converges the registry with reality in one pass: reaps TTL-expired
// branches, fails branches stuck in creating/resetting past stuckTimeout,
// removes orphaned managed containers, and GCs dangling layers/volumes. It is
// the unified loop body branchd runs on a ticker (and once at startup); the
// CLI/REST doctor (plan) and gc (apply) call PlanReconcile/ApplyReconcile
// directly. logf (nil = silent) receives a one-line summary per pass.
func (e *Engine) Reconcile(ctx context.Context, now time.Time, stuckTimeout time.Duration, logf func(format string, args ...any)) (ReconcilePlan, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	taken, err := e.ApplyReconcile(ctx, now, stuckTimeout)
	if taken.Drift() {
		logf("reconcile: took %d action(s): %v", len(taken.Actions), summarize(taken))
	}
	if err != nil {
		logf("reconcile: %v", err)
	}
	return taken, err
}

// summarize renders a plan as a compact per-kind count for log lines.
func summarize(p ReconcilePlan) map[ActionKind]int {
	counts := map[ActionKind]int{}
	for _, a := range p.Actions {
		counts[a.Kind]++
	}
	return counts
}

// RunReconcile runs Reconcile on a ticker until ctx is done; branchd's single
// background loop. It runs one pass immediately so startup drift converges
// without waiting a full interval.
func (e *Engine) RunReconcile(ctx context.Context, interval, stuckTimeout time.Duration, logf func(format string, args ...any)) {
	e.Reconcile(ctx, time.Now(), stuckTimeout, logf)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			e.Reconcile(ctx, now, stuckTimeout, logf)
		}
	}
}
