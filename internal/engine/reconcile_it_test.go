package engine

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/jackc/pgx/v5"

	"github.com/abd-ulbasit/pgoverlay/internal/pgctl/pgctltest"
	"github.com/abd-ulbasit/pgoverlay/internal/registry"
	"github.com/abd-ulbasit/pgoverlay/internal/runtime"
)

// TestReconcileGCEndToEnd drives the unified reconcile loop against real
// docker: a healthy source+branch must survive, while a stray managed volume,
// a stuck `creating` registry row and a TTL-expired branch are all cleaned by
// one reconcile pass. Names use a gc- prefix so they cannot collide with other
// IT suites' resources.
func TestReconcileGCEndToEnd(t *testing.T) {
	if os.Getenv("PGOVERLAY_IT") != "1" {
		t.Skip("set PGOVERLAY_IT=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	host, port, network, _ := pgctltest.StartSourcePG(t, ctx)

	d, err := runtime.NewDockerDriver()
	if err != nil {
		t.Fatal(err)
	}
	r, err := registry.Open(t.TempDir() + "/it.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	e := New(r, d, "postgres:17")

	src := &registry.Source{Name: "gc-main", PGVersion: "17", ConnHost: host, ConnPort: port, ConnUser: "postgres", Network: network}
	if err := e.AddSource(ctx, src, "secret"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.RemoveVolume(context.Background(), src.Volume) })

	// a healthy branch that must survive reconcile.
	keep, err := e.CreateBranch(ctx, "gc-keep", "gc-main", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.DestroyBranch(context.Background(), "gc-keep") })

	// a TTL-expired branch the loop should reap (1s TTL, already elapsed).
	if _, err := e.CreateBranch(ctx, "gc-expired", "gc-main", time.Second); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)

	// a stray managed volume owned by no branch (tagged with this registry's
	// instance id so reconcile recognizes it as ITS orphan to reclaim).
	strayVol := "pgoverlay-br-gc-stray-rw"
	if err := d.CreateVolume(ctx, strayVol, map[string]string{"pgoverlay.managed": "true", runtime.LabelInstance: r.InstanceID()}); err != nil {
		t.Fatal(err)
	}

	// a stuck `creating` registry row with its own rw volume.
	stuckVol := "pgoverlay-br-gc-stuck-rw"
	if err := d.CreateVolume(ctx, stuckVol, map[string]string{"pgoverlay.managed": "true", runtime.LabelInstance: r.InstanceID()}); err != nil {
		t.Fatal(err)
	}
	stuck := &registry.Branch{Name: "gc-stuck", SourceID: src.ID, RWVolume: stuckVol, SourceVolume: src.Volume}
	if err := r.CreateBranch(stuck); err != nil {
		t.Fatal(err)
	}

	// one reconcile pass with a future clock so the just-inserted stuck row is
	// past the 10m stuck timeout, and the 1s-TTL branch is expired.
	taken, err := e.ApplyReconcile(ctx, time.Now().Add(time.Hour), 10*time.Minute)
	if err != nil {
		t.Fatalf("reconcile: %v (took %+v)", err, taken.Actions)
	}

	// the healthy branch survives.
	if b, err := r.GetBranchByName("gc-keep"); err != nil || b.State != registry.BranchReady {
		t.Fatalf("healthy branch gc-keep should survive: %+v err=%v", b, err)
	}

	// the stuck row is failed and its rw volume removed.
	if b, err := r.GetBranchByName("gc-stuck"); err != nil || b.State != registry.BranchFailed {
		t.Fatalf("gc-stuck should be failed: %+v err=%v", b, err)
	}
	if volumeExists(t, ctx, d, r.InstanceID(), stuckVol) {
		t.Fatalf("stuck rw volume %q survived reconcile", stuckVol)
	}

	// the stray volume is gone.
	if volumeExists(t, ctx, d, r.InstanceID(), strayVol) {
		t.Fatalf("stray volume %q survived reconcile", strayVol)
	}

	// the TTL-expired branch is reaped (destroyed tombstone or gone).
	if b, err := r.GetBranchByName("gc-expired"); err == nil && b.State != registry.BranchDestroyed {
		t.Fatalf("gc-expired should be reaped, got %+v", b)
	}

	// the keep branch's rw volume and the source volume are still present.
	if !volumeExists(t, ctx, d, r.InstanceID(), keep.RWVolume) {
		t.Fatalf("kept branch rw volume %q was GC'd", keep.RWVolume)
	}
	if !volumeExists(t, ctx, d, r.InstanceID(), src.Volume) {
		t.Fatalf("source volume %q was GC'd", src.Volume)
	}
}

