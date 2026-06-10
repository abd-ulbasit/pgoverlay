package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/abd-ulbasit/pgbranch/internal/cow"
	"github.com/abd-ulbasit/pgbranch/internal/registry"
	"github.com/abd-ulbasit/pgbranch/internal/runtime"
)

// CreateBranch is a saga: every step registers a compensation that runs
// (in reverse order) if a later step fails. No orphans, ever.
func (e *Engine) CreateBranch(ctx context.Context, name, sourceName string) (*registry.Branch, error) {
	src, err := e.reg.GetSourceByName(sourceName)
	if err != nil {
		return nil, fmt.Errorf("source %q: %w", sourceName, err)
	}
	if src.State != registry.SourceReady {
		return nil, fmt.Errorf("source %q is %s, not ready", sourceName, src.State)
	}
	plan := cow.PlanBranch(name, src.Volume)
	b := &registry.Branch{Name: name, SourceID: src.ID, RWVolume: plan.RWVolume}
	if err := e.reg.CreateBranch(b); err != nil {
		return nil, err
	}

	var undo []func()
	fail := func(stepErr error) (*registry.Branch, error) {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
		e.reg.TransitionBranch(b.ID, registry.BranchFailed, stepErr.Error())
		return nil, stepErr
	}
	bg := context.WithoutCancel(ctx)

	// 1. rw volume (upper/work + entrypoint script live here)
	if err := e.drv.CreateVolume(ctx, plan.RWVolume, map[string]string{"pgbranch.managed": "true", "pgbranch.branch.id": b.ID}); err != nil {
		return fail(fmt.Errorf("create rw volume: %w", err))
	}
	undo = append(undo, func() { e.drv.RemoveVolume(bg, plan.RWVolume) })

	// 2. write entrypoint into the rw volume
	if err := e.drv.RunHelper(ctx, runtime.HelperSpec{
		Image:  "alpine:3.21",
		Cmd:    []string{"sh", "-c", `printf '%s' "$PGBRANCH_ENTRYPOINT" > /pgbranch/rw/entrypoint.sh && chmod 0755 /pgbranch/rw/entrypoint.sh && mkdir -p /pgbranch/rw/upper /pgbranch/rw/work`},
		Env:    []string{"PGBRANCH_ENTRYPOINT=" + cow.EntrypointScript},
		Mounts: []runtime.Mount{{Volume: plan.RWVolume, Target: cow.RWPath}},
	}); err != nil {
		return fail(fmt.Errorf("install entrypoint: %w", err))
	}

	// 3. branch container
	cid, err := e.drv.StartBranch(ctx, runtime.BranchSpec{
		Name:  "pgbranch-br-" + name,
		Image: e.image(src.PGVersion),
		Env: []string{
			"PGDATA=" + cow.MergedPath,
			"PGBRANCH_LOWERS=" + plan.LowerEnv(),
		},
		Mounts: []runtime.Mount{
			{Volume: src.Volume, Target: "/pgbranch/lower0", ReadOnly: true},
			{Volume: plan.RWVolume, Target: cow.RWPath},
		},
		Entrypoint: []string{"/bin/sh", cow.RWPath + "/entrypoint.sh"},
		Labels: map[string]string{
			"pgbranch.managed": "true", "pgbranch.role": "branch",
			"pgbranch.branch.id": b.ID, "pgbranch.branch.name": name,
		},
	})
	if err != nil {
		return fail(fmt.Errorf("start instance: %w", err))
	}
	undo = append(undo, func() { e.drv.StopRemove(bg, cid) })

	// 4. wait for postgres readiness (covers WAL recovery time)
	if err := e.waitReady(ctx, cid, 90*time.Second); err != nil {
		return fail(fmt.Errorf("instance never became ready: %w", err))
	}

	// 5. record container + host port, mark ready
	info, err := e.drv.Inspect(ctx, cid)
	if err != nil {
		return fail(err)
	}
	if err := e.reg.MarkBranchReady(b.ID, cid, info.Port); err != nil {
		return fail(err)
	}
	return e.reg.GetBranchByName(name)
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

func (e *Engine) DestroyBranch(ctx context.Context, name string) error {
	b, err := e.reg.GetBranchByName(name)
	if err != nil {
		return err
	}
	if err := e.reg.TransitionBranch(b.ID, registry.BranchDestroying, "destroy requested"); err != nil {
		return err
	}
	if b.ContainerID != "" {
		if err := e.drv.StopRemove(ctx, b.ContainerID); err != nil {
			return fmt.Errorf("remove container: %w", err)
		}
	}
	if err := e.drv.RemoveVolume(ctx, b.RWVolume); err != nil {
		return fmt.Errorf("remove rw volume: %w", err)
	}
	return e.reg.TransitionBranch(b.ID, registry.BranchDestroyed, "")
}
