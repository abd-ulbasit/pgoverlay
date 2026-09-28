package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/abd-ulbasit/pgoverlay/internal/cow"
	"github.com/abd-ulbasit/pgoverlay/internal/registry"
	"github.com/abd-ulbasit/pgoverlay/internal/runtime"
)

// ActionKind enumerates the convergence steps a reconcile pass can take. The
// values double as the {action} label on pgoverlay_reconcile_actions_total.
type ActionKind string

const (
	// ActionReap destroys a branch whose TTL has passed.
	ActionReap ActionKind = "reap"
	// ActionFailStuck fails a branch wedged in creating/resetting past the
	// stuck timeout and cleans its half-built resources.
	ActionFailStuck ActionKind = "fail_stuck"
	// ActionFailStuckSource fails a source wedged in seeding: its seed has not
	// made progress (heartbeat) for the stuck timeout, so the process running
	// it died. Its half-seeded volume is removed.
	ActionFailStuckSource ActionKind = "fail_stuck_source"
	// ActionRetryDestroy re-runs the teardown of a branch wedged in destroying
	// (a destroy that failed or was interrupted part-way).
	ActionRetryDestroy ActionKind = "retry_destroy"
	// ActionRestartBranch recreates the container/pod of a ready branch that
	// is gone or stopped for good, on the branch's existing volumes. If it does
	// not become ready the branch is failed; its volumes are kept.
	ActionRestartBranch ActionKind = "restart_branch"
	// ActionUpdateEndpoint records the address a ready branch's running
	// container/pod now has (a new pod IP, a re-published port).
	ActionUpdateEndpoint ActionKind = "update_endpoint"
	// ActionRemoveOrphanContainer removes a managed container/pod with no live
	// registry row.
	ActionRemoveOrphanContainer ActionKind = "remove_orphan_container"
	// ActionRemoveOrphanHelper removes a finished helper container/pod that
	// the process which ran it never removed (it died first).
	ActionRemoveOrphanHelper ActionKind = "remove_orphan_helper"
	// ActionGCLayer removes a frozen layer (volume + row) whose refcount is 0.
	ActionGCLayer ActionKind = "gc_layer"
	// ActionGCVolume removes a managed volume owned by no live branch/source.
	ActionGCVolume ActionKind = "gc_volume"
)

// Action is one intended convergence step. Target is the branch name, source
// name, container id, layer volume or volume name the action operates on;
// Reason is a human-readable justification. ReconcilePlan is a list of these.
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

// reconcileState is the engine's in-process reconcile bookkeeping.
//
// claims counts resources that a running operation has created, or is about
// to create, before the registry records them: a freeze's fresh parent rw
// volume until CommitFreeze, a refresh's next-generation volume until
// BumpSourceGeneration, a source being seeded, a branch reconcile is
// restarting. PlanReconcile and applyAction consult it, so those resources are
// never taken for orphans or abandoned rows while they are in use.
//
// Claims only cover operations in this process. Volume GC additionally skips
// any volume younger than the stuck timeout, which covers another process (a
// CLI next to branchd, a previous leader) that has just created one, and a
// running seed heartbeats its source row for the same reason.
type reconcileState struct {
	mu        sync.Mutex
	claims    map[string]int
	refreshed map[string]time.Time // last RefreshBranchEndpoint runtime check, per branch
}

func (s *reconcileState) claim(key string) (release func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claims == nil {
		s.claims = map[string]int{}
	}
	s.claims[key]++
	return s.releaser(key)
}

// tryClaim claims key only if nobody holds it.
func (s *reconcileState) tryClaim(key string) (release func(), ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claims[key] > 0 {
		return nil, false
	}
	if s.claims == nil {
		s.claims = map[string]int{}
	}
	s.claims[key] = 1
	return s.releaser(key), true
}

func (s *reconcileState) releaser(key string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.claims[key]--; s.claims[key] <= 0 {
				delete(s.claims, key)
			}
		})
	}
}

