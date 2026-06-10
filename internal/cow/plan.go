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

// EntrypointScriptDirect runs postgres straight on a writable copy-on-write
// view of the data dir — zfs clones and csi PVC clones; no overlay assembly.
//
//go:embed entrypoint_direct.sh
var EntrypointScriptDirect string

const (
	MergedPath = "/pgbranch/merged" // PGDATA inside branch container (overlay)
	RWPath     = "/pgbranch/rw"     // branch rw layer mountpoint
	// DirectDataPath is PGDATA in the direct (zfs/csi) modes: the writable
	// clone (dataset mountpoint or PVC) is mounted at RWPath and seeding put
	// the cluster in its data/ subdir.
	DirectDataPath = RWPath + "/data"
)

// Backend selects the copy-on-write mechanism branches are built on.
type Backend string

const (
	BackendOverlay Backend = "overlay" // OverlayFS assembled inside the branch container (default)
	BackendZFS     Backend = "zfs"     // ZFS snapshot+clone datasets (experimental)
	BackendCSI     Backend = "csi"     // Kubernetes PVC clones (--runtime kube --kube-storage csi only)
)

// ParseBackend validates a backend name from configuration (branchd --cow).
func ParseBackend(s string) (Backend, error) {
	switch Backend(s) {
	case BackendOverlay, BackendZFS, BackendCSI:
		return Backend(s), nil
	}
	return "", fmt.Errorf("unknown cow backend %q (want %q, %q or %q)", s, BackendOverlay, BackendZFS, BackendCSI)
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
// zfs and csi branches run directly on their writable clone; only overlay
// branches assemble a mount in-container.
func (p Planner) Entrypoint() string {
	if p.Backend == BackendZFS || p.Backend == BackendCSI {
		return EntrypointScriptDirect
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
	LayerVolumes []string // frozen layer volumes, NEWEST first, mounted ro at /pgbranch/lower1..N
	Lowers       []string // in overlay order, topmost (newest) first, source last
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

// BranchRWVolumeNameGen names a branch's writable volume after gen-1 freezes:
// every freeze turns the current rw volume into an immutable layer (keeping
// its name) and moves the branch onto a fresh volume. Gen 1 is the original
// (legacy) name.
func BranchRWVolumeNameGen(branch string, gen int) string {
	if gen <= 1 {
		return BranchRWVolumeName(branch)
	}
	return fmt.Sprintf("%s-g%d", BranchRWVolumeName(branch), gen)
}

// LowerMountTarget is the in-container mount point of lower layer i
// (0 = the source volume, 1..N = frozen layer volumes, newest first).
func LowerMountTarget(i int) string { return fmt.Sprintf("/pgbranch/lower%d", i) }

// PlanBranch lays out a branch's overlay: rwVolume holds upper/work,
// layerVolumes are the branch's frozen layer chain (NEWEST first, possibly
// empty), and the overlay lowerdir stacks newest layer first with the source
// volume last.
func PlanBranch(rwVolume, sourceVolume string, layerVolumes []string) Plan {
	lowers := make([]string, 0, len(layerVolumes)+1)
	for i := range layerVolumes {
		// A frozen rw volume contains upper/, work/ and entrypoint.sh; only
		// its upper/ subdir is overlay content.
		lowers = append(lowers, LowerMountTarget(i+1)+"/upper")
	}
	// Seeding writes the cluster into a data/ subdir of the source volume
	// (pg_basebackup creates it 0700-owned by uid 999), so the overlay
	// lower is <mountpoint>/data.
	lowers = append(lowers, LowerMountTarget(0)+"/data")
	return Plan{
		SourceVolume: sourceVolume,
		RWVolume:     rwVolume,
		LayerVolumes: layerVolumes,
		Lowers:       lowers,
	}
}

// LowerEnv renders PGBRANCH_LOWERS for the entrypoint (colon-separated).
func (p Plan) LowerEnv() string { return strings.Join(p.Lowers, ":") }
