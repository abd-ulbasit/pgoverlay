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

	"github.com/abd-ulbasit/pgbranch/internal/cow"
	"github.com/abd-ulbasit/pgbranch/internal/metrics"
	"github.com/abd-ulbasit/pgbranch/internal/pgctl"
	"github.com/abd-ulbasit/pgbranch/internal/registry"
	"github.com/abd-ulbasit/pgbranch/internal/runtime"
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
}

// ErrQuotaExceeded is returned by the create paths when --max-branches is set
// and the live-branch count is already at the cap. The API maps it to 403.
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
// unlimited. branchd --max-branches / PGBRANCH_MAX_BRANCHES.
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

func (e *Engine) image(pgVersion string) string {
	if pgVersion == "" {
		return e.defaultImage
	}
	return "postgres:" + pgVersion
}

// seedSource runs the source's seeding method (pg_basebackup or pg_dump,
// per Source.SeedVia) into the given layer. Backend-neutral: the layer is
// resolved through seedTarget (overlay volume, zfs mountpoint, csi PVC).
func (e *Engine) seedSource(ctx context.Context, s *registry.Source, layer, password string) error {
	seedVol, seedKind := e.seedTarget(layer)
	spec := pgctl.SeedSpec{
		Image: e.image(s.PGVersion), Volume: seedVol, MountKind: seedKind, Network: s.Network,
		Host: s.ConnHost, Port: s.ConnPort, User: s.ConnUser, Password: password,
	}
	if s.SeedVia == registry.SeedViaDump {
		return pgctl.SeedDump(ctx, e.drv, pgctl.SeedDumpSpec{
			SeedSpec: spec, Database: s.ConnDB, Schemas: s.DumpSchemas,
		})
	}
	return pgctl.Seed(ctx, e.drv, spec)
}

// AddSource registers a source and seeds it from the given live Postgres.
func (e *Engine) AddSource(ctx context.Context, s *registry.Source, password string) error {
	if err := validateSourceName(s.Name); err != nil {
		return err
	}
	s.Volume = e.planner.SourceLayerName(s.Name, 1)
	if err := e.reg.CreateSource(s); err != nil {
		return err
	}
	if err := e.createSourceLayer(ctx, s.Volume, e.instanceLabels(map[string]string{"pgbranch.managed": "true", "pgbranch.source.name": s.Name})); err != nil {
		e.logCompensationErr("transition", "add source: mark source failed after layer create failed",
			e.reg.SetSourceState(s.ID, registry.SourceFailed, "source layer create failed"), "source", s.Name)
		return err
	}
	if err := e.seedSource(ctx, s, s.Volume, password); err != nil {
		e.logCompensationErr("undo", "add source: remove source layer after seed failed",
			e.removeSourceLayer(context.WithoutCancel(ctx), s.Volume), "source", s.Name, "volume", s.Volume)
		e.logCompensationErr("transition", "add source: mark source failed after seed failed",
			e.reg.SetSourceState(s.ID, registry.SourceFailed, err.Error()), "source", s.Name)
		return fmt.Errorf("seed source %q: %w", s.Name, err)
	}
	return e.reg.SetSourceState(s.ID, registry.SourceReady, "seed complete")
}

// RefreshSource re-seeds a source into a fresh generation volume. Existing
// branches keep the volume they were created from; only new branches see the
// new generation. The previous generation's volume is GC'd once no live
// branch references it. A failed seed leaves the current generation intact.
func (e *Engine) RefreshSource(ctx context.Context, name, password string) error {
	src, err := e.reg.GetSourceByName(name)
	if err != nil {
		return fmt.Errorf("source %q: %w", name, err)
	}
	if src.State != registry.SourceReady {
		return fmt.Errorf("source %q is %s, not ready", name, src.State)
	}
	newVol := e.planner.SourceLayerName(name, src.Generation+1)
	if err := e.createSourceLayer(ctx, newVol, e.instanceLabels(map[string]string{"pgbranch.managed": "true", "pgbranch.source.name": name})); err != nil {
		return err
	}
	if err := e.seedSource(ctx, src, newVol, password); err != nil {
		e.removeSourceLayer(context.WithoutCancel(ctx), newVol)
		return fmt.Errorf("refresh source %q: %w", name, err)
	}
	oldVol := src.Volume
	if err := e.reg.BumpSourceGeneration(src.ID, newVol); err != nil {
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
		return fmt.Errorf("source %q: %w", name, err)
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
	return e.reg.DeleteSource(src.ID)
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
		Image:  "alpine:3.21",
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
