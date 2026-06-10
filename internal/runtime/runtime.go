// Package runtime abstracts where branch instances run (Docker now, K8s in P3).
package runtime

import "context"

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
}

// BranchSpec is a long-running branch Postgres container.
type BranchSpec struct {
	Name       string // container name, e.g. pgbranch-br-pr-1
	Image      string
	Env        []string
	Mounts     []Mount
	Entrypoint []string // overrides image entrypoint
	Labels     map[string]string
	Network    string
}

type ContainerInfo struct {
	ID      string
	Running bool
	Host    string // address the instance is reachable on (127.0.0.1 for docker, pod IP for k8s)
	Port    int    // port on Host (docker: host port mapped to 5432, 0 if none)
	Labels  map[string]string
}

type Driver interface {
	EnsureImage(ctx context.Context, image string) error
	CreateVolume(ctx context.Context, name string, labels map[string]string) error
	RemoveVolume(ctx context.Context, name string) error
	RunHelper(ctx context.Context, spec HelperSpec) (output string, err error)
	StartBranch(ctx context.Context, spec BranchSpec) (id string, err error)
	Exec(ctx context.Context, containerID string, cmd []string) error // error on non-zero exit
	Inspect(ctx context.Context, containerID string) (ContainerInfo, error)
	StopRemove(ctx context.Context, containerID string) error
	ListManaged(ctx context.Context) ([]ContainerInfo, error) // label pgbranch.managed=true
}
