// Package runtime abstracts where branch instances run (Docker now, K8s in P3).
package runtime

import "context"

type Mount struct {
	Volume   string
	Target   string
	ReadOnly bool
}

// HelperSpec is a one-shot container performing a data operation
// (seeding, file fixes). Run blocks until exit; non-zero exit = error
// including captured output.
type HelperSpec struct {
	Image   string
	Cmd     []string
	Env     []string
	Mounts  []Mount
	Network string
	User    string // e.g. "postgres" for pg_basebackup so file ownership is uid 999
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
	Port    int // host port mapped to 5432, 0 if none
	Labels  map[string]string
}

type Driver interface {
	EnsureImage(ctx context.Context, image string) error
	CreateVolume(ctx context.Context, name string, labels map[string]string) error
	RemoveVolume(ctx context.Context, name string) error
	RunHelper(ctx context.Context, spec HelperSpec) error
	StartBranch(ctx context.Context, spec BranchSpec) (id string, err error)
	Exec(ctx context.Context, containerID string, cmd []string) error // error on non-zero exit
	Inspect(ctx context.Context, containerID string) (ContainerInfo, error)
	StopRemove(ctx context.Context, containerID string) error
	ListManaged(ctx context.Context) ([]ContainerInfo, error) // label pgbranch.managed=true
}
