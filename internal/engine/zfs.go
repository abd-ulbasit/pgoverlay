package engine

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/abd-ulbasit/pgbranch/internal/cow"
	"github.com/abd-ulbasit/pgbranch/internal/registry"
	"github.com/abd-ulbasit/pgbranch/internal/runtime"
)

// ZFS backend (experimental): layer operations become zfs commands run in
// privileged one-shot helpers with the host's /dev/zfs mapped in. The cow
// planner builds the argv; this file wraps them into HelperSpecs and gives
// the engine backend-neutral layer primitives.

// zfsHelperImage runs the zfs userland. Alpine has none baked in, so each
// helper installs the zfs package at run time — it must be reachable (apk
// network access) and version-compatible with the host's zfs kernel module.
// Documented in docs/zfs.md.
const zfsHelperImage = "alpine:3.21"

// shellSafeRe matches argv words that need no quoting (dataset names,
// snapshot names, flags). Anything else is single-quoted.
var shellSafeRe = regexp.MustCompile(`^[A-Za-z0-9@%_+=:,./-]+$`)

func shellQuoteArg(a string) string {
	if shellSafeRe.MatchString(a) {
		return a
	}
	return "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
}

func shellJoin(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = shellQuoteArg(a)
	}
	return strings.Join(q, " ")
}

func zfsSpec(script string) runtime.HelperSpec {
	return runtime.HelperSpec{
		Image:       zfsHelperImage,
		Cmd:         []string{"sh", "-c", "apk add --quiet zfs && " + script},
		Privileged:  true,
		HostDevices: []string{"/dev/zfs"},
	}
}

// zfsHelperSpec wraps a planner-built zfs argv in a privileged helper.
func zfsHelperSpec(argv []string) runtime.HelperSpec {
	return zfsSpec(shellJoin(argv))
}

// zfsDestroySpec is zfsHelperSpec made idempotent: destroying an already-
// absent dataset or snapshot succeeds (parity with `docker volume rm -f`,
// which DestroyBranch relies on for failed branches), while a destroy that
// fails with the target still present — e.g. a busy clone — stays an error.
func zfsDestroySpec(argv []string) runtime.HelperSpec {
	target := shellQuoteArg(argv[len(argv)-1])
	return zfsSpec(fmt.Sprintf("{ %s || ! zfs list -t all %s >/dev/null 2>&1; }", shellJoin(argv), target))
}

func (e *Engine) zfs() bool { return e.planner.Backend == cow.BackendZFS }

func (e *Engine) runZFS(ctx context.Context, spec runtime.HelperSpec) error {
	_, err := e.drv.RunHelper(ctx, spec)
	return err
}

// Backend-neutral layer primitives. Overlay: driver volumes. ZFS: datasets.

// createSourceLayer provisions the layer a source generation is seeded into.
func (e *Engine) createSourceLayer(ctx context.Context, name string, labels map[string]string) error {
	if e.zfs() {
		return e.runZFS(ctx, zfsHelperSpec(e.planner.ZFSCreate(name)))
	}
	return e.drv.CreateVolume(ctx, name, labels)
}

// removeSourceLayer tears a source layer down (idempotent in both backends).
func (e *Engine) removeSourceLayer(ctx context.Context, name string) error {
	if e.zfs() {
		return e.runZFS(ctx, zfsDestroySpec(e.planner.ZFSDestroyDataset(name)))
	}
	return e.drv.RemoveVolume(ctx, name)
}

// removeBranchLayer tears down a branch's writable layer: the rw volume
// (overlay), or the clone followed by its origin snapshot (zfs — the clone
// depends on the snapshot, so order matters).
func (e *Engine) removeBranchLayer(ctx context.Context, b *registry.Branch) error {
	if e.zfs() {
		if err := e.runZFS(ctx, zfsDestroySpec(e.planner.ZFSDestroyClone(b.Name))); err != nil {
			return fmt.Errorf("destroy zfs clone: %w", err)
		}
		if err := e.runZFS(ctx, zfsDestroySpec(e.planner.ZFSDestroySnapshot(b.SourceVolume, b.Name))); err != nil {
			return fmt.Errorf("destroy zfs snapshot: %w", err)
		}
		return nil
	}
	return e.drv.RemoveVolume(ctx, b.RWVolume)
}

// seedTarget maps a source layer to the SeedSpec target: the volume itself
// (overlay) or the dataset's mountpoint bind-mounted into the helpers (zfs).
func (e *Engine) seedTarget(layer string) (volume string, kind runtime.MountKind) {
	if e.zfs() {
		return e.planner.Mountpoint(layer), runtime.MountHostPath
	}
	return layer, runtime.MountVolume
}
