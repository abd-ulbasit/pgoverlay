// Package engine orchestrates branch lifecycle as sagas over the registry,
// cow planner, and runtime driver. The CLI (P1) and branchd (P2) both embed it.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/abd-ulbasit/pgoverlay/internal/cow"
	"github.com/abd-ulbasit/pgoverlay/internal/metrics"
	"github.com/abd-ulbasit/pgoverlay/internal/pgctl"
	"github.com/abd-ulbasit/pgoverlay/internal/registry"
	"github.com/abd-ulbasit/pgoverlay/internal/runtime"
)

type Engine struct {
	reg          *registry.Registry
	drv          runtime.Driver
	defaultImage string
	planner      cow.Planner
	// rotateCredentials gives every fresh/reset branch its own password
	// (ALTER ROLE inside the branch, after masking, before ready) instead of
	// inheriting the source's credentials. branchd --rotate-branch-credentials.
	rotateCredentials bool
	// metrics observes saga durations, errors and reaper/reconcile counters.
	// nil = no instrumentation (every call is nil-safe); branchd wires it.
	metrics *metrics.Metrics
	// maxBranches caps the number of live (non-destroyed) branches. 0 = no cap.
	// Enforced in the create paths (branchd --max-branches).
	maxBranches int
	// defaultTTL is applied to a create that requests no TTL (0 = no default,
	// the branch never expires). maxTTL caps any requested TTL (0 = no cap).
	// Both come from branchd --default-ttl / --max-ttl.
	defaultTTL time.Duration
	maxTTL     time.Duration
	// heartbeatEvery is how often a running saga or seed bumps its rows'
	// updated_at (see keepAlive, trackSeeding). 0 = defaultHeartbeat.
	heartbeatEvery time.Duration
	// maxLayerDepth caps an overlay branch's frozen layer chain (see
	// checkLayerDepth). 0 = DefaultMaxLayerDepth.
	maxLayerDepth int
	// rs is reconcile's in-process bookkeeping: sources this process is
	// seeding, branches reconcile is restarting, and the endpoint-refresh
	// rate limit (see reconcile.go).
	rs reconcileState
	// seedSettle is how a fresh seed is prepared before branches start from
	// it (branchd --seed-settle). "" = pgctl.DefaultSettleMode.
	seedSettle pgctl.SettleMode
}

// parentStepTimeout bounds a parent-affecting step (stopping a freeze or
// clone parent) that runs detached from the request context.
const parentStepTimeout = 2 * time.Minute

// ErrQuotaExceeded is returned by the create paths (and DiffBranch, whose
// throwaway is a branch too) when --max-branches is set and the live-branch
// count is already at the cap. The API maps it to 403.
var ErrQuotaExceeded = errors.New("branch quota exceeded")

// Option configures optional engine behavior at construction time.
type Option func(*Engine)

// WithCredentialRotation turns on per-branch credential rotation: every
// branch create and reset generates a fresh password, applies it inside the
// branch and stores it on the branch row (returned by the API as `password`).
func WithCredentialRotation() Option {
	return func(e *Engine) { e.rotateCredentials = true }
}

// WithMaxBranches caps the number of live (non-destroyed) branches. The create
// paths return ErrQuotaExceeded once the cap is reached. 0 (the default) is
// unlimited. branchd --max-branches / PGOVERLAY_MAX_BRANCHES.
func WithMaxBranches(n int) Option {
	return func(e *Engine) { e.maxBranches = n }
}

// WithTTLPolicy sets the create-time TTL policy: defaultTTL is used when a
// create requests no TTL (0 = no default, never expires); maxTTL caps any
// requested TTL (0 = no cap). branchd --default-ttl / --max-ttl. The policy is
// applied in the engine create path so both API- and ghook-created branches
// inherit it.
func WithTTLPolicy(defaultTTL, maxTTL time.Duration) Option {
	return func(e *Engine) { e.defaultTTL = defaultTTL; e.maxTTL = maxTTL }
}

// WithMetrics attaches a metrics sink the engine uses to observe saga
// durations/errors, masking duration, in-flight ops and reaper/reconcile
// counters. nil is accepted (every metric call is nil-safe).
func WithMetrics(m *metrics.Metrics) Option {
	return func(e *Engine) { e.metrics = m }
}

// defaultHeartbeat is keepAlive's default period. It must stay well under
// reconcile's stuck timeout (branchd --stuck-timeout, default 10m).
const defaultHeartbeat = 30 * time.Second

