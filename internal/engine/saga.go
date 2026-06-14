package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/abd-ulbasit/pgbranch/internal/cow"
	"github.com/abd-ulbasit/pgbranch/internal/registry"
	"github.com/abd-ulbasit/pgbranch/internal/runtime"
)

// ErrInvalidName rejects branch names that cannot be used across runtimes
// (docker container names, k8s pod names — RFC 1123 after the pgbranch-br-
// prefix). The API maps it to 400.
var ErrInvalidName = errors.New("invalid branch name")

var branchNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)

// validateName enforces the cross-runtime naming rule shared by branch and
// source names: lowercase letters/digits/hyphens, starting with a letter or
// digit, at most 41 chars. The anchored regex rejects path-traversal payloads
// (no '/', '.', '..'), uppercase, leading '-', spaces and over-length names —
// names flow into container/dataset/volume names, so this is the one gate.
func validateName(kind, name string) error {
	if !branchNameRe.MatchString(name) {
		return fmt.Errorf("%w %q: %s name must match [a-z0-9][a-z0-9-]{0,40} (lowercase letters, digits and hyphens, starting with a letter or digit, at most 41 characters)", ErrInvalidName, name, kind)
	}
	return nil
}

// validateBranchName enforces the cross-runtime naming rule on new branches.
// Stored names (reset/destroy paths) are assumed valid: they passed this
// check when created.
func validateBranchName(name string) error {
	return validateName("branch", name)
}

// validateSourceName enforces the same anchored naming rule on source names.
// A source name flows into volume/dataset names (e.g. the ZFS backend builds
// `tank/pgbranch/src-<name>-gN`), so without this gate a name like
// `../../rpool/ROOT` would traverse the dataset namespace. Gated at the engine
// boundary (AddSource) so every backend and caller is covered.
func validateSourceName(name string) error {
	return validateName("source", name)
}

// observeOp brackets a saga entry point: it increments the in-flight gauge,
// and on return records the op's duration and (on error) the error counter,
// then decrements in-flight. Returns a deferred closure; call as
// `defer e.observeOp("create", &err)()`. All metric calls are nil-safe.
func (e *Engine) observeOp(op string, errp *error) func() {
	e.metrics.IncInflight()
	start := time.Now()
	return func() {
		e.metrics.ObserveOp(op, time.Since(start).Seconds())
		if errp != nil && *errp != nil {
			e.metrics.IncOpError(op)
		}
		e.metrics.DecInflight()
	}
}

// CreateBranch is a saga: every step registers a compensation that runs
// (in reverse order) if a later step fails. No orphans, ever.
// ttl 0 means the branch never expires.
func (e *Engine) CreateBranch(ctx context.Context, name, sourceName string, ttl time.Duration) (_ *registry.Branch, err error) {
	defer e.observeOp("create", &err)()
	if err := validateBranchName(name); err != nil {
		return nil, err
	}
	if err := e.checkQuota(); err != nil {
		return nil, err
	}
	src, err := e.reg.GetSourceByName(sourceName)
	if err != nil {
		return nil, fmt.Errorf("source %q: %w", sourceName, err)
	}
	if src.State != registry.SourceReady {
		return nil, fmt.Errorf("source %q is %s, not ready", sourceName, src.State)
	}
	expiresAt := e.expiresAtFor(ttl)
	b := &registry.Branch{
		Name: name, SourceID: src.ID, RWVolume: e.planner.BranchLayerName(name),
		SourceVolume: src.Volume, ExpiresAt: expiresAt,
	}
	if err := e.reg.CreateBranchCtx(ctx, b); err != nil {
		return nil, err
	}
	if err := e.provision(ctx, b, src); err != nil {
		e.logCompensationErr("transition", "create: mark branch failed after provision failed",
			e.reg.TransitionBranchCtx(ctx, b.ID, registry.BranchFailed, err.Error()), "branch", b.Name, "branch_id", b.ID)
		return nil, err
	}
	return e.reg.GetBranchByName(name)
}

