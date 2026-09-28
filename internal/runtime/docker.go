package runtime

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

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

// NewDockerDriver builds a client for the Docker endpoint the docker CLI would
// use: DOCKER_HOST (with DOCKER_CERT_PATH / DOCKER_TLS_VERIFY), else the
// current CLI context, including its TLS material. Endpoints the SDK cannot
// dial (ssh://) are rejected with an explanation instead of failing later
// with a misleading HTTP error.
func NewDockerDriver() (*DockerDriver, error) {
	opts := []client.Opt{client.FromEnv, client.WithAPIVersionNegotiation()}
	if host := os.Getenv("DOCKER_HOST"); host != "" {
		if err := checkDockerHost(host, "DOCKER_HOST"); err != nil {
			return nil, err
		}
	} else {
		c, err := currentCLIContext()
		if err != nil {
			return nil, err
		}
		if c.Host != "" {
			if err := checkDockerHost(c.Host, fmt.Sprintf("docker context %q", c.Name)); err != nil {
				return nil, err
			}
			tlsOpt, err := contextTLSOpt(c)
			if err != nil {
				return nil, err
			}
			if tlsOpt != nil {
				opts = append(opts, tlsOpt) // before WithHost, which configures this transport
			}
			opts = append(opts, client.WithHost(c.Host))
		}
	}
	cli, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &DockerDriver{cli: cli}, nil
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

// CreateVolume creates an empty named volume. Docker's VolumeCreate is
// idempotent on the name — it hands back an existing volume unchanged, labels
// and data included — so the name is checked first and an existing volume is
// an error wrapping ErrVolumeExists. Volume names are derived from branch and
// source names, so adopting silently would give a recreated branch the writes
// (or the frozen layer) of whatever last used the name.
func (d *DockerDriver) CreateVolume(ctx context.Context, name string, labels map[string]string) error {
	if _, err := d.cli.VolumeInspect(ctx, name); err == nil {
		return fmt.Errorf("create volume %s: %w", name, ErrVolumeExists)
	} else if !client.IsErrNotFound(err) {
		return fmt.Errorf("create volume %s: %w", name, err)
	}
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
		Image: UtilityImage,
		Cmd:   []string{"sh", "-c", "cp -a /pgoverlay-clone-src/. /pgoverlay-clone-dst/"},
		Mounts: []Mount{
			{Volume: src, Target: "/pgoverlay-clone-src", ReadOnly: true},
			{Volume: dst, Target: "/pgoverlay-clone-dst"},
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
		Labels: map[string]string{"pgoverlay.managed": "true", "pgoverlay.role": "helper"}}
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
		CapAdd:        []string{"SYS_ADMIN"},           // overlay mount inside container
		SecurityOpt:   []string{"apparmor=unconfined"}, // no-op where AppArmor absent
		NetworkMode:   container.NetworkMode(spec.Network),
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
	}
	// The host port is chosen here and published explicitly, not left to
	// docker's ephemeral allocator (HostPort ""). Docker re-runs that
	// allocator on every start, so with the unless-stopped restart policy a
	// daemon or host restart brought the branch back on a different port
	// while the registry, the proxy and `pgb connect` kept the old one. An
	// explicit HostPort is part of the container's config and survives
	// restarts.
	//
	// The chosen port can still be taken by another process before the start
	// binds it ("address already in use" / "port is already allocated"), so
	// that specific failure is retried with a new port; any other error is
	// returned. The failed container is removed and waited for (StopRemove)
	// before the retry reuses its name.
	var lastErr error
	for attempt := 0; attempt < startBranchAttempts; attempt++ {
		port, err := pickHostPort()
		if err != nil {
			return "", fmt.Errorf("pick host port: %w", err)
		}
		host.PortBindings = nat.PortMap{"5432/tcp": {{HostIP: "127.0.0.1", HostPort: strconv.Itoa(port)}}}
		cr, err := d.cli.ContainerCreate(ctx, cfg, host, nil, nil, spec.Name)
		if err != nil {
			return "", fmt.Errorf("create branch container: %w", err)
		}
		if err := d.cli.ContainerStart(ctx, cr.ID, container.StartOptions{}); err != nil {
			lastErr = fmt.Errorf("start branch container: %w", err)
			if rmErr := d.StopRemove(context.WithoutCancel(ctx), cr.ID); rmErr != nil {
				return "", fmt.Errorf("%w (removing the failed container: %v)", lastErr, rmErr)
			}
			if isPortRace(err) {
				continue
			}
			return "", lastErr
		}
		return cr.ID, nil
	}
	return "", lastErr
}

