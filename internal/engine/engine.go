// Package engine orchestrates branch lifecycle as sagas over the registry,
// cow planner, and runtime driver. The CLI (P1) and branchd (P2) both embed it.
package engine

import (
	"context"
	"fmt"

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
	s.Volume = cow.SourceVolumeName(s.Name)
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
