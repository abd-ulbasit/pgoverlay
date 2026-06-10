package engine

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/abd-ulbasit/pgbranch/internal/cow"
	"github.com/abd-ulbasit/pgbranch/internal/pgctl"
	"github.com/abd-ulbasit/pgbranch/internal/registry"
	"github.com/abd-ulbasit/pgbranch/internal/runtime"
)

// TestZFSEndToEndBranching is the zfs-backend twin of TestEndToEndBranching.
// It cannot run on the usual Colima/macOS dev setup (no ZFS in the VM) — see
// docs/zfs.md for the environment recipe and a manual walkthrough.
func TestZFSEndToEndBranching(t *testing.T) {
	if os.Getenv("PGBRANCH_ZFS_IT") != "1" {
		t.Skip("set PGBRANCH_ZFS_IT=1 to run: needs a Linux docker host with the zfs kernel module " +
			"(/dev/zfs present), an imported zpool with a dataset prefix for pgbranch " +
			"(e.g. `zpool create tank ...; zfs create tank/pgbranch`) named in PGBRANCH_ZFS_DATASET, " +
			"default zfs mountpoints, and network access for the helper's `apk add zfs`. " +
			"Not available on Colima/macOS — docs/zfs.md has the manual verification walkthrough.")
	}
	dataset := os.Getenv("PGBRANCH_ZFS_DATASET")
	if dataset == "" {
		t.Fatal("PGBRANCH_ZFS_DATASET must name the dataset prefix, e.g. tank/pgbranch")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
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
	t.Cleanup(func() { r.Close() })
	e := NewWithPlanner(r, d, "postgres:17", cow.Planner{Backend: cow.BackendZFS, Dataset: dataset})

	src := &registry.Source{Name: "zfs-main", PGVersion: "17", ConnHost: host, ConnPort: port, ConnUser: "postgres", Network: network}
	if err := e.AddSource(ctx, src, "secret"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.RemoveSource(context.Background(), "zfs-main"); err != nil {
			t.Errorf("remove source: %v", err)
		}
	})

	start := time.Now()
	b, err := e.CreateBranch(ctx, "zfs-pr-1", "zfs-main", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("zfs branch created in %s", time.Since(start))
	t.Cleanup(func() {
		if err := e.DestroyBranch(context.Background(), "zfs-pr-1"); err != nil {
			t.Errorf("destroy zfs-pr-1: %v", err)
		}
	})

	// branch sees source data
	if n := mustQueryInt(t, ctx, branchConn(b), `SELECT count(*) FROM accounts`); n != 10000 {
		t.Fatalf("branch rows = %d", n)
	}
	// writes to the clone do not affect the source
	mustExec(t, ctx, branchConn(b), `UPDATE accounts SET balance = 0`)
	if n := mustQueryInt(t, ctx, hostConn, `SELECT sum(balance) FROM accounts`); n != 1000000 {
		t.Fatalf("source mutated! sum=%d", n)
	}
	// usage reflects the clone's unique space after writes
	mustExec(t, ctx, branchConn(b), `CHECKPOINT`)
	usage, err := e.BranchUsage(ctx, "zfs-pr-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("clone used bytes after full-table update: %d", usage)
	if usage <= 0 {
		t.Fatalf("BranchUsage = %d, want > 0 after writes", usage)
	}
}
