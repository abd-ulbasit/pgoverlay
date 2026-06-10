package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/abd-ulbasit/pgbranch/internal/pgctl"
	"github.com/abd-ulbasit/pgbranch/internal/pgproxy"
	"github.com/abd-ulbasit/pgbranch/internal/registry"
	"github.com/abd-ulbasit/pgbranch/internal/runtime"
)

// dockerVolumeExists asks the docker CLI whether a named volume exists —
// the layer-GC assertions need ground truth, not just registry state.
func dockerVolumeExists(t *testing.T, name string) bool {
	t.Helper()
	err := exec.Command("docker", "volume", "inspect", name).Run()
	if err == nil {
		return true
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return false
	}
	t.Fatalf("docker volume inspect %s: %v", name, err)
	return false
}

// TestBranchFromBranchEndToEnd is the decision-3 scenario:
//
//	source -> b1 -> write marker -> b2 from b1 (freeze) ->
//	b2 sees the marker; b1 keeps its data and stays writable; the source is
//	untouched; dbname@b2 routes through the wire proxy; destroying b1 first
//	leaves b2 alive (the frozen layer keeps it); resetting b2 returns it to
//	its base chain (including b1's marker); destroying b2 GCs the layers and
//	the volumes are actually gone.
func TestBranchFromBranchEndToEnd(t *testing.T) {
	if os.Getenv("PGBRANCH_IT") != "1" {
		t.Skip("set PGBRANCH_IT=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	host, port, network, hostConn := pgctl.StartSourcePG(t, ctx)
	mustExec(t, ctx, hostConn, `CREATE TABLE accounts(id int primary key, balance int);
		INSERT INTO accounts SELECT i, 100 FROM generate_series(1,10000) i`)

	d, err := runtime.NewDockerDriver()
	if err != nil {
		t.Fatal(err)
	}
	r, err := registry.Open(t.TempDir() + "/it.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() }) // before destroy cleanups (LIFO)
	e := New(r, d, "postgres:17")

	src := &registry.Source{Name: "bfb-main", PGVersion: "17", ConnHost: host, ConnPort: port, ConnUser: "postgres", Network: network}
	if err := e.AddSource(ctx, src, "secret"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.RemoveVolume(context.Background(), src.Volume) })
	// belt and braces: the branches are destroyed mid-test; tolerate that
	destroyIfLive := func(name string) {
		if err := e.DestroyBranch(context.Background(), name); err != nil && !errors.Is(err, registry.ErrNotFound) {
			t.Errorf("cleanup destroy %s: %v", name, err)
		}
	}
	t.Cleanup(func() { destroyIfLive("bfb-b2"); destroyIfLive("bfb-b1") })

	b1, err := e.CreateBranch(ctx, "bfb-b1", "bfb-main", 0)
	if err != nil {
		t.Fatal(err)
	}
	// the marker written to b1 must be visible in the child but nowhere else
	mustExec(t, ctx, branchConn(b1), `CREATE TABLE marker(v text); INSERT INTO marker VALUES ('from-b1')`)

	start := time.Now()
	b2, err := e.CreateBranchFrom(ctx, "bfb-b2", "bfb-b1", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("branch bfb-b2 created FROM BRANCH bfb-b1 in %s", time.Since(start))

	// child sees the parent's marker and the source data
	if n := mustQueryInt(t, ctx, branchConn(b2), `SELECT count(*) FROM marker WHERE v='from-b1'`); n != 1 {
		t.Fatalf("child marker rows = %d, want 1", n)
	}
	if n := mustQueryInt(t, ctx, branchConn(b2), `SELECT count(*) FROM accounts`); n != 10000 {
		t.Fatalf("child accounts = %d", n)
	}

	// parent survived the freeze: ready, data intact, still writable
	b1, err = r.GetBranchByName("bfb-b1") // freeze restarted it: new container/port
	if err != nil {
		t.Fatal(err)
	}
	if b1.State != registry.BranchReady {
		t.Fatalf("parent state after freeze = %q", b1.State)
	}
	if n := mustQueryInt(t, ctx, branchConn(b1), `SELECT count(*) FROM marker WHERE v='from-b1'`); n != 1 {
		t.Fatalf("parent lost its marker after freeze: %d", n)
	}
	if n := mustQueryInt(t, ctx, branchConn(b1), `SELECT count(*) FROM accounts`); n != 10000 {
		t.Fatalf("parent accounts after freeze = %d", n)
	}
	mustExec(t, ctx, branchConn(b1), `INSERT INTO marker VALUES ('post-freeze')`)
	if n := mustQueryInt(t, ctx, branchConn(b1), `SELECT count(*) FROM marker`); n != 2 {
		t.Fatalf("parent marker rows after post-freeze write = %d, want 2", n)
	}
	// ...and post-freeze parent writes do NOT leak into the child
	if n := mustQueryInt(t, ctx, branchConn(b2), `SELECT count(*) FROM marker`); n != 1 {
		t.Fatalf("child sees post-freeze parent writes: %d marker rows", n)
	}
	// the source never saw any of it
	if n := mustQueryInt(t, ctx, hostConn, `SELECT count(*) FROM information_schema.tables WHERE table_name='marker'`); n != 0 {
		t.Fatal("marker table leaked into the source")
	}
	if n := mustQueryInt(t, ctx, hostConn, `SELECT sum(balance) FROM accounts`); n != 1000000 {
		t.Fatalf("source mutated: sum=%d", n)
	}

	// the wire router resolves dbname@child live
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxyCtx, stopProxy := context.WithCancel(context.Background())
	t.Cleanup(stopProxy)
	go pgproxy.New(&pgproxy.RegistryResolver{Reg: r}).Serve(proxyCtx, lis)
	pconn, err := pgx.Connect(ctx, fmt.Sprintf("postgres://postgres:secret@%s/postgres@bfb-b2", lis.Addr()))
	if err != nil {
		t.Fatalf("connect to child through proxy: %v", err)
	}
	var n int
	if err := pconn.QueryRow(ctx, `SELECT count(*) FROM marker WHERE v='from-b1'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("query child through proxy: n=%d err=%v", n, err)
	}
	pconn.Close(ctx)

	frozenLayerVol := "pgbranch-br-bfb-b1-rw" // b1's original rw, frozen
	parentNewRW := b1.RWVolume                // b1's post-freeze rw (g2)
	if parentNewRW != "pgbranch-br-bfb-b1-rw-g2" {
		t.Fatalf("parent rw after freeze = %q", parentNewRW)
	}

	// destroy the PARENT first: the child must survive via the frozen layer
	if err := e.DestroyBranch(ctx, "bfb-b1"); err != nil {
		t.Fatal(err)
	}
	if dockerVolumeExists(t, parentNewRW) {
		t.Fatal("destroyed parent's own rw volume still exists")
	}
	if !dockerVolumeExists(t, frozenLayerVol) {
		t.Fatal("frozen layer volume GC'd while the child still references it")
	}
	if n := mustQueryInt(t, ctx, branchConn(b2), `SELECT count(*) FROM accounts`); n != 10000 {
		t.Fatalf("child broken after parent destroy: accounts=%d", n)
	}

	// reset the child: back to its OWN base chain — b1's marker is there,
	// the child's own writes are not
	mustExec(t, ctx, branchConn(b2), `INSERT INTO marker VALUES ('from-b2')`)
	b2, err = e.ResetBranch(ctx, "bfb-b2")
	if err != nil {
		t.Fatal(err)
	}
	if n := mustQueryInt(t, ctx, branchConn(b2), `SELECT count(*) FROM marker`); n != 1 {
		t.Fatalf("reset child marker rows = %d, want 1 (only the parent's)", n)
	}
	if n := mustQueryInt(t, ctx, branchConn(b2), `SELECT count(*) FROM marker WHERE v='from-b1'`); n != 1 {
		t.Fatal("reset child lost the parent's marker")
	}

	// destroy the child: the zero-ref layer chain is GC'd, volumes actually gone
	if err := e.DestroyBranch(ctx, "bfb-b2"); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"pgbranch-br-bfb-b2-rw", frozenLayerVol, parentNewRW} {
		if dockerVolumeExists(t, v) {
			t.Errorf("volume %q still exists after final destroy", v)
		}
	}
	if layers, err := r.ListLayersBySource(src.ID); err != nil || len(layers) != 0 {
		t.Fatalf("layer rows after final destroy: %v err=%v", layers, err)
	}
}
