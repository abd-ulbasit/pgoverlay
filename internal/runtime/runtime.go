// Package runtime abstracts where branch instances run (Docker now, K8s in P3).
package runtime

import (
	"context"
	"errors"
	"time"
)

// LabelInstance tags every managed resource with the owning registry's
// instance id (registry.InstanceID()). Reconcile reclaims only resources
// carrying ITS id, so concurrent pgoverlay instances sharing one Docker daemon
// (and the parallel IT suite) never GC each other's live containers/volumes.
const LabelInstance = "pgoverlay.instance"

// LabelBranchID tags a branch container/pod (and its rw volume) with the id of
// the registry row it belongs to. Reconcile uses it to recognise a container a
// live saga has started but not yet recorded on the row.
const LabelBranchID = "pgoverlay.branch.id"

// ErrNotFound is wrapped into the error Inspect returns when the container or
// pod does not exist, so callers can tell "gone" from "could not ask" with
// errors.Is.
var ErrNotFound = errors.New("not found")

// ErrVolumeExists is wrapped into the error CreateVolume returns when a volume
// of that name already exists. Creating never adopts an existing volume: a
// stale volume left behind under a reused name would otherwise become a new
// branch's writable layer, with the old branch's writes in it.
var ErrVolumeExists = errors.New("volume already exists")

// MountKind selects how Mount.Volume is interpreted.
type MountKind string

const (
	// MountVolume (the zero value) names a managed volume: a docker named
	// volume, or a dataRoot subdirectory on the kube storage node.
	MountVolume MountKind = ""
	// MountHostPath bind-mounts an absolute host path. Used by the zfs
	// backend to mount dataset mountpoints; the path must already exist.
	MountHostPath MountKind = "hostpath"
)

type Mount struct {
	Kind     MountKind
	Volume   string // volume name (MountVolume) or absolute host path (MountHostPath)
	Target   string
	ReadOnly bool
}

// HelperSpec is a one-shot container performing a data operation
// (seeding, file fixes, measurements). Run blocks until exit and returns the
// captured combined output; non-zero exit = error including that output.
type HelperSpec struct {
	Image   string
	Cmd     []string
	Env     []string
	Mounts  []Mount
	Network string
	User    string // e.g. "postgres" for pg_basebackup so file ownership is uid 999
	// Privileged runs the helper with full privileges, with HostDevices
	// mapped in (zfs backend: /dev/zfs). The docker driver maps the devices
	// explicitly; on kube a privileged container sees host devices anyway.
	Privileged  bool
	HostDevices []string
	// SysAdmin runs the helper with the privileges an overlay branch
	// container has, and no more: CAP_SYS_ADMIN (to mount an overlay) with
	// the AppArmor profile, and on kube the seccomp profile, unconfined. The
	// copy-up probe uses it. Ignored when Privileged is set.
	SysAdmin bool
}

// BranchSpec is a long-running branch Postgres container.
type BranchSpec struct {
	Name       string // container name, e.g. pgoverlay-br-pr-1
	Image      string
	Env        []string
	Mounts     []Mount
	Entrypoint []string // overrides image entrypoint
	Labels     map[string]string
	Network    string
}

// ContainerInfo describes one branch or helper instance as the runtime sees it.
//
// Running and Stopped are not complements. Running means the instance is up
// and Host/Port are current. Stopped means it is down and the runtime will not
// bring it back on its own: a docker container that exited, died or was
// created but never started, or a pod in phase Failed (for example evicted)
// or Succeeded. Neither being set means the runtime is still working on it
// (docker restarting, a Pending or terminating pod): look again later.
type ContainerInfo struct {
	ID      string
	Running bool
	Stopped bool
	Status  string // the runtime's own description of the state, for messages
	Host    string // address the instance is reachable on (127.0.0.1 for docker, pod IP for k8s); "" unless Running
	Port    int    // port on Host (docker: host port mapped to 5432, 0 if none)
	Created time.Time
	Labels  map[string]string
}

// VolumeInfo is one managed volume as ListManagedVolumes reports it.
type VolumeInfo struct {
	Name string
	// Created is when the volume was created; zero when the runtime cannot
	// tell. Reconcile never garbage-collects a volume younger than its grace
	// period, because a saga in another process may have just created it.
	Created time.Time
	// Leftover marks storage the runtime no longer has a volume for: a
	// directory under the docker driver's volume root (WithVolumeRoot) whose
	// volume is gone, because a removal stopped between deleting the volume
	// and deleting its directory, or someone removed the volume by hand.
	// Nothing can mount it (a container would get a fresh, empty volume of
	// that name), so it counts as missing everywhere except garbage
	// collection, where RemoveVolume deletes the directory.
	Leftover bool
}

type Driver interface {
	EnsureImage(ctx context.Context, image string) error
	// CreateVolume provisions an empty volume carrying labels. It does not
	// adopt an existing volume of the same name: the docker driver returns an
	// error wrapping ErrVolumeExists, and a csi PVC create fails with
	// AlreadyExists.
	CreateVolume(ctx context.Context, name string, labels map[string]string) error
	RemoveVolume(ctx context.Context, name string) error
	// CloneVolume provisions dst as a copy of src (dst must not exist; labels
	// land on dst). Copy-on-write where the storage supports it (kube csi:
	// PVC dataSource clone / snapshot restore); a full copy elsewhere.
	CloneVolume(ctx context.Context, src, dst string, labels map[string]string) error
	RunHelper(ctx context.Context, spec HelperSpec) (output string, err error)
	StartBranch(ctx context.Context, spec BranchSpec) (id string, err error)
	Exec(ctx context.Context, containerID string, cmd []string) error // error on non-zero exit
	// ExecOutput is Exec with the command's stdout captured and returned
	// (stderr goes into the error on failure). Used where the engine needs
	// in-container command output (pg_dump, row-count probes) against a
	// RUNNING instance — RunHelper can also capture output but spins a new
	// container, which cannot reach an instance's local socket.
	ExecOutput(ctx context.Context, containerID string, cmd []string) (string, error)
	// Inspect reports one instance. A missing container/pod is an error
	// wrapping ErrNotFound.
	Inspect(ctx context.Context, containerID string) (ContainerInfo, error)
	StopRemove(ctx context.Context, containerID string) error
	ListManaged(ctx context.Context) ([]ContainerInfo, error) // labels pgoverlay.managed=true, pgoverlay.role=branch
	// ListHelpers returns every pgoverlay helper container/pod (labels
	// pgoverlay.managed=true, pgoverlay.role=helper), running or not. Helpers
	// are removed by the process that ran them; reconcile uses this to find
	// the ones a crashed process left behind.
	ListHelpers(ctx context.Context) ([]ContainerInfo, error)
	// ListManagedVolumes returns every volume carrying both the
	// pgoverlay.managed=true label AND pgoverlay.instance=<instanceID> (docker
	// named volumes / kube PVCs). Reconcile uses it to find orphaned rw and
	// source-layer volumes owned by THIS registry; scoping by instance id keeps
	// one instance from GC'ing another's volumes on a shared daemon. The zfs
	// backend manages datasets, not driver volumes, so its driver may return an
	// empty list (zfs orphans are GC'd via the per-branch/source paths instead).
	ListManagedVolumes(ctx context.Context, instanceID string) ([]VolumeInfo, error)
}