// provision runs the resource steps shared by create and reset: writable
// layer, entrypoint install, branch container, readiness wait, masking, mark
// ready. Every step registers a compensation that unwinds (in reverse order)
// on failure; the caller owns the state transition to failed. The layer
// steps depend on the cow backend (overlay volumes vs zfs snapshot+clone).
//
// Overlay branches stack on their own base chain: frozen layers (if any,
// newest first) over the source volume — so resetting a branch created from
// another branch returns it to that parent-derived base, not to the source.
func (e *Engine) provision(ctx context.Context, b *registry.Branch, src *registry.Source) error {
	if e.zfs() {
		return e.provisionZFS(ctx, b, src)
	}
	if e.csi() {
		return e.provisionCSI(ctx, b, src)
	}
	chain, err := e.reg.LayerChain(b.ID)
	if err != nil {
		return err
	}
	plan := cow.PlanBranch(b.RWVolume, b.SourceVolume, layerVolumes(chain))

	var undo []func()
	fail := func(stepErr error) error {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
		return stepErr
	}
	bg := context.WithoutCancel(ctx)

	// 1. rw volume (upper/work + entrypoint script live here)
	if err := e.drv.CreateVolume(ctx, plan.RWVolume, e.instanceLabels(map[string]string{"pgbranch.managed": "true", "pgbranch.branch.id": b.ID})); err != nil {
		return fail(fmt.Errorf("create rw volume: %w", err))
	}
	undo = append(undo, func() {
		e.logCompensationErr("undo", "provision: remove rw volume", e.drv.RemoveVolume(bg, plan.RWVolume),
			"branch", b.Name, "volume", plan.RWVolume)
	})

	// 2. write entrypoint into the rw volume
	if err := e.installOverlayEntrypoint(ctx, plan.RWVolume); err != nil {
		return fail(fmt.Errorf("install entrypoint: %w", err))
	}

	// 3. branch container
	cid, err := e.startOverlayBranch(ctx, b.Name, plan, e.image(src.PGVersion), e.branchLabels(b))
	if err != nil {
		return fail(fmt.Errorf("start instance: %w", err))
	}
	undo = append(undo, func() {
		e.logCompensationErr("undo", "provision: stop/remove branch container", e.drv.StopRemove(bg, cid),
			"branch", b.Name, "container", cid)
	})

	// 4-6. readiness, masking, mark ready
	if err := e.awaitAndMark(ctx, b, src, cid); err != nil {
		return fail(err)
	}
	return nil
}

// layerVolumes projects a layer chain (topmost first) onto its volume names.
func layerVolumes(chain []registry.Layer) []string {
	if len(chain) == 0 {
		return nil
	}
	out := make([]string, len(chain))
	for i, l := range chain {
		out[i] = l.Volume
	}
	return out
}

// installOverlayEntrypoint writes the overlay entrypoint script into a rw
// volume and prepares its upper/work dirs.
func (e *Engine) installOverlayEntrypoint(ctx context.Context, rwVolume string) error {
	_, err := e.drv.RunHelper(ctx, runtime.HelperSpec{
		Image:  "alpine:3.21",
		Cmd:    []string{"sh", "-c", `printf '%s' "$PGBRANCH_ENTRYPOINT" > /pgbranch/rw/entrypoint.sh && chmod 0755 /pgbranch/rw/entrypoint.sh && mkdir -p /pgbranch/rw/upper /pgbranch/rw/work`},
		Env:    []string{"PGBRANCH_ENTRYPOINT=" + cow.EntrypointScript},
		Mounts: []runtime.Mount{{Volume: rwVolume, Target: cow.RWPath}},
	})
	return err
}

// startOverlayBranch starts a branch container assembling the overlay stack
// from plan: source volume ro at lower0, frozen layer volumes (newest first)
// ro at lower1..N, the rw volume at RWPath. PGBRANCH_LOWERS lists the overlay
// lowerdirs newest-first with the source last (see cow.PlanBranch).
func (e *Engine) startOverlayBranch(ctx context.Context, name string, plan cow.Plan, image string, labels map[string]string) (string, error) {
	mounts := make([]runtime.Mount, 0, len(plan.LayerVolumes)+2)
	mounts = append(mounts, runtime.Mount{Volume: plan.SourceVolume, Target: cow.LowerMountTarget(0), ReadOnly: true})
	for i, lv := range plan.LayerVolumes {
		mounts = append(mounts, runtime.Mount{Volume: lv, Target: cow.LowerMountTarget(i + 1), ReadOnly: true})
	}
	mounts = append(mounts, runtime.Mount{Volume: plan.RWVolume, Target: cow.RWPath})
	return e.drv.StartBranch(ctx, runtime.BranchSpec{
		Name:  "pgbranch-br-" + name,
		Image: image,
		Env: []string{
			"PGDATA=" + cow.MergedPath,
			"PGBRANCH_LOWERS=" + plan.LowerEnv(),
		},
		Mounts:     mounts,
		Entrypoint: []string{"/bin/sh", cow.RWPath + "/entrypoint.sh"},
		Labels:     labels,
	})
}