// WithHeartbeatInterval sets how often a running saga bumps its branch rows'
// (and a running seed its source row's) updated_at so reconcile never
// mistakes a slow-but-alive operation for an abandoned one. Keep it well
// under the stuck timeout (branchd uses a quarter of --stuck-timeout, capped
// at defaultHeartbeat). d <= 0 keeps the default.
func WithHeartbeatInterval(d time.Duration) Option {
	return func(e *Engine) {
		if d > 0 {
			e.heartbeatEvery = d
		}
	}
}

// DefaultMaxLayerDepth is the default cap on an overlay branch's frozen layer
// chain. Every branch-from-branch freezes the parent's writes into one more
// layer, and the parent keeps its whole chain until it is destroyed (reset
// keeps it too), so a parent forked N times stacks N layers. Each is an
// overlay lowerdir: lookups walk them all, and the mount option string must
// fit in one page (about 160 lowerdirs); the kernel stops at 500.
const DefaultMaxLayerDepth = 100

// WithMaxLayerDepth caps overlay layer chains at n frozen layers: branching
// from a branch whose chain is already that deep is refused with
// ErrQuotaExceeded. branchd --max-layer-depth. n <= 0 keeps the default.
func WithMaxLayerDepth(n int) Option {
	return func(e *Engine) {
		if n > 0 {
			e.maxLayerDepth = n
		}
	}
}

// New builds an engine on the default OverlayFS backend.
func New(reg *registry.Registry, drv runtime.Driver, defaultImage string, opts ...Option) *Engine {
	return NewWithPlanner(reg, drv, defaultImage, cow.Planner{Backend: cow.BackendOverlay}, opts...)
}

// NewWithPlanner selects the copy-on-write backend (branchd --cow).
func NewWithPlanner(reg *registry.Registry, drv runtime.Driver, defaultImage string, planner cow.Planner, opts ...Option) *Engine {
	e := &Engine{reg: reg, drv: drv, defaultImage: defaultImage, planner: planner}
	for _, o := range opts {
		o(e)
	}
	return e
}

// logCompensationErr surfaces a swallowed best-effort error: a saga
// compensation (undo: RemoveVolume/StopRemove), a post-failure state
// transition (transition: TransitionBranch(..., Failed)/TouchBranch), or a
// deferred cleanup (cleanup: throwaway DestroyBranch). It does NOT change
// control flow — the caller still proceeds best-effort; this only makes the
// failure observable via a slog.Warn and the compensation-failures counter.
// kind is the metric label (transition|undo|cleanup). Extra attrs (e.g.
// "branch", name, "rw_volume", vol) are appended to the log line. nil err is a
// no-op so call sites can pass results unconditionally.
func (e *Engine) logCompensationErr(kind, msg string, err error, attrs ...any) {
	if err == nil {
		return
	}
	e.metrics.IncCompensationFailure(kind)
	slog.Warn(msg, append(attrs, "kind", kind, "err", err)...)
}

// heartbeatInterval is how often running work bumps its rows' updated_at:
// branch sagas (keepAlive) and source seeds (trackSeeding) alike.
func (e *Engine) heartbeatInterval() time.Duration {
	if e.heartbeatEvery > 0 {
		return e.heartbeatEvery
	}
	return defaultHeartbeat
}

