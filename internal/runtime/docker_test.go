package runtime

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
)

func TestToMountsKinds(t *testing.T) {
	got := toMounts([]Mount{
		{Volume: "pgoverlay-src-main", Target: "/pgoverlay/lower0", ReadOnly: true},
		{Kind: MountHostPath, Volume: "/tank/pgoverlay/br-pr-1", Target: "/pgoverlay/rw"},
	})
	if len(got) != 2 {
		t.Fatalf("mounts = %d", len(got))
	}
	if string(got[0].Type) != "volume" || got[0].Source != "pgoverlay-src-main" || !got[0].ReadOnly {
		t.Errorf("mount[0] = %+v, want ro volume pgoverlay-src-main", got[0])
	}
	if string(got[1].Type) != "bind" || got[1].Source != "/tank/pgoverlay/br-pr-1" || got[1].Target != "/pgoverlay/rw" || got[1].ReadOnly {
		t.Errorf("mount[1] = %+v, want rw bind /tank/pgoverlay/br-pr-1", got[1])
	}
}

func TestHelperHostConfigPrivilegedDevices(t *testing.T) {
	// zfs helpers: privileged with /dev/zfs mapped in
	host := helperHostConfig(HelperSpec{
		Privileged:  true,
		HostDevices: []string{"/dev/zfs"},
		Mounts:      []Mount{{Kind: MountHostPath, Volume: "/tank/pgoverlay/src-main-g1", Target: "/seed"}},
		Network:     "bridge",
	})
	if !host.Privileged {
		t.Fatal("want Privileged")
	}
	if len(host.Resources.Devices) != 1 ||
		host.Resources.Devices[0].PathOnHost != "/dev/zfs" ||
		host.Resources.Devices[0].PathInContainer != "/dev/zfs" {
		t.Fatalf("devices = %+v, want /dev/zfs mapped", host.Resources.Devices)
	}
	if string(host.NetworkMode) != "bridge" {
		t.Errorf("network = %q", host.NetworkMode)
	}
	if len(host.Mounts) != 1 || string(host.Mounts[0].Type) != "bind" {
		t.Errorf("mounts = %+v, want one bind mount", host.Mounts)
	}
	// default helpers stay unprivileged with no devices
	plain := helperHostConfig(HelperSpec{Mounts: []Mount{{Volume: "v", Target: "/t"}}})
	if plain.Privileged || len(plain.Resources.Devices) != 0 {
		t.Fatalf("plain helper privileged=%v devices=%v, want false/none", plain.Privileged, plain.Resources.Devices)
	}
}