// provisionZFS is provision's layer half for the zfs backend: instead of an
// empty rw volume overlaid on the source, the branch gets a writable clone
// of a per-branch snapshot of the source dataset — both instant — and the
// container runs straight on the clone's mountpoint (no overlay entrypoint).
func (e *Engine) provisionZFS(ctx context.Context, b *registry.Branch, src *registry.Source) error {
	var undo []func()
	fail := func(stepErr error) error {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
		return stepErr
	}
	bg := context.WithoutCancel(ctx)

	// 1. snapshot the source dataset
	if err := e.runZFS(ctx, zfsHelperSpec(e.planner.ZFSSnapshot(b.SourceVolume, b.Name))); err != nil {
		return fail(fmt.Errorf("zfs snapshot: %w", err))
	}
	undo = append(undo, func() {
		e.logCompensationErr("undo", "provisionZFS: destroy snapshot",
			e.runZFS(bg, zfsDestroySpec(e.planner.ZFSDestroySnapshot(b.SourceVolume, b.Name))),
			"branch", b.Name, "source_volume", b.SourceVolume)
	})

	// 2. clone it into the branch's writable dataset
	if err := e.runZFS(ctx, zfsHelperSpec(e.planner.ZFSClone(b.SourceVolume, b.Name))); err != nil {
		return fail(fmt.Errorf("zfs clone: %w", err))
	}
	undo = append(undo, func() {
		e.logCompensationErr("undo", "provisionZFS: destroy clone",
			e.runZFS(bg, zfsDestroySpec(e.planner.ZFSDestroyClone(b.Name))),
			"branch", b.Name, "rw_volume", b.RWVolume)
	})

	// 3. install the zfs entrypoint into the clone, next to its data/ dir
	// (plain unprivileged helper: it only writes a file)
	cloneMount := runtime.Mount{Kind: runtime.MountHostPath, Volume: e.planner.Mountpoint(b.RWVolume), Target: cow.RWPath}
	if _, err := e.drv.RunHelper(ctx, runtime.HelperSpec{
		Image:  "alpine:3.21",
		Cmd:    []string{"sh", "-c", `printf '%s' "$PGBRANCH_ENTRYPOINT" > /pgbranch/rw/entrypoint.sh && chmod 0755 /pgbranch/rw/entrypoint.sh`},
		Env:    []string{"PGBRANCH_ENTRYPOINT=" + cow.EntrypointScriptDirect},
		Mounts: []runtime.Mount{cloneMount},
	}); err != nil {
		return fail(fmt.Errorf("install entrypoint: %w", err))
	}

	// 4. branch container on the clone mountpoint
	cid, err := e.drv.StartBranch(ctx, runtime.BranchSpec{
		Name:       "pgbranch-br-" + b.Name,
		Image:      e.image(src.PGVersion),
		Env:        []string{"PGDATA=" + cow.DirectDataPath},
		Mounts:     []runtime.Mount{cloneMount},
		Entrypoint: []string{"/bin/sh", cow.RWPath + "/entrypoint.sh"},
		Labels:     e.branchLabels(b),
	})
	if err != nil {
		return fail(fmt.Errorf("start instance: %w", err))
	}
	undo = append(undo, func() {
		e.logCompensationErr("undo", "provisionZFS: stop/remove branch container", e.drv.StopRemove(bg, cid),
			"branch", b.Name, "container", cid)
	})

	if err := e.awaitAndMark(ctx, b, src, cid); err != nil {
		return fail(err)
	}
	return nil
}

func (e *Engine) branchLabels(b *registry.Branch) map[string]string {
	return e.instanceLabels(map[string]string{
		"pgbranch.managed": "true", "pgbranch.role": "branch",
		"pgbranch.branch.id": b.ID, "pgbranch.branch.name": b.Name,
	})
}