// keepAlive bumps the given branch rows' updated_at every heartbeat until the
// returned stop function is called (stop waits for the ticker goroutine, so no
// touch lands after it returns). Sagas hold it for as long as they keep rows
// in creating/resetting: reconcile fails rows whose updated_at is older than
// the stuck timeout, and without a heartbeat a long readiness wait or masking
// script (arbitrary user SQL) got a live branch — and, in a freeze, its
// parent — failed and torn down mid-provision.
func (e *Engine) keepAlive(ids ...string) (stop func()) {
	every := e.heartbeatInterval()
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				for _, id := range ids {
					e.logCompensationErr("transition", "heartbeat: touch branch stuck-timer", e.reg.TouchBranch(id), "branch_id", id)
				}
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

// checkQuota enforces --max-branches before a create provisions anything:
// when the cap is set and the live (non-destroyed) branch count is already at
// or over it, the create is refused with ErrQuotaExceeded. 0 = unlimited.
func (e *Engine) checkQuota() error {
	if e.maxBranches <= 0 {
		return nil
	}
	n, err := e.reg.CountLiveBranches()
	if err != nil {
		return err
	}
	if n >= e.maxBranches {
		return fmt.Errorf("%w: %d live branch(es) at the --max-branches=%d cap", ErrQuotaExceeded, n, e.maxBranches)
	}
	return nil
}

// checkLayerDepth refuses an overlay freeze that would push a chain past the
// configured depth. There is no compaction yet, so the way out is a branch
// with a shorter history: recreate the parent from its source (or from a
// branch with a shorter chain) and replay its setup.
func (e *Engine) checkLayerDepth(parent *registry.Branch, chain []registry.Layer) error {
	limit := e.maxLayerDepth
	if limit <= 0 {
		limit = DefaultMaxLayerDepth
	}
	if len(chain) >= limit {
		return fmt.Errorf("%w: branch %q already stacks %d frozen layers (--max-layer-depth=%d); branching from it again would exceed the limit. Branch from a branch with a shorter history, or recreate %q from its source",
			ErrQuotaExceeded, parent.Name, len(chain), limit, parent.Name)
	}
	return nil
}

// expiresAtFor applies the TTL policy (--default-ttl / --max-ttl) to a
// requested ttl and renders the resulting expires_at. A zero requested ttl
// falls back to defaultTTL; a requested ttl above maxTTL is capped to maxTTL.
// The effective ttl of 0 means the branch never expires (empty expires_at).
// Centralised here so every create path (API and ghook, which both go through
// the engine) gets identical behaviour.
func (e *Engine) expiresAtFor(ttl time.Duration) string {
	if ttl <= 0 && e.defaultTTL > 0 {
		ttl = e.defaultTTL
	}
	if e.maxTTL > 0 && ttl > e.maxTTL {
		ttl = e.maxTTL
	}
	if ttl <= 0 {
		return ""
	}
	return time.Now().Add(ttl).UTC().Format(time.RFC3339)
}

// image is the container image for a source's seed helpers and branches: the
// source's own image when it set one (extensions, locales, a matching libc),
// else postgres:<pg_version>, else the engine default.
func (e *Engine) image(src *registry.Source) string {
	switch {
	case src.Image != "":
		return src.Image
	case src.PGVersion == "":
		return e.defaultImage
	}
	return "postgres:" + src.PGVersion
}

// WithSeedSettle sets how every seed (source add and refresh) is prepared
// before branches start from it: pgctl.SettleFreeze (the default) recovers
// the copy, freezes and analyzes it and shuts it down cleanly,
// pgctl.SettleRecover only recovers and shuts down, pgctl.SettleOff leaves it
// as the seed command wrote it. branchd --seed-settle, pgb
// $PGOVERLAY_SEED_SETTLE. "" keeps the default.
func WithSeedSettle(m pgctl.SettleMode) Option {
	return func(e *Engine) { e.seedSettle = m }
}

// seedSource runs the source's seeding method (pg_basebackup or pg_dump,
// per Source.SeedVia) into the given layer, then settles it (see
// pgctl.Settle; the dump helper settles in place). Backend-neutral: the
// layer is resolved through seedTarget (overlay volume, zfs mountpoint, csi
// PVC), and every backend gains from a settled seed: branches start from a
// clean shutdown instead of replaying the backup's WAL, and their reads do
// not write.
func (e *Engine) seedSource(ctx context.Context, s *registry.Source, layer, password string) error {
	seedVol, seedKind := e.seedTarget(layer)
	spec := pgctl.SeedSpec{
		Image: e.image(s), Volume: seedVol, MountKind: seedKind, Network: s.Network,
		Host: s.ConnHost, Port: s.ConnPort, User: s.ConnUser, Password: password,
		Settle: e.seedSettle,
	}
	if s.SeedVia == registry.SeedViaDump {
		return pgctl.SeedDump(ctx, e.drv, pgctl.SeedDumpSpec{
			SeedSpec: spec, Database: s.ConnDB, Schemas: s.DumpSchemas,
		})
	}
	if err := pgctl.Seed(ctx, e.drv, spec); err != nil {
		return err
	}
	return pgctl.Settle(ctx, e.drv, spec)
}

// AddSource registers a source and seeds it from the given live Postgres.
func (e *Engine) AddSource(ctx context.Context, s *registry.Source, password string) error {
	if err := validateSourceName(s.Name); err != nil {
		return err
	}
	// reject unusable connection settings (empty host, bad port or sslmode,
	// unstorable schema patterns) before any row or layer exists
	conn := pgctl.SeedDumpSpec{
		SeedSpec: pgctl.SeedSpec{Host: s.ConnHost, Port: s.ConnPort, User: s.ConnUser},
		Schemas:  s.DumpSchemas,
	}
	if err := conn.Validate(); err != nil {
		return fmt.Errorf("source %q: %w", s.Name, err)
	}
	s.Volume = e.planner.SourceLayerName(s.Name, 1)
	if err := e.reg.CreateSourceCtx(ctx, s); err != nil {
		return err
	}
	// heartbeat the seeding row so reconcile can tell a long seed from one
	// whose process died (fail_stuck_source)
	defer e.trackSeeding(s.ID)()
	if err := e.createSourceLayer(ctx, s.Volume, e.instanceLabels(map[string]string{"pgoverlay.managed": "true", "pgoverlay.source.name": s.Name})); err != nil {
		e.logCompensationErr("transition", "add source: mark source failed after layer create failed",
			e.reg.SetSourceStateCtx(ctx, s.ID, registry.SourceFailed, "source layer create failed"), "source", s.Name)
		return err
	}
	if err := e.seedSource(ctx, s, s.Volume, password); err != nil {
		e.logCompensationErr("undo", "add source: remove source layer after seed failed",
			e.removeSourceLayer(context.WithoutCancel(ctx), s.Volume), "source", s.Name, "volume", s.Volume)
		e.logCompensationErr("transition", "add source: mark source failed after seed failed",
			e.reg.SetSourceStateCtx(ctx, s.ID, registry.SourceFailed, failureReason(err)), "source", s.Name)
		return fmt.Errorf("seed source %q: %w", s.Name, err)
	}
	return e.reg.SetSourceStateCtx(ctx, s.ID, registry.SourceReady, "seed complete")
}

// RefreshSource re-seeds a source into a fresh generation volume. Existing
// branches keep the volume they were created from; only new branches see the
// new generation. The previous generation's volume is GC'd once no live
// branch references it. A failed seed leaves the current generation intact.
func (e *Engine) RefreshSource(ctx context.Context, name, password string) error {
	src, err := e.reg.GetSourceByName(name)
	if err != nil {
		return err
	}
	if src.State != registry.SourceReady {
		return fmt.Errorf("source %q is %s, not ready", name, src.State)
	}
	newVol := e.planner.SourceLayerName(name, src.Generation+1)
	// claim the next generation before creating it: nothing names it until
	// BumpSourceGeneration, and reconcile's volume GC must not take it while
	// it is being seeded. The claim is a registry column, so it also holds
	// against a reconcile pass in another process (an HA peer, local pgb).
	if err := e.reg.SetSourcePendingVolume(src.ID, newVol); err != nil {
		return fmt.Errorf("refresh source %q: claim %s: %w", name, newVol, err)
	}
	release := func() {
		e.logCompensationErr("undo", "refresh source: release new generation claim",
			e.reg.SetSourcePendingVolume(src.ID, ""), "source", name, "volume", newVol)
	}
	if err := e.createSourceLayer(ctx, newVol, e.instanceLabels(map[string]string{"pgoverlay.managed": "true", "pgoverlay.source.name": name})); err != nil {
		release()
		return err
	}
	if err := e.seedSource(ctx, src, newVol, password); err != nil {
		e.logCompensationErr("undo", "refresh source: remove new generation layer after seed failed",
			e.removeSourceLayer(context.WithoutCancel(ctx), newVol), "source", name, "volume", newVol)
		release()
		return fmt.Errorf("refresh source %q: %w", name, err)
	}
	oldVol := src.Volume
	if err := e.reg.BumpSourceGenerationCtx(ctx, src.ID, newVol); err != nil {
		return err
	}
	e.gcSourceVolume(ctx, src.ID, oldVol)
	return nil
}

// RemoveSource deletes a source's volume, its orphaned frozen layers, and
// the registry rows. Refused while any live branch still uses the source or
// (defensively) while any layer is still referenced.
func (e *Engine) RemoveSource(ctx context.Context, name string) error {
	src, err := e.reg.GetSourceByName(name)
	if err != nil {
		return err
	}
	n, err := e.reg.CountLiveBranchesBySource(src.ID)
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("source %q has %d live branch(es); destroy them first", name, n)
	}
	// with no live branches every layer must be zero-ref (references come
	// from live branches only); GC any orphans best-effort GC left behind
	layers, err := e.reg.ListLayersBySource(src.ID)
	if err != nil {
		return err
	}
	for _, l := range layers {
		if n, err := e.reg.CountBranchesReferencingLayer(l.ID); err != nil {
			return err
		} else if n > 0 {
			return fmt.Errorf("source %q has a frozen layer (%s) still referenced by %d live branch(es); destroy them first", name, l.Volume, n)
		}
	}
	for _, l := range layers {
		if err := e.removeSourceLayer(ctx, l.Volume); err != nil {
			return fmt.Errorf("remove layer volume %q: %w", l.Volume, err)
		}
	}
	if err := e.removeSourceLayer(ctx, src.Volume); err != nil && src.State == registry.SourceReady {
		// failed sources may have no layer (seed cleanup removed it)
		return fmt.Errorf("remove source layer: %w", err)
	}
	// DeleteSource cascades the layer rows
	if err := e.reg.DeleteSourceCtx(ctx, src.ID); err != nil {
		return err
	}
	// failed attempts of the same name (registries from before CreateSource
	// replaced them could hold several) go too, or `source rm` would report
	// success while `source ls` still lists the name. They own no volumes.
	if _, err := e.reg.DeleteFailedSources(name); err != nil {
		return fmt.Errorf("remove failed attempts of source %q: %w", name, err)
	}
	return nil
}