func itDriver(t *testing.T) Driver {
	t.Helper()
	if os.Getenv("PGOVERLAY_IT") != "1" {
		t.Skip("set PGOVERLAY_IT=1 to run integration tests")
	}
	d, err := NewDockerDriver()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestVolumeAndHelperRoundtrip(t *testing.T) {
	d := itDriver(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	vol := "pgoverlay-test-vol"
	if err := d.CreateVolume(ctx, vol, map[string]string{"pgoverlay.managed": "true"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.RemoveVolume(context.Background(), vol) })

	if err := d.EnsureImage(ctx, "alpine:3.21"); err != nil {
		t.Fatal(err)
	}
	// write a file via one helper, verify via another
	if _, err := d.RunHelper(ctx, HelperSpec{
		Image:  "alpine:3.21",
		Cmd:    []string{"sh", "-c", "echo hello > /data/probe"},
		Mounts: []Mount{{Volume: vol, Target: "/data"}},
	}); err != nil {
		t.Fatal(err)
	}
	// successful helpers return their combined output
	out, err := d.RunHelper(ctx, HelperSpec{
		Image:  "alpine:3.21",
		Cmd:    []string{"cat", "/data/probe"},
		Mounts: []Mount{{Volume: vol, Target: "/data", ReadOnly: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("helper output %q, want it to contain %q", out, "hello")
	}
	// failing helper surfaces output in error
	_, err = d.RunHelper(ctx, HelperSpec{Image: "alpine:3.21", Cmd: []string{"sh", "-c", "echo boom >&2; exit 3"}})
	if err == nil {
		t.Fatal("want error from non-zero helper exit")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("helper error %q does not include captured output", err)
	}
}

// Reconcile acts on Running/Stopped: a stopped container (exited, dead, or
// never started) will not come back by itself, while restarting/removing/
// paused are transitional and must be left alone.
func TestDockerStateMapping(t *testing.T) {
	cases := map[string][2]bool{
		"running":    {true, false},
		"exited":     {false, true},
		"dead":       {false, true},
		"created":    {false, true},
		"restarting": {false, false},
		"removing":   {false, false},
		"paused":     {false, false},
		"":           {false, false},
	}
	for state, want := range cases {
		running, stopped := dockerState(state)
		if running != want[0] || stopped != want[1] {
			t.Errorf("dockerState(%q) = %v/%v, want %v/%v", state, running, stopped, want[0], want[1])
		}
	}
}

// ListManaged reports the published host port of a running container (the
// list API carries it), so reconcile can compare it with the registry without
// one inspect per branch; a stopped container has no address.
func TestSummaryInfo(t *testing.T) {
	up := summaryInfo(container.Summary{ID: "a", State: "running", Status: "Up 2 minutes", Created: 1700000000,
		Ports: []container.Port{{IP: "127.0.0.1", PrivatePort: 5432, PublicPort: 40123, Type: "tcp"}}})
	if !up.Running || up.Stopped || up.Host != "127.0.0.1" || up.Port != 40123 || up.Status != "Up 2 minutes" {
		t.Errorf("running summary = %+v", up)
	}
	if !up.Created.Equal(time.Unix(1700000000, 0)) {
		t.Errorf("Created = %v", up.Created)
	}
	down := summaryInfo(container.Summary{ID: "b", State: "exited", Status: "Exited (137) 1 minute ago"})
	if down.Running || !down.Stopped || down.Host != "" || down.Port != 0 || !down.Created.IsZero() {
		t.Errorf("exited summary = %+v", down)
	}
}

func TestDockerInspectMissingIsNotFound(t *testing.T) {
	_, d := newFakeDockerAPI(t)
	_, err := d.Inspect(context.Background(), "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Inspect(missing) = %v, want ErrNotFound", err)
	}
}

func TestDockerInspectReportsStateAndAddress(t *testing.T) {
	f, d := newFakeDockerAPI(t)
	ctx := context.Background()
	f.containers["c1"] = &fakeContainer{id: "c1", name: "pgoverlay-br-x", state: container.StateRunning,
		host: container.HostConfig{PortBindings: nat.PortMap{"5432/tcp": {{HostIP: "127.0.0.1", HostPort: "40555"}}}}}
	info, err := d.Inspect(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if !info.Running || info.Stopped || info.Host != "127.0.0.1" || info.Port != 40555 || info.Created.IsZero() {
		t.Errorf("running Inspect = %+v", info)
	}
	f.containers["c1"].state = container.StateExited
	info, err = d.Inspect(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if info.Running || !info.Stopped || info.Host != "" || info.Port != 0 || !strings.Contains(info.Status, "exited") {
		t.Errorf("exited Inspect = %+v", info)
	}
}

// CreateVolume must not adopt a volume that already exists (docker's
// VolumeCreate would return it unchanged, data and all).
func TestDockerCreateVolumeRefusesExisting(t *testing.T) {
	f, d := newFakeDockerAPI(t)
	ctx := context.Background()
	f.volumes["pgoverlay-br-p-rw"] = volume.Volume{Name: "pgoverlay-br-p-rw", Labels: map[string]string{"old": "true"}}
	err := d.CreateVolume(ctx, "pgoverlay-br-p-rw", map[string]string{"new": "true"})
	if !errors.Is(err, ErrVolumeExists) {
		t.Fatalf("CreateVolume(existing) = %v, want ErrVolumeExists", err)
	}
	if n := f.called("POST /volumes/create"); n != 0 {
		t.Errorf("VolumeCreate called %d times for an existing name", n)
	}
	if err := d.CreateVolume(ctx, "pgoverlay-br-q-rw", map[string]string{"new": "true"}); err != nil {
		t.Fatalf("CreateVolume(new) = %v", err)
	}
	if v := f.volumes["pgoverlay-br-q-rw"]; v.Labels["new"] != "true" {
		t.Errorf("new volume labels = %v", v.Labels)
	}
}

func TestDockerListManagedVolumesReportsCreated(t *testing.T) {
	f, d := newFakeDockerAPI(t)
	f.volumes["v1"] = volume.Volume{Name: "v1", CreatedAt: "2024-05-06T07:08:09Z",
		Labels: map[string]string{"pgoverlay.managed": "true", LabelInstance: "inst"}}
	vols, err := d.ListManagedVolumes(context.Background(), "inst")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	if len(vols) != 1 || vols[0].Name != "v1" || !vols[0].Created.Equal(want) {
		t.Fatalf("ListManagedVolumes = %+v, want v1 created %v", vols, want)
	}
}

func TestIsPortRace(t *testing.T) {
	retry := []string{
		"start branch container: ... failed to listen on TCP socket: address already in use",
		"driver failed programming external connectivity: port is already allocated",
		"failed to set up container networking: driver failed programming external connectivity on endpoint x: Bind for 127.0.0.1:40001 failed: port is already allocated",
	}
	for _, m := range retry {
		if !isPortRace(errors.New(m)) {
			t.Errorf("isPortRace(%q) = false, want true", m)
		}
	}
	// the generic networking prefix alone is not a port collision: a missing
	// network fails the same way on every attempt
	for _, m := range []string{"no such image", "permission denied", "", "failed to set up container networking: network pgnet not found"} {
		if isPortRace(errors.New(m)) {
			t.Errorf("isPortRace(%q) = true, want false", m)
		}
	}
	if isPortRace(nil) {
		t.Error("isPortRace(nil) = true")
	}
}

func execFake(t *testing.T) (*fakeDockerAPI, *DockerDriver, context.Context) {
	t.Helper()
	f, d := newFakeDockerAPI(t)
	f.containers["c1"] = &fakeContainer{id: "c1", state: container.StateRunning}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return f, d, ctx
}

// The first inspect after the stream ends can still report the exec running
// with the default exit code 0. The result must be the exit code the command
// actually finished with, or masking and credential rotation would pass on a
// failed psql.
func TestExecOutputWaitsForExitCode(t *testing.T) {
	f, d, ctx := execFake(t)
	f.execOut = "partial output\n"
	f.execInspects = []container.ExecInspect{{Running: true, ExitCode: 0}, {Running: true, ExitCode: 0}, {Running: false, ExitCode: 3}}
	_, err := d.ExecOutput(ctx, "c1", []string{"psql", "-c", "select 1"})
	if err == nil || !strings.Contains(err.Error(), "exited 3") {
		t.Fatalf("ExecOutput = %v, want the real exit code 3", err)
	}
	if f.execInspectN < 3 {
		t.Errorf("exec inspected %d times, want polling until it stopped running", f.execInspectN)
	}
}

// A stream that breaks mid-frame is an error, not truncated success (a
// truncated pg_dump would otherwise become a diff's base schema).
func TestExecOutputBrokenStreamIsError(t *testing.T) {
	f, d, ctx := execFake(t)
	f.execTruncate = true
	f.execInspects = []container.ExecInspect{{Running: false, ExitCode: 0}}
	_, err := d.ExecOutput(ctx, "c1", []string{"pg_dump"})
	if err == nil || !strings.Contains(err.Error(), "reading output") {
		t.Fatalf("ExecOutput = %v, want a stream read error", err)
	}
}

func TestDemuxExecStream(t *testing.T) {
	var in bytes.Buffer
	stdcopy.NewStdWriter(&in, stdcopy.Stdout).Write([]byte("out1 "))
	stdcopy.NewStdWriter(&in, stdcopy.Stderr).Write([]byte("err1"))
	stdcopy.NewStdWriter(&in, stdcopy.Stdout).Write([]byte("out2"))
	whole := in.Bytes()

	var out, errOut bytes.Buffer
	if err := demuxExecStream(&out, &errOut, bytes.NewReader(whole)); err != nil {
		t.Fatal(err)
	}
	if out.String() != "out1 out2" || errOut.String() != "err1" {
		t.Errorf("stdout=%q stderr=%q", out.String(), errOut.String())
	}
	// cut inside the last header, and inside the last payload
	for _, cut := range []int{len(whole) - 4 - 8 + 3, len(whole) - 2} {
		err := demuxExecStream(io.Discard, io.Discard, bytes.NewReader(whole[:cut]))
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("cut at %d/%d: err = %v, want io.ErrUnexpectedEOF", cut, len(whole), err)
		}
	}
	var sys bytes.Buffer
	stdcopy.NewStdWriter(&sys, stdcopy.Systemerr).Write([]byte("exec failed"))
	if err := demuxExecStream(io.Discard, io.Discard, &sys); err == nil || !strings.Contains(err.Error(), "exec failed") {
		t.Errorf("systemerr frame: err = %v", err)
	}
}

// Engine commands run as the postgres OS user so `local ... peer` auth
// (the distro-packaged default copied in by pg_basebackup) accepts them.
func TestExecOutputRunsAsPostgres(t *testing.T) {
	f, d, ctx := execFake(t)
	f.execOut = "ok\n"
	f.execInspects = []container.ExecInspect{{Running: false, ExitCode: 0}}
	out, err := d.ExecOutput(ctx, "c1", []string{"pg_isready"})
	if err != nil || out != "ok\n" {
		t.Fatalf("ExecOutput = %q, %v", out, err)
	}
	if len(f.execCreates) != 1 || f.execCreates[0].User != "postgres" {
		t.Fatalf("exec create = %+v, want User postgres", f.execCreates)
	}
}

// scriptPorts makes pickHostPort return the given ports in order.
func scriptPorts(t *testing.T, ports ...int) {
	t.Helper()
	orig := pickHostPort
	t.Cleanup(func() { pickHostPort = orig })
	pickHostPort = func() (int, error) {
		if len(ports) == 0 {
			t.Fatal("pickHostPort called more often than scripted")
		}
		p := ports[0]
		ports = ports[1:]
		return p, nil
	}
}

func publishedPort(t *testing.T, c *fakeContainer) string {
	t.Helper()
	b := c.host.PortBindings["5432/tcp"]
	if len(b) != 1 || b[0].HostIP != "127.0.0.1" {
		t.Fatalf("5432/tcp bindings = %+v, want one loopback binding", b)
	}
	return b[0].HostPort
}

// A branch container publishes 5432 on a host port chosen up front, so the
// binding is part of the container config and a docker restart (restart
// policy, daemon or host reboot) brings it back on the same port.
func TestStartBranchPinsHostPort(t *testing.T) {
	f, d := newFakeDockerAPI(t)
	scriptPorts(t, 40001)
	id, err := d.StartBranch(context.Background(), BranchSpec{Name: "pgoverlay-br-x", Image: "postgres:17"})
	if err != nil {
		t.Fatal(err)
	}
	c := f.containers[id]
	if got := publishedPort(t, c); got != "40001" {
		t.Errorf("HostPort = %q, want the picked port 40001 (not an ephemeral \"\")", got)
	}
	if c.host.RestartPolicy.Name != container.RestartPolicyUnlessStopped {
		t.Errorf("restart policy = %q", c.host.RestartPolicy.Name)
	}
	info, err := d.Inspect(context.Background(), id)
	if err != nil || info.Port != 40001 {
		t.Fatalf("Inspect = %+v, %v; want port 40001", info, err)
	}
}

// A port collision at start is retried on a new port. The failed container is
// removed and waited for before the retry reuses its name, even when the
// daemon finishes removal after the DELETE call returns.
func TestStartBranchRetriesPortCollision(t *testing.T) {
	f, d := newFakeDockerAPI(t)
	scriptPorts(t, 40001, 40002)
	f.startErrs = []string{"failed to set up container networking: Bind for 127.0.0.1:40001 failed: port is already allocated"}
	f.removeLag = 2
	id, err := d.StartBranch(context.Background(), BranchSpec{Name: "pgoverlay-br-x", Image: "postgres:17"})
	if err != nil {
		t.Fatalf("StartBranch after one collision: %v", err)
	}
	if got := publishedPort(t, f.containers[id]); got != "40002" {
		t.Errorf("HostPort = %q, want the second pick 40002", got)
	}
	if len(f.containers) != 1 {
		t.Errorf("containers = %d, want only the started one (failed attempt removed)", len(f.containers))
	}
	if n := f.called("POST /containers/create"); n != 2 {
		t.Errorf("creates = %d, want 2", n)
	}
}

// A networking failure that is not a port collision fails at once.
func TestStartBranchDoesNotRetryOtherNetworkErrors(t *testing.T) {
	f, d := newFakeDockerAPI(t)
	scriptPorts(t, 40001)
	f.startErrs = []string{"failed to set up container networking: network pgnet not found"}
	_, err := d.StartBranch(context.Background(), BranchSpec{Name: "pgoverlay-br-x", Image: "postgres:17"})
	if err == nil || !strings.Contains(err.Error(), "network pgnet not found") {
		t.Fatalf("StartBranch = %v, want the network error", err)
	}
	if n := f.called("POST /containers/create"); n != 1 {
		t.Errorf("creates = %d, want 1 (no retry)", n)
	}
	if len(f.containers) != 0 {
		t.Errorf("failed container left behind: %d", len(f.containers))
	}
}