func (s *reconcileState) claimed(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claims[key] > 0
}

// allowRefresh rate-limits RefreshBranchEndpoint per branch.
func (s *reconcileState) allowRefresh(name string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if last, ok := s.refreshed[name]; ok && now.Sub(last) < endpointRefreshInterval {
		return false
	}
	if s.refreshed == nil {
		s.refreshed = map[string]time.Time{}
	}
	s.refreshed[name] = now
	return true
}

func volumeClaim(name string) string { return "volume:" + name }
func sourceClaim(id string) string   { return "source:" + id }
func branchClaim(name string) string { return "branch:" + name }

// claimVolume marks a volume the caller is about to create as in flight until
// release is called: after the registry records the volume, or after the
// caller's compensation removed it. Claim BEFORE creating the volume.
func (e *Engine) claimVolume(name string) (release func()) {
	return e.rs.claim(volumeClaim(name))
}

// seedHeartbeat is how often a running seed bumps its source row.
var seedHeartbeat = 30 * time.Second

// trackSeeding marks source id as being seeded by this process until stop is
// called. Reconcile skips a claimed source, and the heartbeat keeps its
// updated_at fresh so no other process's reconcile takes a long seed for an
// abandoned one (fail_stuck_source).
func (e *Engine) trackSeeding(id string) (stop func()) {
	release := e.rs.claim(sourceClaim(id))
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(seedHeartbeat)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if err := e.reg.TouchSource(id); err != nil {
					slog.Warn("seed heartbeat failed", "source_id", id, "err", err)
				}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			wg.Wait()
			release()
		})
	}
}