// maxFailureReason caps a failure reason stored in the registry.
const maxFailureReason = 1024

// failureReason renders err for the transitions journal, which lives at rest
// in the registry file. Seed and masking failures embed the helper's or
// psql's output, and Postgres prints offending row values in DETAIL and
// CONTEXT lines (`DETAIL:  Key (email)=(…) already exists`, `CONTEXT:  COPY
// customers, line 4213, column email: "…"`) — production data, and for a
// failed masking script data that was never masked. Those lines are dropped
// and the rest is capped; the caller still gets the full error.
func failureReason(err error) string {
	lines := strings.Split(err.Error(), "\n")
	kept := lines[:0]
	for _, l := range lines {
		if strings.Contains(l, "DETAIL:") || strings.Contains(l, "CONTEXT:") {
			continue
		}
		kept = append(kept, l)
	}
	reason := strings.Join(kept, "\n")
	if len(reason) > maxFailureReason {
		reason = strings.ToValidUTF8(reason[:maxFailureReason], "") + " …(truncated)"
	}
	return reason
}

// BranchUsage measures a branch's copy-on-write layer in bytes (the branch's
// own writes, not the shared source data). Overlay: `du -sb` on the rw
// volume; zfs: the clone's `used` property (space unique to the clone). It
// is a helper-container roundtrip — cheap, but not free.
func (e *Engine) BranchUsage(ctx context.Context, name string) (int64, error) {
	b, err := e.reg.GetBranchByName(name)
	if err != nil {
		return 0, err
	}
	spec := runtime.HelperSpec{
		Image:  runtime.UtilityImage,
		Cmd:    []string{"du", "-sb", cow.RWPath},
		Mounts: []runtime.Mount{{Volume: b.RWVolume, Target: cow.RWPath, ReadOnly: true}},
	}
	if e.zfs() {
		spec = zfsHelperSpec(e.planner.ZFSUsed(b.RWVolume))
	}
	out, err := e.drv.RunHelper(ctx, spec)
	if err != nil {
		return 0, fmt.Errorf("measure branch %q usage: %w", name, err)
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return 0, fmt.Errorf("measure branch %q usage: empty measurement output", name)
	}
	n, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("measure branch %q usage: unparseable measurement output %q", name, out)
	}
	return n, nil
}

// ReapExpired destroys every ready/failed branch whose TTL has passed and
// returns the names destroyed. Retained as a thin primitive over the unified
// reconcile loop (see reconcile.go) for callers that only want the TTL pass;
// now is injected for testability. The reconcile loop in branchd folds this in.
func (e *Engine) ReapExpired(ctx context.Context, now time.Time) (destroyed []string, err error) {
	expired, err := e.reg.ListExpiredBranches(now.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	var errs []error
	for _, b := range expired {
		if derr := e.DestroyBranch(ctx, b.Name); derr != nil {
			errs = append(errs, fmt.Errorf("reap %q: %w", b.Name, derr))
			continue
		}
		destroyed = append(destroyed, b.Name)
	}
	return destroyed, errors.Join(errs...)
}
