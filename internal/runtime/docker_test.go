package runtime

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/volume"
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

func TestDockerListManagedVolumesReportsCreated(t *testing.T) {
	f, d := newFakeDockerAPI(t)
	f.volumes["v1"] = volume.Volume{Name: "v1", CreatedAt: "2024-05-06T07:08:09Z"}
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
		"failed to set up container networking",
	}
	for _, m := range retry {
		if !isPortRace(errors.New(m)) {
			t.Errorf("isPortRace(%q) = false, want true", m)
		}
	}
	for _, m := range []string{"no such image", "permission denied", ""} {
		if isPortRace(errors.New(m)) {
			t.Errorf("isPortRace(%q) = true, want false", m)
		}
	}
	if isPortRace(nil) {
		t.Error("isPortRace(nil) = true")
	}
}
