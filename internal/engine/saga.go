package engine

import (
	"context"
	"errors"
	"fmt"
	"regexp"
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

// validateBranchName enforces the cross-runtime naming rule on new branches.
// Stored names (reset/destroy paths) are assumed valid: they passed this
// check when created.
func validateBranchName(name string) error {
	if !branchNameRe.MatchString(name) {
		return fmt.Errorf("%w %q: must match [a-z0-9][a-z0-9-]{0,40} (lowercase letters, digits and hyphens, starting with a letter or digit, at most 41 characters)", ErrInvalidName, name)
	}
	return nil
}

// CreateBranch is a saga: every step registers a compensation that runs
// (in reverse order) if a later step fails. No orphans, ever.
// ttl 0 means the branch never expires.
func (e *Engine) CreateBranch(ctx context.Context, name, sourceName string, ttl time.Duration) (*registry.Branch, error) {
	if err := validateBranchName(name); err != nil {
		return nil, err
	}
	src, err := e.reg.GetSourceByName(sourceName)
	if err != nil {
		return nil, fmt.Errorf("source %q: %w", sourceName, err)
	}
	if src.State != registry.SourceReady {
		return nil, fmt.Errorf("source %q is %s, not ready", sourceName, src.State)
	}
	expiresAt := ""
	if ttl > 0 {
		expiresAt = time.Now().Add(ttl).UTC().Format(time.RFC3339)
	}
	b := &registry.Branch{
		Name: name, SourceID: src.ID, RWVolume: cow.BranchRWVolumeName(name),
		SourceVolume: src.Volume, ExpiresAt: expiresAt,
	}
	if err := e.reg.CreateBranch(b); err != nil {
		return nil, err
	}
	if err := e.provision(ctx, b, src); err != nil {
		e.reg.TransitionBranch(b.ID, registry.BranchFailed, err.Error())
		return nil, err
	}
	return e.reg.GetBranchByName(name)
}

// provision runs the resource steps shared by create and reset: rw volume,
// entrypoint install, branch container, readiness wait, masking, mark ready.
// Every step registers a compensation that unwinds (in reverse order) on
// failure; the caller owns the state transition to failed.
func (e *Engine) provision(ctx context.Context, b *registry.Branch, src *registry.Source) error {
	plan := cow.PlanBranch(b.Name, b.SourceVolume)

	var undo []func()
	fail := func(stepErr error) error {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
		return stepErr
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
		Name:  "pgbranch-br-" + b.Name,
		Image: e.image(src.PGVersion),
		Env: []string{
			"PGDATA=" + cow.MergedPath,
			"PGBRANCH_LOWERS=" + plan.LowerEnv(),
		},
		Mounts: []runtime.Mount{
			{Volume: plan.SourceVolume, Target: "/pgbranch/lower0", ReadOnly: true},
			{Volume: plan.RWVolume, Target: cow.RWPath},
		},
		Entrypoint: []string{"/bin/sh", cow.RWPath + "/entrypoint.sh"},
		Labels: map[string]string{
			"pgbranch.managed": "true", "pgbranch.role": "branch",
			"pgbranch.branch.id": b.ID, "pgbranch.branch.name": b.Name,
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

	// 5. apply the source's masking scripts inside the fresh clone (so the
	// branch never serves unmasked data); reset re-runs this because it
	// re-clones from the source. A failing script fails the branch.
	if err := e.applyMasking(ctx, cid, src); err != nil {
		return fail(err)
	}

	// 6. record container + address, mark ready
	info, err := e.drv.Inspect(ctx, cid)
	if err != nil {
		return fail(err)
	}
	if err := e.reg.MarkBranchReady(b.ID, cid, info.Host, info.Port); err != nil {
		return fail(err)
	}
	return nil
}

// ResetBranch throws away a ready branch's writes and reprovisions it from
// its recorded source volume on the same registry row (ready -> resetting ->
// ready; new container id and host port).
func (e *Engine) ResetBranch(ctx context.Context, name string) (*registry.Branch, error) {
	b, err := e.reg.GetBranchByName(name)
	if err != nil {
		return nil, err
	}
	src, err := e.reg.GetSourceByID(b.SourceID)
	if err != nil {
		return nil, err
	}
	if err := e.reg.TransitionBranch(b.ID, registry.BranchResetting, "reset requested"); err != nil {
		return nil, err
	}
	fail := func(stepErr error) (*registry.Branch, error) {
		e.reg.TransitionBranch(b.ID, registry.BranchFailed, stepErr.Error())
		return nil, stepErr
	}
	if b.ContainerID != "" {
		if err := e.drv.StopRemove(ctx, b.ContainerID); err != nil {
			return fail(fmt.Errorf("remove container: %w", err))
		}
	}
	if err := e.drv.RemoveVolume(ctx, b.RWVolume); err != nil {
		return fail(fmt.Errorf("remove rw volume: %w", err))
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
	user, db := src.ConnUser, src.ConnDB
	if user == "" {
		user = "postgres"
	}
	if db == "" {
		db = "postgres"
	}
	for _, sc := range scripts {
		if err := e.drv.Exec(ctx, cid, []string{"psql", "-v", "ON_ERROR_STOP=1", "-U", user, "-d", db, "-c", sc.SQL}); err != nil {
			return fmt.Errorf("masking script %q: %w", sc.Name, err)
		}
	}
	return nil
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
	if err := e.reg.TransitionBranch(b.ID, registry.BranchDestroyed, ""); err != nil {
		return err
	}
	// the destroyed branch may have been the last reference to an
	// old-generation source volume
	e.gcSourceVolume(ctx, b.SourceID, b.SourceVolume)
	return nil
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
	e.drv.RemoveVolume(ctx, volume)
}
