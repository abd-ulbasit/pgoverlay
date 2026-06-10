// Package engine orchestrates branch lifecycle as sagas over the registry,
// cow planner, and runtime driver. The CLI (P1) and branchd (P2) both embed it.
package engine

import (
	"context"
	"errors"
	"fmt"
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
}

func New(reg *registry.Registry, drv runtime.Driver, defaultImage string) *Engine {
	return &Engine{reg: reg, drv: drv, defaultImage: defaultImage}
}

func (e *Engine) image(pgVersion string) string {
	if pgVersion == "" {
		return e.defaultImage
	}
	return "postgres:" + pgVersion
}

// AddSource registers a source and seeds it from the given live Postgres.
func (e *Engine) AddSource(ctx context.Context, s *registry.Source, password string) error {
	s.Volume = cow.SourceVolumeName(s.Name, 1)
	if err := e.reg.CreateSource(s); err != nil {
		return err
	}
	if err := e.drv.CreateVolume(ctx, s.Volume, map[string]string{"pgbranch.managed": "true", "pgbranch.source.name": s.Name}); err != nil {
		e.reg.SetSourceState(s.ID, registry.SourceFailed, "volume create failed")
		return err
	}
	err := pgctl.Seed(ctx, e.drv, pgctl.SeedSpec{
		Image: e.image(s.PGVersion), Volume: s.Volume, Network: s.Network,
		Host: s.ConnHost, Port: s.ConnPort, User: s.ConnUser, Password: password,
	})
	if err != nil {
		e.drv.RemoveVolume(context.WithoutCancel(ctx), s.Volume)
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
	newVol := cow.SourceVolumeName(name, src.Generation+1)
	if err := e.drv.CreateVolume(ctx, newVol, map[string]string{"pgbranch.managed": "true", "pgbranch.source.name": name}); err != nil {
		return err
	}
	err = pgctl.Seed(ctx, e.drv, pgctl.SeedSpec{
		Image: e.image(src.PGVersion), Volume: newVol, Network: src.Network,
		Host: src.ConnHost, Port: src.ConnPort, User: src.ConnUser, Password: password,
	})
	if err != nil {
		e.drv.RemoveVolume(context.WithoutCancel(ctx), newVol)
		return fmt.Errorf("refresh source %q: %w", name, err)
	}
	oldVol := src.Volume
	if err := e.reg.BumpSourceGeneration(src.ID, newVol); err != nil {
		return err
	}
	e.gcSourceVolume(ctx, src.ID, oldVol)
	return nil
}

// RemoveSource deletes a source's volume and registry row. Refused while any
// live branch still uses the source.
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
	if err := e.drv.RemoveVolume(ctx, src.Volume); err != nil && src.State == registry.SourceReady {
		// failed sources may have no volume (seed cleanup removed it)
		return fmt.Errorf("remove source volume: %w", err)
	}
	return e.reg.DeleteSource(src.ID)
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
			e.drv.RemoveVolume(ctx, b.RWVolume)
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
