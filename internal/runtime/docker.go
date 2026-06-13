package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
)

type DockerDriver struct{ cli *client.Client }

func NewDockerDriver() (*DockerDriver, error) {
	opts := []client.Opt{client.FromEnv, client.WithAPIVersionNegotiation()}
	if os.Getenv("DOCKER_HOST") == "" {
		if host := DockerHostFromCLIContext(); host != "" {
			opts = append(opts, client.WithHost(host))
		}
	}
	cli, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &DockerDriver{cli: cli}, nil
}

// DockerHostFromCLIContext resolves the docker endpoint from the CLI's
// current context (~/.docker/config.json + contexts/meta). The Go SDK's
// FromEnv only honors DOCKER_HOST, so without this, setups like Colima
// (where /var/run/docker.sock is absent or stale) fail. Returns "" when
// unresolvable. Exported so integration helpers can prime DOCKER_HOST for
// libraries (e.g. testcontainers) that don't read docker CLI contexts.
func DockerHostFromCLIContext() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	cfg := struct {
		CurrentContext string `json:"currentContext"`
	}{}
	raw, err := os.ReadFile(filepath.Join(home, ".docker", "config.json"))
	if err != nil || json.Unmarshal(raw, &cfg) != nil {
		return ""
	}
	if cfg.CurrentContext == "" || cfg.CurrentContext == "default" {
		return ""
	}
	sum := sha256.Sum256([]byte(cfg.CurrentContext))
	metaPath := filepath.Join(home, ".docker", "contexts", "meta", hex.EncodeToString(sum[:]), "meta.json")
	meta := struct {
		Endpoints map[string]struct {
			Host string `json:"Host"`
		} `json:"Endpoints"`
	}{}
	raw, err = os.ReadFile(metaPath)
	if err != nil || json.Unmarshal(raw, &meta) != nil {
		return ""
	}
	return meta.Endpoints["docker"].Host
}

func (d *DockerDriver) EnsureImage(ctx context.Context, ref string) error {
	if _, err := d.cli.ImageInspect(ctx, ref); err == nil {
		return nil
	}
	rc, err := d.cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	defer rc.Close()
	_, err = io.Copy(io.Discard, rc)
	return err
}

func (d *DockerDriver) CreateVolume(ctx context.Context, name string, labels map[string]string) error {
	_, err := d.cli.VolumeCreate(ctx, volume.CreateOptions{Name: name, Labels: labels})
	return err
}

func (d *DockerDriver) RemoveVolume(ctx context.Context, name string) error {
	return d.cli.VolumeRemove(ctx, name, true)
}

// CloneVolume provisions dst as a copy of src. Docker named volumes have no
// copy-on-write clone primitive, so this is a full `cp -a` through a helper
// container — a generic fallback satisfying the Driver contract; no engine
// flow uses it on docker today (the overlay and zfs backends have cheaper
// mechanisms).
func (d *DockerDriver) CloneVolume(ctx context.Context, src, dst string, labels map[string]string) error {
	if err := d.CreateVolume(ctx, dst, labels); err != nil {
		return fmt.Errorf("clone volume %s -> %s: %w", src, dst, err)
	}
	if _, err := d.RunHelper(ctx, HelperSpec{
		Image: "alpine:3.21",
		Cmd:   []string{"sh", "-c", "cp -a /pgbranch-clone-src/. /pgbranch-clone-dst/"},
		Mounts: []Mount{
			{Volume: src, Target: "/pgbranch-clone-src", ReadOnly: true},
			{Volume: dst, Target: "/pgbranch-clone-dst"},
		},
	}); err != nil {
		d.RemoveVolume(context.WithoutCancel(ctx), dst)
		return fmt.Errorf("clone volume %s -> %s: %w", src, dst, err)
	}
	return nil
}

func toMounts(ms []Mount) []mount.Mount {
	out := make([]mount.Mount, 0, len(ms))
	for _, m := range ms {
		typ := mount.TypeVolume
		if m.Kind == MountHostPath {
			typ = mount.TypeBind
		}
		out = append(out, mount.Mount{Type: typ, Source: m.Volume, Target: m.Target, ReadOnly: m.ReadOnly})
	}
	return out
}

// helperHostConfig renders the host-side container config for a helper:
// mounts, network, and — for zfs helpers — privileged mode with the host
// devices mapped in.
func helperHostConfig(spec HelperSpec) *container.HostConfig {
	host := &container.HostConfig{
		Mounts:      toMounts(spec.Mounts),
		NetworkMode: container.NetworkMode(spec.Network),
		Privileged:  spec.Privileged,
	}
	for _, dev := range spec.HostDevices {
		host.Resources.Devices = append(host.Resources.Devices,
			container.DeviceMapping{PathOnHost: dev, PathInContainer: dev, CgroupPermissions: "rwm"})
	}
	return host
}

func (d *DockerDriver) RunHelper(ctx context.Context, spec HelperSpec) (string, error) {
	if err := d.EnsureImage(ctx, spec.Image); err != nil {
		return "", err
	}
	cfg := &container.Config{Image: spec.Image, Cmd: spec.Cmd, Env: spec.Env, User: spec.User,
		Labels: map[string]string{"pgbranch.managed": "true", "pgbranch.role": "helper"}}
	host := helperHostConfig(spec)
	cr, err := d.cli.ContainerCreate(ctx, cfg, host, nil, nil, "")
	if err != nil {
		return "", fmt.Errorf("create helper: %w", err)
	}
	defer d.cli.ContainerRemove(context.WithoutCancel(ctx), cr.ID, container.RemoveOptions{Force: true, RemoveVolumes: true})
	if err := d.cli.ContainerStart(ctx, cr.ID, container.StartOptions{}); err != nil {
		return "", fmt.Errorf("start helper: %w", err)
	}
	waitC, errC := d.cli.ContainerWait(ctx, cr.ID, container.WaitConditionNotRunning)
	select {
	case err := <-errC:
		return "", err
	case st := <-waitC:
		out := d.logs(ctx, cr.ID)
		if st.StatusCode != 0 {
			return out, fmt.Errorf("helper exited %d: %s", st.StatusCode, out)
		}
		return out, nil
	}
}

