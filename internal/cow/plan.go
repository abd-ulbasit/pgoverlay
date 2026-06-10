// Package cow plans copy-on-write layer layouts for branch containers.
// The overlay mount itself is performed inside the branch container by
// EntrypointScript; host code only decides volume names and mount paths.
package cow

import (
	_ "embed"
	"fmt"
	"strings"
)

//go:embed entrypoint.sh
var EntrypointScript string

//go:embed entrypoint_zfs.sh
var EntrypointScriptZFS string

const (
	MergedPath = "/pgbranch/merged" // PGDATA inside branch container (overlay)
	RWPath     = "/pgbranch/rw"     // branch rw layer mountpoint
	// ZFSDataPath is PGDATA in zfs mode: the clone mountpoint is bind-mounted
	// at RWPath and seeding put the cluster in its data/ subdir.
	ZFSDataPath = RWPath + "/data"
)

// Backend selects the copy-on-write mechanism branches are built on.
type Backend string

const (
	BackendOverlay Backend = "overlay" // OverlayFS assembled inside the branch container (default)
	BackendZFS     Backend = "zfs"     // ZFS snapshot+clone datasets (experimental)
)

// ParseBackend validates a backend name from configuration (branchd --cow).
func ParseBackend(s string) (Backend, error) {
	switch Backend(s) {
	case BackendOverlay, BackendZFS:
		return Backend(s), nil
	}
	return "", fmt.Errorf("unknown cow backend %q (want %q or %q)", s, BackendOverlay, BackendZFS)
}

// Planner yields backend-specific layer names, entrypoints, and (zfs) the
// exact zfs argv the engine runs through privileged helpers. Pure — no I/O.
type Planner struct {
	Backend Backend
	Dataset string // zfs only: dataset prefix all pgbranch datasets live under, e.g. "tank/pgbranch"
}

// SourceLayerName names the layer a source generation is seeded into:
// a docker/kube volume (overlay) or a zfs dataset (zfs).
func (p Planner) SourceLayerName(source string, gen int) string {
	if p.Backend == BackendZFS {
		return fmt.Sprintf("%s/src-%s-g%d", p.Dataset, source, gen)
	}
	return SourceVolumeName(source, gen)
}

// BranchLayerName names a branch's writable layer: the rw volume (overlay)
// or the clone dataset (zfs).
func (p Planner) BranchLayerName(branch string) string {
	if p.Backend == BackendZFS {
		return p.Dataset + "/br-" + branch
	}
	return BranchRWVolumeName(branch)
}

// SnapshotName names the per-branch snapshot on the source dataset.
func (p Planner) SnapshotName(sourceDataset, branch string) string {
	return sourceDataset + "@br-" + branch
}

// Mountpoint maps a dataset to its default zfs mountpoint (/<dataset>).
// The zfs backend requires default mountpoints (no altroot/mountpoint=).
func (p Planner) Mountpoint(dataset string) string { return "/" + dataset }

// Entrypoint returns the branch container entrypoint script for the backend.
func (p Planner) Entrypoint() string {
	if p.Backend == BackendZFS {
		return EntrypointScriptZFS
	}
	return EntrypointScript
}

// zfs argv builders — the engine wraps these into privileged helpers.

func (p Planner) ZFSCreate(dataset string) []string {
	return []string{"zfs", "create", "-p", dataset}
}
func (p Planner) ZFSSnapshot(sourceDataset, branch string) []string {
	return []string{"zfs", "snapshot", p.SnapshotName(sourceDataset, branch)}
}
func (p Planner) ZFSClone(sourceDataset, branch string) []string {
	return []string{"zfs", "clone", p.SnapshotName(sourceDataset, branch), p.BranchLayerName(branch)}
}
func (p Planner) ZFSDestroyClone(branch string) []string {
	return []string{"zfs", "destroy", "-r", p.BranchLayerName(branch)}
}
func (p Planner) ZFSDestroySnapshot(sourceDataset, branch string) []string {
	return []string{"zfs", "destroy", "-r", p.SnapshotName(sourceDataset, branch)}
}
func (p Planner) ZFSDestroyDataset(dataset string) []string {
	return []string{"zfs", "destroy", "-r", dataset}
}
func (p Planner) ZFSUsed(dataset string) []string {
	return []string{"zfs", "list", "-Hp", "-o", "used", dataset}
}

type Plan struct {
	SourceVolume string   // mounted ro at /pgbranch/lower0
	RWVolume     string   // upper+work live here
	Lowers       []string // in overlay order, topmost first
}

// SourceVolumeName names a source's seed volume for a given generation.
// Generation 1 keeps the legacy Phase 1 name so existing volumes keep working.
func SourceVolumeName(source string, gen int) string {
	if gen <= 1 {
		return "pgbranch-src-" + source
	}
	return fmt.Sprintf("pgbranch-src-%s-g%d", source, gen)
}
func BranchRWVolumeName(branch string) string { return "pgbranch-br-" + branch + "-rw" }

func PlanBranch(branchName, sourceVolume string) Plan {
	return Plan{
		SourceVolume: sourceVolume,
		RWVolume:     BranchRWVolumeName(branchName),
		// Seeding writes the cluster into a data/ subdir of the source volume
		// (pg_basebackup creates it 0700-owned by uid 999), so the overlay
		// lower is <mountpoint>/data.
		Lowers: []string{"/pgbranch/lower0/data"},
	}
}

// LowerEnv renders PGBRANCH_LOWERS for the entrypoint (colon-separated).
func (p Plan) LowerEnv() string { return strings.Join(p.Lowers, ":") }