// startBranchAttempts bounds StartBranch's retries on a host-port collision.
const startBranchAttempts = 5

// pickHostPort chooses the host port a branch container publishes 5432 on.
// It asks the kernel for a free loopback port and releases it for docker to
// bind; the small window in which another process can take it is covered by
// StartBranch's retry. A variable so tests can script the choice.
var pickHostPort = func() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// isPortRace reports whether a container-start error is a host-port
// collision worth retrying with another port. Only the two collision
// messages count: "failed to set up container networking" also prefixes
// unrelated failures (a missing network, a bad driver option) that a retry
// cannot fix.
func isPortRace(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "address already in use") ||
		strings.Contains(msg, "port is already allocated")
}

func (d *DockerDriver) Exec(ctx context.Context, id string, cmd []string) error {
	_, err := d.ExecOutput(ctx, id, cmd)
	return err
}

// execUser is the OS user in-container commands run as. Every engine exec is
// a Postgres client (psql, pg_dump, pg_isready) talking to the branch over its
// local socket, and a source whose pg_hba.conf authenticates local
// connections with `peer` (the Debian/Ubuntu packaging default) only lets the
// OS user postgres in as the postgres role. Docker's default exec user is the
// container's (root, for the postgres images), which peer auth rejects.
const execUser = "postgres"

// ExecOutput runs cmd in the container as execUser and returns its stdout.
// The exec API multiplexes stdout/stderr over one attached stream (stdcopy);
// stderr is kept separate so captured output (e.g. a pg_dump) stays clean,
// and is embedded in the error on non-zero exit.
//
// Success needs both a complete stream and a finished command with exit code
// 0. A stream that breaks early (a dropped connection to the daemon) is an
// error rather than truncated output, and the exit code is read only once the
// daemon reports the exec is no longer running: ExecInspect's ExitCode is 0
// until then, which would pass a command that has not finished (or that will
// fail) as a success.
func (d *DockerDriver) ExecOutput(ctx context.Context, id string, cmd []string) (string, error) {
	ex, err := d.cli.ContainerExecCreate(ctx, id, container.ExecOptions{User: execUser, Cmd: cmd, AttachStdout: true, AttachStderr: true})
	if err != nil {
		return "", err
	}
	att, err := d.cli.ContainerExecAttach(ctx, ex.ID, container.ExecStartOptions{})
	if err != nil {
		return "", err
	}
	defer att.Close()
	var stdout, stderr bytes.Buffer
	if err := demuxExecStream(&stdout, &stderr, att.Reader); err != nil {
		return stdout.String(), fmt.Errorf("exec %v: reading output: %w", cmd, err)
	}
	code, err := d.execExitCode(ctx, ex.ID)
	if err != nil {
		return stdout.String(), fmt.Errorf("exec %v: %w", cmd, err)
	}
	if code != 0 {
		return stdout.String(), fmt.Errorf("exec %v exited %d: %s%s", cmd, code, stderr.String(), stdout.String())
	}
	return stdout.String(), nil
}