// PlanReconcile computes the convergence plan WITHOUT mutating anything: it is
// the read-only half of reconcile, backing pgb doctor and GET
// /v1/reconcile/plan. now and stuckTimeout drive the TTL-reap, stuck-row and
// volume-age checks; the rest compares the registry with the runtime in both
// directions.
func (e *Engine) PlanReconcile(ctx context.Context, now time.Time, stuckTimeout time.Duration) (ReconcilePlan, error) {
	var plan ReconcilePlan
	before := registry.TimeString(now.Add(-stuckTimeout))

	// (a) TTL-expired branches → reap.
	expired, err := e.reg.ListExpiredBranches(now.UTC().Format(time.RFC3339))
	if err != nil {
		return plan, err
	}
	reaping := map[string]bool{}
	for _, b := range expired {
		plan.add(ActionReap, b.Name, "ttl expired at "+b.ExpiresAt)
		reaping[b.Name] = true
	}

	// (b) rows wedged in a transient state past the stuck timeout: branches
	// in creating/resetting → fail; sources in seeding → fail; branches in
	// destroying → finish the teardown.
	stuck, err := e.reg.ListStuckBranches(before)
	if err != nil {
		return plan, err
	}
	for _, b := range stuck {
		plan.add(ActionFailStuck, b.Name,
			fmt.Sprintf("stuck in %s longer than %s", b.State, stuckTimeout))
	}
	stuckSources, err := e.reg.ListStuckSources(before)
	if err != nil {
		return plan, err
	}
	for _, s := range stuckSources {
		if e.rs.claimed(sourceClaim(s.ID)) {
			continue // this process is seeding it
		}
		plan.add(ActionFailStuckSource, s.Name,
			fmt.Sprintf("seeding with no progress for longer than %s", stuckTimeout))
	}
	destroying, err := e.reg.ListStuckDestroyingBranches(before)
	if err != nil {
		return plan, err
	}
	for _, b := range destroying {
		plan.add(ActionRetryDestroy, b.Name,
			fmt.Sprintf("stuck in destroying longer than %s", stuckTimeout))
	}

	live, err := e.reg.ListLiveBranches()
	if err != nil {
		return plan, err
	}
	managed, err := e.drv.ListManaged(ctx)
	if err != nil {
		return plan, err
	}

	// (c) registry → runtime: every ready branch needs a running container at
	// the address the registry routes to. Docker restarts containers and
	// kubelet recreates pod sandboxes without pgoverlay taking part, and a
	// container or pod can be removed or evicted from outside.
	byID := make(map[string]runtime.ContainerInfo, len(managed))
	for _, c := range managed {
		byID[c.ID] = c
	}
	for _, b := range live {
		if b.State != registry.BranchReady || reaping[b.Name] || e.rs.claimed(branchClaim(b.Name)) {
			continue
		}
		if kind, reason := readyDrift(b, byID); kind != "" {
			plan.add(kind, b.Name, reason)
		}
	}

	// (d) runtime → registry: managed containers with no live registry row →
	// remove.
	known := map[string]bool{}
	liveByID := make(map[string]*registry.Branch, len(live))
	for _, b := range live {
		liveByID[b.ID] = b
		if b.ContainerID != "" {
			known[b.ContainerID] = true
		}
	}
	instanceID := e.reg.InstanceID()
	for _, c := range managed {
		// Only reclaim containers this registry owns. A managed container whose
		// pgoverlay.instance label is absent or names another instance belongs to
		// a different pgoverlay on the same daemon — leave it alone.
		if c.Labels[runtime.LabelInstance] != instanceID {
			continue
		}
		if !known[c.ID] && !ownedInFlight(c, liveByID) {
			plan.add(ActionRemoveOrphanContainer, c.ID, "managed container with no live branch row")
		}
	}

	// (d) finished helpers nobody removed (the process running them died).
	helpers, err := e.drv.ListHelpers(ctx)
	if err != nil {
		return plan, err
	}
	for _, h := range helpers {
		if orphanHelper(h, now, stuckTimeout) {
			plan.add(ActionRemoveOrphanHelper, h.ID, fmt.Sprintf("helper finished (%s) and was never removed", h.Status))
		}
	}

	// (e) frozen layers with refcount 0 → GC.
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

	// (e) managed volumes owned by no live branch/source → GC. The zfs backend
	// manages datasets, not driver volumes, so its driver reports no volumes
	// and this is a no-op (zfs orphans are GC'd via the layer/branch paths).
	vols, err := e.drv.ListManagedVolumes(ctx, instanceID)
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
			if liveVols[v.Name] || planned[v.Name] || e.rs.claimed(volumeClaim(v.Name)) {
				continue
			}
			// A volume the registry does not name yet may belong to an
			// operation in another process that has just created it (a freeze
			// before CommitFreeze, a refresh before BumpSourceGeneration). Give
			// it the stuck timeout before calling it an orphan.
			if !v.Created.IsZero() && now.Sub(v.Created) < stuckTimeout {
				continue
			}
			plan.add(ActionGCVolume, v.Name, "managed volume owned by no live branch or source")
		}
	}

	return plan, nil
}

// readyDrift compares a ready branch with the runtime's view of its container.
// A container that is gone, or stopped for good, is restarted; one that runs
// on a different address has that address recorded. A container the runtime
// is still (re)starting (docker restarting, a Pending or terminating pod) is
// left alone.
func readyDrift(b *registry.Branch, byID map[string]runtime.ContainerInfo) (ActionKind, string) {
	c, ok := byID[b.ContainerID]
	switch {
	case b.ContainerID == "" || !ok:
		return ActionRestartBranch, fmt.Sprintf("container %s of the ready branch is gone", shortID(b.ContainerID))
	case c.Stopped:
		return ActionRestartBranch, fmt.Sprintf("container %s of the ready branch is not running (%s)", shortID(c.ID), c.Status)
	case c.Running && c.Host != "" && c.Port != 0 && (c.Host != b.Host || c.Port != b.Port):
		return ActionUpdateEndpoint, fmt.Sprintf("address moved from %s to %s",
			net.JoinHostPort(b.Host, strconv.Itoa(b.Port)), net.JoinHostPort(c.Host, strconv.Itoa(c.Port)))
	}
	return "", ""
}

