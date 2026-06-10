package runtime

import (
	"context"
	"os"
	"testing"
	"time"
)

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
	if err := d.RunHelper(ctx, HelperSpec{
		Image:  "alpine:3.21",
		Cmd:    []string{"sh", "-c", "echo hello > /data/probe"},
		Mounts: []Mount{{Volume: vol, Target: "/data"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.RunHelper(ctx, HelperSpec{
		Image:  "alpine:3.21",
		Cmd:    []string{"sh", "-c", "grep -q hello /data/probe"},
		Mounts: []Mount{{Volume: vol, Target: "/data", ReadOnly: true}},
	}); err != nil {
		t.Fatal(err)
	}
	// failing helper surfaces output in error
	err := d.RunHelper(ctx, HelperSpec{Image: "alpine:3.21", Cmd: []string{"sh", "-c", "echo boom >&2; exit 3"}})
	if err == nil {
		t.Fatal("want error from non-zero helper exit")
	}
}
