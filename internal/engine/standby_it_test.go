package engine

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/abd-ulbasit/pgoverlay/internal/pgctl/pgctltest"
	"github.com/abd-ulbasit/pgoverlay/internal/registry"
	"github.com/abd-ulbasit/pgoverlay/internal/runtime"
)

// TestSeedFromStandbyBranchesArePrimaries seeds a source from a streaming
// replica (the setup the docs recommend) and checks that its branches are
// independent, writable primaries: not in recovery, no WAL receiver, no
// replication settings, no connection back to the primary, and blind to
// writes made on the primary after the seed. Before the fix every branch
// booted as a read-only hot standby streaming from production.
func TestSeedFromStandbyBranchesArePrimaries(t *testing.T) {
	if os.Getenv("PGOVERLAY_IT") != "1" {
		t.Skip("set PGOVERLAY_IT=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	primaryHost, _, network, primaryConn := pgctltest.StartSourcePG(t, ctx)
	mustExec(t, ctx, primaryConn, `CREATE TABLE t(i int); INSERT INTO t SELECT generate_series(1,100); CHECKPOINT`)
	standbyHost, standbyPort, standbyConn := pgctltest.StartStandbyPG(t, ctx, network, primaryHost)
	deadline := time.Now().Add(time.Minute)
	for mustQueryInt(t, ctx, standbyConn, `SELECT count(*) FROM pg_class WHERE relname = 't'`) == 0 ||
		mustQueryInt(t, ctx, standbyConn, `SELECT count(*) FROM t`) != 100 {
		if time.Now().After(deadline) {
			t.Fatal("standby never replayed the seed rows")
		}
		time.Sleep(time.Second)
	}
	if mustQueryInt(t, ctx, standbyConn, `SELECT pg_is_in_recovery()::int`) != 1 {
		t.Fatal("test setup: the replica is not in recovery")
	}

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

	src := &registry.Source{Name: "sby-main", PGVersion: "17", ConnHost: standbyHost, ConnPort: standbyPort,
		ConnUser: "postgres", Network: network}
	if err := e.AddSource(ctx, src, "secret"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.RemoveVolume(context.Background(), src.Volume) })

	b, err := e.CreateBranch(ctx, "sby-pr-1", "sby-main", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.DestroyBranch(context.Background(), "sby-pr-1"); err != nil {
			t.Errorf("destroy sby-pr-1: %v", err)
		}
	})
	br := branchConn(b)

	if mustQueryInt(t, ctx, br, `SELECT pg_is_in_recovery()::int`) != 0 {
		t.Fatal("branch of a standby source is in recovery (a hot standby, not a primary)")
	}
	mustExec(t, ctx, br, `INSERT INTO t VALUES (1000)`)
	if n := mustQueryInt(t, ctx, br, `SELECT count(*) FROM pg_stat_wal_receiver`); n != 0 {
		t.Fatalf("branch runs %d WAL receiver(s)", n)
	}
	if n := mustQueryInt(t, ctx, br, `SELECT count(*) FROM pg_settings
		WHERE name IN ('primary_conninfo', 'primary_slot_name', 'restore_command') AND setting <> ''`); n != 0 {
		t.Fatalf("branch kept %d replication/recovery setting(s)", n)
	}
	// production serves exactly one walsender: the replica, not the branch
	if n := mustQueryInt(t, ctx, primaryConn, `SELECT count(*) FROM pg_stat_replication`); n != 1 {
		t.Fatalf("primary has %d replication clients, want 1 (the replica)", n)
	}

	// writes on production after the seed never reach the branch
	mustExec(t, ctx, primaryConn, `INSERT INTO t SELECT generate_series(101, 200)`)
	time.Sleep(3 * time.Second)
	if n := mustQueryInt(t, ctx, br, `SELECT count(*) FROM t`); n != 101 {
		t.Fatalf("branch rows = %d, want 101 (100 seeded + 1 local insert)", n)
	}
}