// shortID abbreviates a docker container id for messages (pod names pass
// through).
func shortID(id string) string {
	if id == "" {
		return "(none)"
	}
	if len(id) == 64 {
		return id[:12]
	}
	return id
}

// ownedInFlight reports whether a container that no row names still belongs
// to a live branch that may yet record it: its pgoverlay.branch.id label names
// a row in creating or resetting (a saga started it and has not written
// container_id yet) or ready (reconcile is restarting it). Failed and
// destroying rows own nothing unrecorded — the operations that failed them
// removed the containers they started — so those containers are orphans.
func ownedInFlight(c runtime.ContainerInfo, liveByID map[string]*registry.Branch) bool {
	b := liveByID[c.Labels[runtime.LabelBranchID]]
	if b == nil {
		return false
	}
	switch b.State {
	case registry.BranchCreating, registry.BranchResetting, registry.BranchReady:
		return true
	}
	return false
}

// orphanHelper reports whether a helper is finished and old enough that the
// process which ran it would have removed it by now. Running helpers are left
// alone: a live seed in another process looks the same as an orphaned one.
func orphanHelper(h runtime.ContainerInfo, now time.Time, grace time.Duration) bool {
	return h.Stopped && !h.Created.IsZero() && now.Sub(h.Created) >= grace
}

// reconcilePass carries a pass's clock into apply, so the apply-time
// re-checks use the same cut-offs as the plan.
type reconcilePass struct {
	now          time.Time
	stuckTimeout time.Duration
}

func (p reconcilePass) before() string {
	return registry.TimeString(p.now.Add(-p.stuckTimeout))
}