// itDockerClient is a raw docker client for the IT to act on containers
// behind pgoverlay's back (docker restart / rm -f / stop).
func itDockerClient(t *testing.T) *client.Client {
	t.Helper()
	opts := []client.Opt{client.FromEnv, client.WithAPIVersionNegotiation()}
	if os.Getenv("DOCKER_HOST") == "" {
		if h := runtime.DockerHostFromCLIContext(); h != "" {
			opts = append(opts, client.WithHost(h))
		}
	}
	cli, err := client.NewClientWithOpts(opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Close() })
	return cli
}

// eventuallyInt retries a query until the branch accepts connections again
// (postgres restarting after a docker restart) and returns the result.
func eventuallyInt(t *testing.T, ctx context.Context, conn, q string) int {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		c, err := pgx.Connect(ctx, conn)
		if err == nil {
			var n int
			err = c.QueryRow(ctx, q).Scan(&n)
			c.Close(ctx)
			if err == nil {
				return n
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never answered %q: %v", conn, q, err)
		}
		time.Sleep(time.Second)
	}
}

// TestReconcileRepairsDockerDrift is the live counterpart of issue #8 on a
// real docker daemon:
//   - `docker restart` keeps the branch's pinned host port, so there is no
//     drift and the recorded address still works;
//   - `docker rm -f` of a ready branch's container is repaired by reconcile:
//     a new container on the branch's volumes, writes intact;
//   - `docker stop` (which the unless-stopped policy does not undo) likewise.
func TestReconcileRepairsDockerDrift(t *testing.T) {
	if os.Getenv("PGOVERLAY_IT") != "1" {
		t.Skip("set PGOVERLAY_IT=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	host, port, network, _ := pgctltest.StartSourcePG(t, ctx)
	d, err := runtime.NewDockerDriver()
	if err != nil {
		t.Fatal(err)
	}
	r, err := registry.Open(t.TempDir() + "/it.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	e := New(r, d, "postgres:17")
	src := &registry.Source{Name: "drift-main", PGVersion: "17", ConnHost: host, ConnPort: port, ConnUser: "postgres", Network: network}
	if err := e.AddSource(ctx, src, "secret"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.RemoveVolume(context.Background(), src.Volume) })
	b, err := e.CreateBranch(ctx, "drift-pr", "drift-main", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.DestroyBranch(context.Background(), "drift-pr"); err != nil {
			t.Errorf("destroy drift-pr: %v", err)
		}
	})
	mustExec(t, ctx, branchConn(b), `CREATE TABLE kept(x int); INSERT INTO kept VALUES (42); CHECKPOINT`)
	cli := itDockerClient(t)
	driftFor := func(plan ReconcilePlan) []Action {
		var out []Action
		for _, a := range plan.Actions {
			if a.Target == "drift-pr" {
				out = append(out, a)
			}
		}
		return out
	}

	// 1. docker restart: same host port, nothing to repair
	timeout := 10
	if err := cli.ContainerRestart(ctx, b.ContainerID, container.StopOptions{Timeout: &timeout}); err != nil {
		t.Fatal(err)
	}
	if info, err := d.Inspect(ctx, b.ContainerID); err != nil || info.Port != b.Port {
		t.Fatalf("after docker restart: %+v, %v; want host port %d kept", info, err, b.Port)
	}
	if n := eventuallyInt(t, ctx, branchConn(b), `SELECT x FROM kept`); n != 42 {
		t.Fatalf("kept = %d after docker restart", n)
	}
	plan, err := e.PlanReconcile(ctx, time.Now(), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if a := driftFor(plan); len(a) != 0 {
		t.Fatalf("drift after a plain docker restart: %+v", a)
	}

	// 2. docker rm -f: reconcile starts a new container on the same volumes
	if err := cli.ContainerRemove(ctx, b.ContainerID, container.RemoveOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	if plan, _ := e.PlanReconcile(ctx, time.Now(), 10*time.Minute); !hasAction(plan, ActionRestartBranch, "drift-pr") {
		t.Fatalf("doctor misses the removed container: %+v", plan.Actions)
	}
	taken, err := e.ApplyReconcile(ctx, time.Now(), 10*time.Minute)
	if err != nil || !hasAction(taken, ActionRestartBranch, "drift-pr") {
		t.Fatalf("reconcile after rm -f: %+v, %v", taken.Actions, err)
	}
	b2, err := r.GetBranchByName("drift-pr")
	if err != nil || b2.State != registry.BranchReady || b2.ContainerID == b.ContainerID {
		t.Fatalf("after restart: %+v, %v", b2, err)
	}
	if n := mustQueryInt(t, ctx, branchConn(b2), `SELECT x FROM kept`); n != 42 {
		t.Fatalf("kept = %d after the restart: the branch's writes were lost", n)
	}

	// 3. docker stop: not restarted by the policy; reconcile does it
	if err := cli.ContainerStop(ctx, b2.ContainerID, container.StopOptions{Timeout: &timeout}); err != nil {
		t.Fatal(err)
	}
	// Reconcile deliberately leaves a container alone while the runtime still
	// reports it as transitioning (restarting, removing), so wait until docker
	// reports it stopped, and give reconcile a few passes, as the periodic
	// loop would.
	var stopped runtime.ContainerInfo
	for deadline := time.Now().Add(20 * time.Second); ; {
		info, err := d.Inspect(ctx, b2.ContainerID)
		if err == nil && info.Stopped {
			stopped = info
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("container not reported stopped after docker stop: %+v, %v", info, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	var passes []string
	for deadline := time.Now().Add(20 * time.Second); ; {
		taken, err = e.ApplyReconcile(ctx, time.Now(), 10*time.Minute)
		if err == nil && hasAction(taken, ActionRestartBranch, "drift-pr") {
			break
		}
		passes = append(passes, fmt.Sprintf("%+v (err %v)", taken.Actions, err))
		if time.Now().After(deadline) {
			listed, lerr := d.ListManaged(ctx)
			cur, _ := r.GetBranchByName("drift-pr")
			t.Fatalf("reconcile after docker stop never restarted the branch.\ninspect after stop: %+v\nbranch row: %+v\nListManaged: %+v (err %v)\npasses: %v",
				stopped, cur, listed, lerr, passes)
		}
		time.Sleep(time.Second)
	}
	b3, err := r.GetBranchByName("drift-pr")
	if err != nil {
		t.Fatal(err)
	}
	if n := mustQueryInt(t, ctx, branchConn(b3), `SELECT x FROM kept`); n != 42 {
		t.Fatalf("kept = %d after the stop/restart", n)
	}
	if plan, _ := e.PlanReconcile(ctx, time.Now(), 10*time.Minute); len(driftFor(plan)) != 0 {
		t.Fatalf("drift remains: %+v", driftFor(plan))
	}
}

// volumeExists reports whether a managed volume with the given name is still
// listed by the driver.
func volumeExists(t *testing.T, ctx context.Context, d runtime.Driver, instanceID, name string) bool {
	t.Helper()
	vols, err := d.ListManagedVolumes(ctx, instanceID)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vols {
		if v.Name == name {
			return true
		}
	}
	return false
}
