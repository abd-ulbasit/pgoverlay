package runtime

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestToMountsKinds(t *testing.T) {
	got := toMounts([]Mount{
		{Volume: "pgbranch-src-main", Target: "/pgbranch/lower0", ReadOnly: true},
		{Kind: MountHostPath, Volume: "/tank/pgbranch/br-pr-1", Target: "/pgbranch/rw"},
	})
	if len(got) != 2 {
		t.Fatalf("mounts = %d", len(got))
	}
	if string(got[0].Type) != "volume" || got[0].Source != "pgbranch-src-main" || !got[0].ReadOnly {
		t.Errorf("mount[0] = %+v, want ro volume pgbranch-src-main", got[0])
	}
	if string(got[1].Type) != "bind" || got[1].Source != "/tank/pgbranch/br-pr-1" || got[1].Target != "/pgbranch/rw" || got[1].ReadOnly {
		t.Errorf("mount[1] = %+v, want rw bind /tank/pgbranch/br-pr-1", got[1])
	}
}

func TestHelperHostConfigPrivilegedDevices(t *testing.T) {
	// zfs helpers: privileged with /dev/zfs mapped in
	host := helperHostConfig(HelperSpec{
		Privileged:  true,
		HostDevices: []string{"/dev/zfs"},
		Mounts:      []Mount{{Kind: MountHostPath, Volume: "/tank/pgbranch/src-main-g1", Target: "/seed"}},
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
	if os.Getenv("PGBRANCH_IT") != "1" {
		t.Skip("set PGBRANCH_IT=1 to run integration tests")
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

	vol := "pgbranch-test-vol"
	if err := d.CreateVolume(ctx, vol, map[string]string{"pgbranch.managed": "true"}); err != nil {
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