// ApplyReconcile computes a plan and executes it, returning the actions taken.
// It re-checks every destructive action against the live registry immediately
// before acting (safety: a branch may have been provisioned, a layer
// referenced, a volume claimed between planning and applying) and only ever
// touches pgoverlay-managed resources. Best-effort: an action that fails is
// recorded as an error but does not abort the pass.
func (e *Engine) ApplyReconcile(ctx context.Context, now time.Time, stuckTimeout time.Duration) (ReconcilePlan, error) {
	e.metrics.IncReconcileRun()
	plan, err := e.PlanReconcile(ctx, now, stuckTimeout)
	if err != nil {
		return ReconcilePlan{}, err
	}
	pass := reconcilePass{now: now, stuckTimeout: stuckTimeout}
	var taken ReconcilePlan
	var errs []error
	reaped := 0
	for _, a := range plan.Actions {
		applied, err := e.applyAction(ctx, a, pass)
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

// resumeDestroy finishes the teardown of a branch wedged in destroying. It
// is DestroyBranch, which resumes from the destroying state once the
// lifecycle fix for failed destroys (LIFECYCLE-02, issue #10) makes it
// re-entrant; until then the retry reports DestroyBranch's refusal. A
// variable so tests can observe the call.
var resumeDestroy = (*Engine).DestroyBranch

// applyAction executes one planned action after re-validating it against the
// live registry. Returns applied=false (no error) when the re-check shows the
// drift is gone (the resource became legitimate since planning).
func (e *Engine) applyAction(ctx context.Context, a Action, p reconcilePass) (applied bool, err error) {
	switch a.Kind {
	case ActionReap:
		b, err := e.reg.GetBranchByName(a.Target)
		if err != nil {
			if errors.Is(err, registry.ErrNotFound) {
				return false, nil // already gone
			}
			return false, err
		}
		// re-check: still expiring, and still at rest. A reset or freeze that
		// started since planning owns the branch now; destroying it would force
		// it out of that transient state while the saga is still provisioning
		// on the same volume names. The next pass reaps it once the saga ends.
		if b.ExpiresAt == "" || (b.State != registry.BranchReady && b.State != registry.BranchFailed) {
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
		// Fail the row FIRST, as one compare-and-swap on the plan's stuck
		// criterion. When the owning saga finished, failed or made progress
		// since planning the swap does not happen and nothing is touched;
		// tearing down first would leave a saga that won the race with a
		// ready row whose container and volume were just deleted.
		failed, err := e.reg.FailStuckBranch(ctx, b.ID, p.before(), "reconcile: stuck "+string(b.State))
		if err != nil || !failed {
			return false, err
		}
		// re-read: the saga may have recorded a container after the read above
		if cur, err := e.reg.GetBranchByName(a.Target); err == nil && cur.ID == b.ID {
			b.ContainerID = cur.ContainerID
		}
		slog.Warn("reconcile: failed stuck branch", "branch", b.Name, "state", b.State, "rw_volume", b.RWVolume)
		if b.ContainerID != "" {
			if err := e.drv.StopRemove(ctx, b.ContainerID); err != nil {
				slog.Warn("reconcile: remove stuck branch container failed", "branch", b.Name, "container", b.ContainerID, "err", err)
			}
		}
		// Never delete a volume that holds another live branch's data. A
		// branch-from-branch freeze (overlay) or PVC clone (csi/zfs) parks the
		// PARENT in resetting and keeps the parent's live data in its rw volume
		// until CommitFreeze; an in-flight child references that volume (as its
		// source_volume, or by naming the parent). A slow freeze can overrun the
		// stuck timeout — failing the parent is fine, but removing its rw volume
		// would be permanent data loss, so skip the layer removal when the volume
		// is still referenced. (Re-checked here against the live registry, like
		// the GC paths, so a child provisioned since planning is respected.)
		referenced, err := e.reg.CountLiveBranchesReferencingRW(b.Name, b.RWVolume)
		if err != nil {
			return true, err
		}
		if referenced > 0 {
			slog.Warn("reconcile: stuck branch rw volume is live data for another branch; failing the row but keeping the volume",
				"branch", b.Name, "rw_volume", b.RWVolume, "referencing_branches", referenced)
		} else if err := e.removeBranchLayer(ctx, b); err != nil {
			slog.Warn("reconcile: remove stuck branch layer failed", "branch", b.Name, "rw_volume", b.RWVolume, "err", err)
		}
		return true, nil

	case ActionFailStuckSource:
		src, err := e.reg.GetSourceByName(a.Target)
		if err != nil {
			if errors.Is(err, registry.ErrNotFound) {
				return false, nil
			}
			return false, err
		}
		if src.State != registry.SourceSeeding || e.rs.claimed(sourceClaim(src.ID)) {
			return false, nil
		}
		failed, err := e.reg.FailStuckSource(ctx, src.ID, p.before(),
			fmt.Sprintf("reconcile: seeding made no progress for %s (the seeding process stopped)", p.stuckTimeout))
		if err != nil || !failed {
			return false, err
		}
		slog.Warn("reconcile: failed source stuck in seeding", "source", src.Name, "volume", src.Volume)
		// the half-seeded layer is useless; AddSource's own failure path
		// removes it the same way (best-effort: a helper still writing to it
		// keeps a docker volume busy, and RemoveSource cleans it up later)
		if err := e.removeSourceLayer(ctx, src.Volume); err != nil {
			slog.Warn("reconcile: remove half-seeded source layer failed", "source", src.Name, "volume", src.Volume, "err", err)
		}
		return true, nil

	case ActionRetryDestroy:
		b, err := e.reg.GetBranchByName(a.Target)
		if err != nil {
			if errors.Is(err, registry.ErrNotFound) {
				return false, nil
			}
			return false, err
		}
		if b.State != registry.BranchDestroying {
			return false, nil
		}
		slog.Warn("reconcile: retrying destroy of branch stuck in destroying", "branch", b.Name)
		if err := resumeDestroy(e, ctx, b.Name); err != nil {
			return false, err
		}
		return true, nil

	case ActionRestartBranch:
		release, ok := e.rs.tryClaim(branchClaim(a.Target))
		if !ok {
			return false, nil // another pass in this process is restarting it
		}
		defer release()
		b, err := e.reg.GetBranchByName(a.Target)
		if err != nil {
			if errors.Is(err, registry.ErrNotFound) {
				return false, nil
			}
			return false, err
		}
		if b.State != registry.BranchReady {
			return false, nil
		}
		if b.ContainerID != "" {
			info, err := e.drv.Inspect(ctx, b.ContainerID)
			if err != nil && !errors.Is(err, runtime.ErrNotFound) {
				return false, err
			}
			if err == nil && !info.Stopped {
				return false, nil // back up, or the runtime is bringing it back
			}
		}
		if err := e.restartBranch(ctx, b); err != nil {
			return false, err
		}
		return true, nil

	case ActionUpdateEndpoint:
		b, err := e.reg.GetBranchByName(a.Target)
		if err != nil {
			if errors.Is(err, registry.ErrNotFound) {
				return false, nil
			}
			return false, err
		}
		if b.State != registry.BranchReady || b.ContainerID == "" {
			return false, nil
		}
		info, err := e.drv.Inspect(ctx, b.ContainerID)
		if errors.Is(err, runtime.ErrNotFound) {
			return false, nil // gone now: the next pass restarts it
		}
		if err != nil {
			return false, err
		}
		if !info.Running || info.Host == "" || info.Port == 0 || (info.Host == b.Host && info.Port == b.Port) {
			return false, nil
		}
		slog.Info("reconcile: branch address moved", "branch", b.Name,
			"from", net.JoinHostPort(b.Host, strconv.Itoa(b.Port)), "to", net.JoinHostPort(info.Host, strconv.Itoa(info.Port)))
		return e.reg.UpdateBranchEndpoint(b.ID, b.ContainerID, b.ContainerID, info.Host, info.Port)

	case ActionRemoveOrphanContainer:
		// re-check: the container must still have no live registry row, and
		// must not belong to a branch whose saga may still record it.
		live, err := e.reg.ListLiveBranches()
		if err != nil {
			return false, err
		}
		liveByID := make(map[string]*registry.Branch, len(live))
		for _, b := range live {
			if b.ContainerID == a.Target {
				return false, nil // a branch claimed it since planning
			}
			liveByID[b.ID] = b
		}
		info, err := e.drv.Inspect(ctx, a.Target)
		if errors.Is(err, runtime.ErrNotFound) {
			return false, nil // already gone
		}
		if err != nil {
			return false, err
		}
		if ownedInFlight(info, liveByID) {
			return false, nil
		}
		slog.Info("reconcile: removing orphan container", "container", a.Target)
		if err := e.drv.StopRemove(ctx, a.Target); err != nil {
			return false, err
		}
		return true, nil

	case ActionRemoveOrphanHelper:
		info, err := e.drv.Inspect(ctx, a.Target)
		if errors.Is(err, runtime.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !info.Stopped {
			return false, nil
		}
		slog.Info("reconcile: removing finished helper left behind", "helper", a.Target, "status", info.Status)
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
		// that any live branch's layer chain / rw / source volume now
		// references, or that an operation in this process has claimed.
		if e.rs.claimed(volumeClaim(a.Target)) {
			return false, nil
		}
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

// restartReadyTimeout bounds how long a restarted branch may take to accept
// connections before it is failed (the same budget provisioning uses).
var restartReadyTimeout = 90 * time.Second

// errBranchMoved reports that a branch changed hands (reset, freeze, destroy)
// while reconcile was restarting it; the operation that took it over owns it.
var errBranchMoved = errors.New("branch changed while it was being restarted; left to the operation that changed it")

// restartBranch recreates a ready branch's container on its existing volumes
// after the runtime lost it: removed (`docker rm`, a pod deleted or drained),
// or stopped for good (`docker stop`, an evicted pod, a container docker could
// not start again after a reboot). The data lives in the volumes, so this is
// the same restart a freeze or csi quiesce performs; masking and credential
// rotation are not repeated (the data already carries both).
//
// The row stays ready throughout; its address was dead anyway. The new
// container is recorded before the readiness wait so no other pass reaps it,
// and its address once it is ready. A branch that does not come back within
// restartReadyTimeout is failed, keeping its volumes. A failure to start the
// container at all is returned without failing the row, so the next pass
// tries again.
func (e *Engine) restartBranch(ctx context.Context, b *registry.Branch) error {
	bg := context.WithoutCancel(ctx)
	src, err := e.reg.GetSourceByID(b.SourceID)
	if err != nil {
		return fmt.Errorf("source of branch %q: %w", b.Name, err)
	}
	slog.Warn("reconcile: restarting ready branch whose container is gone or stopped", "branch", b.Name, "container", b.ContainerID)
	if b.ContainerID != "" {
		if err := e.drv.StopRemove(ctx, b.ContainerID); err != nil {
			return fmt.Errorf("remove the stopped container: %w", err)
		}
	}
	if err := e.removeStrayBranchContainer(ctx, b); err != nil {
		return err
	}
	cid, err := e.startExistingBranch(ctx, b, src)
	if err != nil {
		return fmt.Errorf("start branch %q: %w", b.Name, err)
	}
	owned, err := e.reg.UpdateBranchEndpoint(b.ID, b.ContainerID, cid, b.Host, b.Port)
	if err != nil || !owned {
		e.logCompensationErr("undo", "reconcile: remove restarted container the branch no longer records", e.drv.StopRemove(bg, cid),
			"branch", b.Name, "container", cid)
		if err != nil {
			return err
		}
		return errBranchMoved
	}
	notReady := func(cause error) error {
		if ctx.Err() != nil {
			// shutting down: leave the starting container recorded; the next
			// pass (or process) looks at it again
			return cause
		}
		e.logCompensationErr("undo", "reconcile: remove restarted container that never became ready", e.drv.StopRemove(bg, cid),
			"branch", b.Name, "container", cid)
		reason := "reconcile: container lost and restart failed: " + cause.Error()
		failed, err := e.reg.FailReadyBranch(bg, b.ID, cid, reason)
		switch {
		case err != nil:
			return fmt.Errorf("restart branch %q: %v; marking it failed: %w", b.Name, cause, err)
		case !failed:
			return fmt.Errorf("restart branch %q: %w (%v)", b.Name, cause, errBranchMoved)
		}
		return fmt.Errorf("restart branch %q: %w; branch marked failed, its volumes are kept", b.Name, cause)
	}
	if err := e.waitReady(ctx, cid, restartReadyTimeout); err != nil {
		return notReady(fmt.Errorf("never became ready: %w", err))
	}
	info, err := e.inspectAddr(ctx, cid)
	if err != nil {
		return notReady(err)
	}
	if ok, err := e.reg.UpdateBranchEndpoint(b.ID, cid, cid, info.Host, info.Port); err != nil {
		return err
	} else if !ok {
		return errBranchMoved
	}
	slog.Info("reconcile: restarted branch", "branch", b.Name, "container", cid, "host", info.Host, "port", info.Port)
	return nil
}

// branchContainerName is the container/pod name of a branch's instance.
func branchContainerName(branch string) string { return "pgoverlay-br-" + branch }

// removeStrayBranchContainer removes a container that holds the branch's
// container name without being the one the row records — what a restart
// interrupted between starting a container and recording it leaves behind.
// Only a container labelled with this registry's instance and this branch's
// id is removed; any other holder of the name is reported.
func (e *Engine) removeStrayBranchContainer(ctx context.Context, b *registry.Branch) error {
	name := branchContainerName(b.Name)
	info, err := e.drv.Inspect(ctx, name)
	if errors.Is(err, runtime.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Labels[runtime.LabelInstance] != e.reg.InstanceID() || info.Labels[runtime.LabelBranchID] != b.ID {
		return fmt.Errorf("container name %s is taken by %s, which is not this branch's container", name, shortID(info.ID))
	}
	return e.drv.StopRemove(ctx, info.ID)
}

// startExistingBranch starts a branch's instance on the volumes it already
// has: the overlay stack over its layer chain, its zfs clone, or its csi PVC.
func (e *Engine) startExistingBranch(ctx context.Context, b *registry.Branch, src *registry.Source) (string, error) {
	image := e.image(src)
	switch {
	case e.zfs():
		// same spec as provisionZFS step 4
		return e.drv.StartBranch(ctx, runtime.BranchSpec{
			Name:       branchContainerName(b.Name),
			Image:      image,
			Env:        []string{"PGDATA=" + cow.DirectDataPath},
			Mounts:     []runtime.Mount{{Kind: runtime.MountHostPath, Volume: e.planner.Mountpoint(b.RWVolume), Target: cow.RWPath}},
			Entrypoint: []string{"/bin/sh", cow.RWPath + "/entrypoint.sh"},
			Labels:     e.branchLabels(b),
		})
	case e.csi():
		return e.startDirectBranch(ctx, b.Name, b.RWVolume, image, e.branchLabels(b))
	}
	chain, err := e.reg.LayerChain(b.ID)
	if err != nil {
		return "", err
	}
	return e.startOverlayBranch(ctx, b.Name, cow.PlanBranch(b.RWVolume, b.SourceVolume, layerVolumes(chain)), image, e.branchLabels(b))
}

// endpointRefreshInterval bounds how often RefreshBranchEndpoint asks the
// runtime about one branch: the Postgres router calls it after a failed dial,
// which an unauthenticated client can trigger at will.
const endpointRefreshInterval = 5 * time.Second

// RefreshBranchEndpoint re-reads a ready branch's address from the runtime,
// records it when it moved, and returns the current "host:port". The Postgres
// router calls it after a dial to the recorded address fails, so a branch
// whose pod came back with a new IP (or container on a new port) is reachable
// at once instead of after the next reconcile pass. Within
// endpointRefreshInterval of the previous check for the same branch it
// returns the recorded address without asking the runtime.
func (e *Engine) RefreshBranchEndpoint(ctx context.Context, name string) (string, error) {
	b, err := e.reg.GetBranchByName(name)
	if err != nil {
		return "", err
	}
	if b.State != registry.BranchReady {
		return "", fmt.Errorf("branch is %s, not ready", b.State)
	}
	addr := net.JoinHostPort(b.Host, strconv.Itoa(b.Port))
	if b.ContainerID == "" || !e.rs.allowRefresh(name, time.Now()) {
		return addr, nil
	}
	info, err := e.drv.Inspect(ctx, b.ContainerID)
	if err != nil {
		return addr, err
	}
	if !info.Running || info.Host == "" || info.Port == 0 || (info.Host == b.Host && info.Port == b.Port) {
		return addr, nil
	}
	ok, err := e.reg.UpdateBranchEndpoint(b.ID, b.ContainerID, b.ContainerID, info.Host, info.Port)
	if err != nil || !ok {
		return addr, err
	}
	moved := net.JoinHostPort(info.Host, strconv.Itoa(info.Port))
	slog.Info("branch address moved; recorded the new one", "branch", name, "from", addr, "to", moved)
	return moved, nil
}

// Reconcile converges the registry with reality in one pass: reaps TTL-expired
// branches, fails rows stuck in a transient state past stuckTimeout, repairs
// ready branches whose container is gone, stopped or moved, removes orphaned
// managed containers and helpers, and GCs dangling layers/volumes. It is the
// unified loop body branchd runs on a ticker (and once at startup); the
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