// demuxExecStream splits docker's multiplexed attach stream (8-byte header:
// stream id, 3 zero bytes, big-endian payload length; then the payload) into
// stdout and stderr. Unlike stdcopy.StdCopy, which returns success when the
// stream ends in the middle of a frame, a partial header or payload is
// io.ErrUnexpectedEOF: truncated output must not pass for complete output.
// EOF on a frame boundary is the normal end of the stream.
func demuxExecStream(stdout, stderr io.Writer, r io.Reader) error {
	var hdr [8]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		size := int64(binary.BigEndian.Uint32(hdr[4:]))
		var dst io.Writer
		switch stdcopy.StdType(hdr[0]) {
		case stdcopy.Stdin, stdcopy.Stdout:
			dst = stdout
		case stdcopy.Stderr:
			dst = stderr
		case stdcopy.Systemerr:
			var msg bytes.Buffer
			if _, err := io.CopyN(&msg, r, size); err != nil {
				return unexpectedEOF(err)
			}
			return fmt.Errorf("daemon: %s", msg.String())
		default:
			return fmt.Errorf("unrecognized stream header %d", hdr[0])
		}
		if _, err := io.CopyN(dst, r, size); err != nil {
			return unexpectedEOF(err)
		}
	}
}

func unexpectedEOF(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

// execExitCode waits until the daemon reports the exec finished and returns
// its exit code. The attached stream ending and the exec being marked done are
// separate events on the daemon side, so the first inspect can still see it
// running; poll (bounded by ctx) instead of trusting a single read.
func (d *DockerDriver) execExitCode(ctx context.Context, execID string) (int, error) {
	delay := 10 * time.Millisecond
	for {
		insp, err := d.cli.ContainerExecInspect(ctx, execID)
		if err != nil {
			return 0, err
		}
		if !insp.Running {
			return insp.ExitCode, nil
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("waiting for exec to finish: %w", ctx.Err())
		case <-time.After(delay):
		}
		if delay < 250*time.Millisecond {
			delay *= 2
		}
	}
}

func (d *DockerDriver) Inspect(ctx context.Context, id string) (ContainerInfo, error) {
	j, err := d.cli.ContainerInspect(ctx, id)
	if err != nil {
		if client.IsErrNotFound(err) {
			return ContainerInfo{}, fmt.Errorf("container %s: %w: %w", id, ErrNotFound, err)
		}
		return ContainerInfo{}, err
	}
	info := ContainerInfo{ID: j.ID}
	if j.Config != nil {
		info.Labels = j.Config.Labels
	}
	if j.State != nil {
		info.Running, info.Stopped = dockerState(j.State.Status)
		info.Status = j.State.Status
		if info.Stopped && j.State.Status != container.StateCreated {
			info.Status = fmt.Sprintf("%s (%d)", j.State.Status, j.State.ExitCode)
		}
	}
	if t, err := time.Parse(time.RFC3339Nano, j.Created); err == nil {
		info.Created = t
	}
	if info.Running {
		info.Host = "127.0.0.1"
		if j.NetworkSettings != nil {
			if b, ok := j.NetworkSettings.Ports["5432/tcp"]; ok && len(b) > 0 {
				info.Port, _ = strconv.Atoi(b[0].HostPort)
			}
		}
	}
	return info, nil
}

// dockerState maps a docker container state onto ContainerInfo's Running and
// Stopped. exited, dead and created (never started) are Stopped: with the
// unless-stopped restart policy docker brings back a container that crashed,
// but not one that was stopped, failed to start or was never started.
// restarting, removing and paused are neither — docker is still acting on the
// container (or a person paused it on purpose).
func dockerState(state string) (running, stopped bool) {
	switch state {
	case container.StateRunning:
		return true, false
	case container.StateExited, container.StateDead, container.StateCreated:
		return false, true
	}
	return false, false
}

// summaryInfo converts a ContainerList entry. The list API reports published
// ports only for a running container, which is when they are current.
func summaryInfo(c container.Summary) ContainerInfo {
	info := ContainerInfo{ID: c.ID, Status: c.Status, Labels: c.Labels}
	if c.Status == "" {
		info.Status = c.State
	}
	info.Running, info.Stopped = dockerState(c.State)
	if c.Created > 0 {
		info.Created = time.Unix(c.Created, 0)
	}
	if info.Running {
		info.Host = "127.0.0.1"
		for _, p := range c.Ports {
			if p.PrivatePort == 5432 && p.PublicPort != 0 && (p.Type == "" || p.Type == "tcp") {
				info.Port = int(p.PublicPort)
				break
			}
		}
	}
	return info
}

// StopRemove stops and removes the container and waits until it is actually
// gone, matching KubeDriver.StopRemove. Idempotent: NotFound is success.
//
// The wait is load-bearing, not politeness. ContainerRemove returns once the
// daemon has ACCEPTED the removal, while teardown continues in the background
// and the container keeps its reference on the rw volume until it finishes.
// Every caller in internal/engine follows StopRemove with RemoveVolume on that
// volume (saga.go, csi.go, freeze.go, reconcile.go), so returning early makes
// the next call fail with "volume is in use - [<container id>]". That is a
// timing-dependent failure: it needs the volume removal to land inside the
// teardown window, so it passes most runs and fails perhaps one in three.
// KubeDriver already waits and documents why; this is the same contract, which
// the interface has always implied because of how its callers are written.
func (d *DockerDriver) StopRemove(ctx context.Context, id string) error {
	timeout := 30
	_ = d.cli.ContainerStop(ctx, id, container.StopOptions{Timeout: &timeout})
	err := d.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true, RemoveVolumes: true})
	// Idempotent: gone already, or another caller (e.g. a concurrent reconcile
	// pass racing an explicit destroy) is already removing it — both mean the
	// container is going away, which is the intent.
	if err != nil && !client.IsErrNotFound(err) && !strings.Contains(err.Error(), "already in progress") {
		return err
	}
	// Poll rather than ContainerWait(WaitConditionRemoved): the container may
	// already be gone by the time we get here (the NotFound and
	// already-in-progress paths above), and ContainerWait on a missing
	// container is an error rather than an immediate success.
	deadline := time.NewTimer(removeWait)
	defer deadline.Stop()
	for {
		_, err := d.cli.ContainerInspect(ctx, id)
		if client.IsErrNotFound(err) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for container %s to be removed: %w", id, ctx.Err())
		case <-deadline.C:
			if err != nil {
				return fmt.Errorf("container %s: removal not confirmed within %s: %w", id, removeWait, err)
			}
			return fmt.Errorf("container %s still exists %s after its removal was requested", id, removeWait)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// removeWait bounds StopRemove's wait for the container to disappear,
// independently of ctx: saga compensations run on context.WithoutCancel and
// reconcile on the process-lifetime context, so a container that never goes
// away (a concurrent removal that failed after answering "already in
// progress", a daemon that stopped answering) must not hang them forever.
var removeWait = 2 * time.Minute

func (d *DockerDriver) ListManaged(ctx context.Context) ([]ContainerInfo, error) {
	return d.listByRole(ctx, "branch")
}

// ListHelpers lists every pgoverlay helper container on the daemon, whichever
// instance ran it (helpers carry no instance label).
func (d *DockerDriver) ListHelpers(ctx context.Context) ([]ContainerInfo, error) {
	return d.listByRole(ctx, "helper")
}

func (d *DockerDriver) listByRole(ctx context.Context, role string) ([]ContainerInfo, error) {
	f := filters.NewArgs(filters.Arg("label", "pgoverlay.managed=true"), filters.Arg("label", "pgoverlay.role="+role))
	cs, err := d.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return nil, err
	}
	out := make([]ContainerInfo, 0, len(cs))
	for _, c := range cs {
		out = append(out, summaryInfo(c))
	}
	return out, nil
}

func (d *DockerDriver) ListManagedVolumes(ctx context.Context, instanceID string) ([]VolumeInfo, error) {
	f := filters.NewArgs(
		filters.Arg("label", "pgoverlay.managed=true"),
		filters.Arg("label", LabelInstance+"="+instanceID),
	)
	resp, err := d.cli.VolumeList(ctx, volume.ListOptions{Filters: f})
	if err != nil {
		return nil, err
	}
	out := make([]VolumeInfo, 0, len(resp.Volumes))
	for _, v := range resp.Volumes {
		info := VolumeInfo{Name: v.Name}
		if t, err := time.Parse(time.RFC3339, v.CreatedAt); err == nil {
			info.Created = t
		}
		out = append(out, info)
	}
	return out, nil
}