// instanceLabels stamps the owning registry's instance id onto a label map so
// reconcile reclaims only resources belonging to THIS registry. Every managed
// resource (volumes, branch containers/pods) is labelled through here — the one
// place the pgbranch.instance label is added — so no call site can omit it.
func (e *Engine) instanceLabels(labels map[string]string) map[string]string {
	if labels == nil {
		labels = map[string]string{}
	}
	labels[runtime.LabelInstance] = e.reg.InstanceID()
	return labels
}

// awaitAndMark is the backend-independent tail of provisioning: wait for
// postgres readiness (covers WAL recovery time), apply the source's masking
// scripts inside the fresh clone (so the branch never serves unmasked data;
// reset re-runs this because it re-clones), rotate the branch's credentials
// (when enabled), then record container + address and mark ready. A failing
// masking script fails the branch.
func (e *Engine) awaitAndMark(ctx context.Context, b *registry.Branch, src *registry.Source, cid string) error {
	// Record the container before the readiness wait so a concurrent reconcile
	// treats this in-flight container as owned (not an orphan to reap).
	if err := e.reg.SetBranchContainer(b.ID, cid); err != nil {
		return err
	}
	if err := e.waitReady(ctx, cid, 90*time.Second); err != nil {
		return fmt.Errorf("instance never became ready: %w", err)
	}
	if err := e.applyMasking(ctx, cid, src); err != nil {
		return err
	}
	if err := e.rotateBranchCredentials(ctx, cid, b, src); err != nil {
		return err
	}
	info, err := e.inspectAddr(ctx, cid)
	if err != nil {
		return err
	}
	return e.reg.MarkBranchReadyCtx(ctx, b.ID, cid, info.Host, info.Port)
}

// rotateBranchCredentials gives a fresh/reset branch its own password: a
// 32-hex crypto/rand secret applied via in-branch psql over the local socket
// (same exec path as masking — peer auth, no password needed) and persisted
// on the branch row before the branch is marked ready. No-op in inherit mode
// (rotation off). Runs on create, reset and branch-from-branch children;
// parent restarts (freeze/csi quiesce) never pass through here, so a parent
// keeps its existing password.
func (e *Engine) rotateBranchCredentials(ctx context.Context, cid string, b *registry.Branch, src *registry.Source) error {
	if !e.rotateCredentials {
		return nil
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Errorf("rotate credentials: %w", err)
	}
	pw := hex.EncodeToString(buf)
	user := src.ConnUser
	if user == "" {
		user = "postgres"
	}
	// the role name is identifier-quoted; the password is pure hex, so the
	// literal needs no escaping
	stmt := fmt.Sprintf(`ALTER ROLE "%s" WITH PASSWORD '%s'`, strings.ReplaceAll(user, `"`, `""`), pw)
	// S-2 (audit-log exposure): the rotated password is passed as a `psql -c`
	// argv element, so it appears in container/k8s exec audit logs. Feeding the
	// SQL on stdin instead would keep it out of argv, but runtime.Driver only
	// exposes Exec/ExecOutput (no stdin) — adding a stdin variant means changing
	// the interface and both the Docker and Kube drivers, an invasive change for
	// a bounded leak. The exposure is bounded: this same password is also stored
	// (now encrypted at rest), is re-rotated on every reset, and belongs to an
	// ephemeral branch. Left as-is deliberately; revisit if a stdin exec lands.
	if err := e.drv.Exec(ctx, cid, psqlCmd(src, stmt)); err != nil {
		return fmt.Errorf("rotate credentials for branch %q: %w", b.Name, err)
	}
	if err := e.reg.SetBranchPassword(b.ID, pw); err != nil {
		return fmt.Errorf("persist rotated password for branch %q: %w", b.Name, err)
	}
	return nil
}