func (d *DockerDriver) logs(ctx context.Context, id string) string {
	rc, err := d.cli.ContainerLogs(ctx, id, container.LogsOptions{ShowStdout: true, ShowStderr: true, Tail: "20"})
	if err != nil {
		return ""
	}
	defer rc.Close()
	var buf bytes.Buffer
	stdcopy.StdCopy(&buf, &buf, rc)
	return buf.String()
}

func (d *DockerDriver) StartBranch(ctx context.Context, spec BranchSpec) (string, error) {
	if err := d.EnsureImage(ctx, spec.Image); err != nil {
		return "", err
	}
	cfg := &container.Config{
		Image: spec.Image, Env: spec.Env, Entrypoint: spec.Entrypoint, Labels: spec.Labels,
		ExposedPorts: nat.PortSet{"5432/tcp": struct{}{}},
	}
	host := &container.HostConfig{
		Mounts:        toMounts(spec.Mounts),
		CapAdd:        []string{"SYS_ADMIN"},                                          // overlay mount inside container
		SecurityOpt:   []string{"apparmor=unconfined"},                                // no-op where AppArmor absent
		PortBindings:  nat.PortMap{"5432/tcp": {{HostIP: "127.0.0.1", HostPort: ""}}}, // random host port
		NetworkMode:   container.NetworkMode(spec.Network),
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
	}
	cr, err := d.cli.ContainerCreate(ctx, cfg, host, nil, nil, spec.Name)
	if err != nil {
		return "", fmt.Errorf("create branch container: %w", err)
	}
	if err := d.cli.ContainerStart(ctx, cr.ID, container.StartOptions{}); err != nil {
		d.cli.ContainerRemove(context.WithoutCancel(ctx), cr.ID, container.RemoveOptions{Force: true, RemoveVolumes: true})
		return "", fmt.Errorf("start branch container: %w", err)
	}
	return cr.ID, nil
}

func (d *DockerDriver) Exec(ctx context.Context, id string, cmd []string) error {
	_, err := d.ExecOutput(ctx, id, cmd)
	return err
}

// ExecOutput runs cmd in the container and returns its stdout. The exec API
// multiplexes stdout/stderr over one attached stream (stdcopy); stderr is
// kept separate so captured output (e.g. a pg_dump) stays clean, and is
// embedded in the error on non-zero exit.
func (d *DockerDriver) ExecOutput(ctx context.Context, id string, cmd []string) (string, error) {
	ex, err := d.cli.ContainerExecCreate(ctx, id, container.ExecOptions{Cmd: cmd, AttachStdout: true, AttachStderr: true})
	if err != nil {
		return "", err
	}
	att, err := d.cli.ContainerExecAttach(ctx, ex.ID, container.ExecStartOptions{})
	if err != nil {
		return "", err
	}
	defer att.Close()
	var stdout, stderr bytes.Buffer
	stdcopy.StdCopy(&stdout, &stderr, att.Reader)
	insp, err := d.cli.ContainerExecInspect(ctx, ex.ID)
	if err != nil {
		return stdout.String(), err
	}
	if insp.ExitCode != 0 {
		return stdout.String(), fmt.Errorf("exec %v exited %d: %s%s", cmd, insp.ExitCode, stderr.String(), stdout.String())
	}
	return stdout.String(), nil
}

func (d *DockerDriver) Inspect(ctx context.Context, id string) (ContainerInfo, error) {
	j, err := d.cli.ContainerInspect(ctx, id)
	if err != nil {
		return ContainerInfo{}, err
	}
	info := ContainerInfo{ID: j.ID, Running: j.State != nil && j.State.Running, Host: "127.0.0.1", Labels: j.Config.Labels}
	if b, ok := j.NetworkSettings.Ports["5432/tcp"]; ok && len(b) > 0 {
		info.Port, _ = strconv.Atoi(b[0].HostPort)
	}
	return info, nil
}

func (d *DockerDriver) StopRemove(ctx context.Context, id string) error {
	timeout := 30
	_ = d.cli.ContainerStop(ctx, id, container.StopOptions{Timeout: &timeout})
	err := d.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true, RemoveVolumes: true})
	if client.IsErrNotFound(err) {
		return nil
	}
	return err
}

func (d *DockerDriver) ListManaged(ctx context.Context) ([]ContainerInfo, error) {
	f := filters.NewArgs(filters.Arg("label", "pgbranch.managed=true"), filters.Arg("label", "pgbranch.role=branch"))
	cs, err := d.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return nil, err
	}
	out := make([]ContainerInfo, 0, len(cs))
	for _, c := range cs {
		out = append(out, ContainerInfo{ID: c.ID, Running: c.State == "running", Labels: c.Labels})
	}
	return out, nil
}

func (d *DockerDriver) ListManagedVolumes(ctx context.Context, instanceID string) ([]string, error) {
	f := filters.NewArgs(
		filters.Arg("label", "pgbranch.managed=true"),
		filters.Arg("label", LabelInstance+"="+instanceID),
	)
	resp, err := d.cli.VolumeList(ctx, volume.ListOptions{Filters: f})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(resp.Volumes))
	for _, v := range resp.Volumes {
		out = append(out, v.Name)
	}
	return out, nil
}
