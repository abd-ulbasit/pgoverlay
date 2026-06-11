// Package engine orchestrates branch lifecycle as sagas over the registry,
// cow planner, and runtime driver. The CLI (P1) and branchd (P2) both embed it.
package engine

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/abd-ulbasit/pgbranch/internal/cow"
	"github.com/abd-ulbasit/pgbranch/internal/pgctl"
	"github.com/abd-ulbasit/pgbranch/internal/registry"
	"github.com/abd-ulbasit/pgbranch/internal/runtime"
)

type Engine struct {
	reg          *registry.Registry
	drv          runtime.Driver
	defaultImage string
	planner      cow.Planner
}

// New builds an engine on the default OverlayFS backend.
func New(reg *registry.Registry, drv runtime.Driver, defaultImage string) *Engine {
	return NewWithPlanner(reg, drv, defaultImage, cow.Planner{Backend: cow.BackendOverlay})
}

// NewWithPlanner selects the copy-on-write backend (branchd --cow).
func NewWithPlanner(reg *registry.Registry, drv runtime.Driver, defaultImage string, planner cow.Planner) *Engine {
	return &Engine{reg: reg, drv: drv, defaultImage: defaultImage, planner: planner}
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
	s.Volume = e.planner.SourceLayerName(s.Name, 1)
	if err := e.reg.CreateSource(s); err != nil {
		return err
	}
	if err := e.createSourceLayer(ctx, s.Volume, map[string]string{"pgbranch.managed": "true", "pgbranch.source.name": s.Name}); err != nil {
		e.reg.SetSourceState(s.ID, registry.SourceFailed, "source layer create failed")
		return err
	}
	if err := e.seedSource(ctx, s, s.Volume, password); err != nil {
		e.removeSourceLayer(context.WithoutCancel(ctx), s.Volume)
		e.reg.SetSourceState(s.ID, registry.SourceFailed, err.Error())
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
	if err := e.createSourceLayer(ctx, newVol, map[string]string{"pgbranch.managed": "true", "pgbranch.source.name": name}); err != nil {
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

// ReapExpired destroys every ready/failed branch whose TTL has passed.
// Called by branchd's reaper loop; now is injected for testability.
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

// RunReaper destroys expired branches every interval until ctx is done.
// branchd runs it as a goroutine; logf (optional, nil = silent) receives
// destroy/error reports.
func (e *Engine) RunReaper(ctx context.Context, interval time.Duration, logf func(format string, args ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			destroyed, err := e.ReapExpired(ctx, now)
			if len(destroyed) > 0 {
				logf("reaper: destroyed expired branches %v", destroyed)
			}
			if err != nil {
				logf("reaper: %v", err)
			}
		}
	}
}

// Reconcile aligns the registry with reality at startup: stuck 'creating'
// branches are failed and their resources cleaned; managed containers with
// no registry row are removed.
func (e *Engine) Reconcile(ctx context.Context) error {
	branches, err := e.reg.ListLiveBranches()
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, b := range branches {
		if b.ContainerID != "" {
			known[b.ContainerID] = true
		}
		if b.State == registry.BranchCreating {
			if b.ContainerID != "" {
				e.drv.StopRemove(ctx, b.ContainerID)
			}
			e.removeBranchLayer(ctx, b)
			e.reg.TransitionBranch(b.ID, registry.BranchFailed, "reconcile: interrupted create")
		}
	}
	managed, err := e.drv.ListManaged(ctx)
	if err != nil {
		return err
	}
	for _, c := range managed {
		if !known[c.ID] {
			e.drv.StopRemove(ctx, c.ID)
		}
	}
	return nil
}