// inspectAddr inspects cid until the runtime reports a routable address.
// Kubernetes pods are exec-ready (pg_isready answers) seconds before the
// kubelet's status sync publishes status.podIP, so a single Inspect right
// after readiness can capture an empty host — the proxy would then dial
// ":5432". Docker returns an address immediately; the first iteration wins.
func (e *Engine) inspectAddr(ctx context.Context, cid string) (runtime.ContainerInfo, error) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		info, err := e.drv.Inspect(ctx, cid)
		if err != nil {
			return info, err
		}
		if info.Host != "" {
			return info, nil
		}
		if time.Now().After(deadline) {
			return info, fmt.Errorf("instance %s reported no address within 30s", cid)
		}
		select {
		case <-ctx.Done():
			return info, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// ResetBranch throws away a ready branch's writes and reprovisions it from
// its recorded source volume on the same registry row (ready -> resetting ->
// ready; new container id and host port).
func (e *Engine) ResetBranch(ctx context.Context, name string) (_ *registry.Branch, err error) {
	defer e.observeOp("reset", &err)()
	b, err := e.reg.GetBranchByName(name)
	if err != nil {
		return nil, err
	}
	src, err := e.reg.GetSourceByID(b.SourceID)
	if err != nil {
		return nil, err
	}
	if err := e.reg.TransitionBranchCtx(ctx, b.ID, registry.BranchResetting, "reset requested"); err != nil {
		return nil, err
	}
	fail := func(stepErr error) (*registry.Branch, error) {
		e.logCompensationErr("transition", "reset: mark branch failed after reset step failed",
			e.reg.TransitionBranchCtx(ctx, b.ID, registry.BranchFailed, stepErr.Error()), "branch", b.Name, "branch_id", b.ID)
		return nil, stepErr
	}
	if b.ContainerID != "" {
		if err := e.drv.StopRemove(ctx, b.ContainerID); err != nil {
			return fail(fmt.Errorf("remove container: %w", err))
		}
	}
	if err := e.removeBranchLayer(ctx, b); err != nil {
		return fail(fmt.Errorf("remove branch layer: %w", err))
	}
	if err := e.provision(ctx, b, src); err != nil {
		return fail(fmt.Errorf("reset %q: %w", name, err))
	}
	return e.reg.GetBranchByName(name)
}

// applyMasking runs the source's masking scripts (registry order) inside the
// branch container via psql over the local socket — peer/local auth inside
// the container means the engine never needs a password. ON_ERROR_STOP makes
// any failing statement abort the script; the first failing script aborts
// provisioning.
func (e *Engine) applyMasking(ctx context.Context, cid string, src *registry.Source) error {
	scripts, err := e.reg.GetMaskScripts(src.ID)
	if err != nil {
		return fmt.Errorf("load mask scripts: %w", err)
	}
	if len(scripts) == 0 {
		return nil
	}
	start := time.Now()
	defer func() { e.metrics.ObserveMasking(time.Since(start).Seconds()) }()
	for _, sc := range scripts {
		if err := e.drv.Exec(ctx, cid, psqlCmd(src, sc.SQL)); err != nil {
			return fmt.Errorf("masking script %q: %w", sc.Name, err)
		}
	}
	return nil
}

// psqlCmd builds an in-container psql invocation over the local socket
// (peer/local auth — no password needed) with the source's user/database.
func psqlCmd(src *registry.Source, sql string) []string {
	user, db := src.ConnUser, src.ConnDB
	if user == "" {
		user = "postgres"
	}
	if db == "" {
		db = "postgres"
	}
	return []string{"psql", "-v", "ON_ERROR_STOP=1", "-U", user, "-d", db, "-c", sql}
}

func (e *Engine) waitReady(ctx context.Context, cid string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		lastErr = e.drv.Exec(ctx, cid, []string{"pg_isready", "-U", "postgres", "-h", "/var/run/postgresql"})
		if lastErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return lastErr
}

func (e *Engine) DestroyBranch(ctx context.Context, name string) (err error) {
	defer e.observeOp("destroy", &err)()
	b, err := e.reg.GetBranchByName(name)
	if err != nil {
		return err
	}
	// zfs children clone snapshots taken on the parent's dataset, so a zfs
	// parent cannot go while children live (overlay parents can: the frozen
	// layer volumes keep children alive).
	if e.zfs() {
		if n, err := e.reg.CountLiveBranchesByVolume(b.RWVolume); err != nil {
			return err
		} else if n > 0 {
			return fmt.Errorf("branch %q has %d child branch(es) cloned from it; destroy them first", name, n)
		}
	}
	chain, err := e.reg.LayerChain(b.ID)
	if err != nil {
		return err
	}
	// Force a branch wedged in a transient state (creating/resetting) to failed
	// first, so it can be destroyed now instead of waiting out the 10m
	// stuck-timeout reconcile. creating->failed and resetting->failed are both
	// legal (the same edges reconcile's fail-stuck path uses), so this just
	// fast-paths what reconcile would eventually do.
	forcedFromTransient := b.State == registry.BranchCreating || b.State == registry.BranchResetting
	if forcedFromTransient {
		if err := e.reg.TransitionBranchCtx(ctx, b.ID, registry.BranchFailed, "destroy requested: forcing stuck "+string(b.State)); err != nil {
			return err
		}
	}
	if err := e.reg.TransitionBranchCtx(ctx, b.ID, registry.BranchDestroying, "destroy requested"); err != nil {
		return err
	}
	if b.ContainerID != "" {
		if err := e.drv.StopRemove(ctx, b.ContainerID); err != nil {
			return fmt.Errorf("remove container: %w", err)
		}
	}
	// When we forced a branch out of a transient state, its rw volume may still
	// be another live branch's data. A freeze parent forced out of 'resetting'
	// keeps its live data in its rw volume until CommitFreeze; an in-flight child
	// references that volume (as its source_volume, or by naming the parent).
	// Removing it would be the A1 data-loss bug, so guard exactly like
	// reconcile's ActionFailStuck. A normally-destroyed ready/failed branch is
	// NOT guarded: a post-CommitFreeze parent has already swapped to a fresh rw
	// volume (its old one is now a layer GC'd separately), and the
	// parent_branch_name link would otherwise false-positive against that fresh
	// volume.
	if forcedFromTransient {
		referenced, err := e.reg.CountLiveBranchesReferencingRW(b.Name, b.RWVolume)
		if err != nil {
			return err
		}
		if referenced > 0 {
			slog.Warn("destroy: forced-stuck branch rw volume is live data for another branch; keeping the volume",
				"branch", b.Name, "rw_volume", b.RWVolume, "referencing_branches", referenced)
		} else if err := e.removeBranchLayer(ctx, b); err != nil {
			return fmt.Errorf("remove branch layer: %w", err)
		}
	} else if err := e.removeBranchLayer(ctx, b); err != nil {
		return fmt.Errorf("remove branch layer: %w", err)
	}
	if err := e.reg.TransitionBranchCtx(ctx, b.ID, registry.BranchDestroyed, ""); err != nil {
		return err
	}
	// the destroyed branch may have been the last reference to its frozen
	// layer chain and/or an old-generation source volume
	e.gcLayers(ctx, chain)
	e.gcSourceVolume(ctx, b.SourceID, b.SourceVolume)
	return nil
}

// gcLayers removes frozen layers with zero remaining references, walking the
// chain topmost-first: any branch referencing a layer also references all of
// that layer's ancestors, so the first still-referenced layer stops the
// cascade. Best-effort, like gcSourceVolume.
func (e *Engine) gcLayers(ctx context.Context, chain []registry.Layer) {
	for _, l := range chain {
		if n, err := e.reg.CountBranchesReferencingLayer(l.ID); err != nil || n > 0 {
			return
		}
		if err := e.removeSourceLayer(ctx, l.Volume); err != nil {
			return
		}
		if err := e.reg.DeleteLayer(l.ID); err != nil {
			return
		}
	}
}

// gcSourceVolume removes an old-generation source volume once it is no
// longer the source's current volume and no live branch references it.
// Best-effort: GC failures leave the volume for the next opportunity.
func (e *Engine) gcSourceVolume(ctx context.Context, sourceID, volume string) {
	if volume == "" {
		return
	}
	if src, err := e.reg.GetSourceByID(sourceID); err == nil && src.Volume == volume {
		return // current generation stays
	}
	if n, err := e.reg.CountLiveBranchesByVolume(volume); err != nil || n > 0 {
		return
	}
	// a zfs child's "source volume" is its parent's clone dataset — never GC
	// a volume that is some live branch's writable layer
	if n, err := e.reg.CountLiveBranchesByRWVolume(volume); err != nil || n > 0 {
		return
	}
	e.removeSourceLayer(ctx, volume)
}
