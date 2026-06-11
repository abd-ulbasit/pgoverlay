package engine

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/abd-ulbasit/pgbranch/internal/pgctl"
	"github.com/abd-ulbasit/pgbranch/internal/registry"
	"github.com/abd-ulbasit/pgbranch/internal/runtime"
)

// TestDumpSeededBranching covers the managed-Postgres path: the source is a
// plain postgres:16 (no replication setup needed) seeded with via=dump scoped
// to the public schema, on a newer branch major (17 — pg_dump can dump older
// servers). Branches must see the dumped rows, not see the scoped-out schema,
// and accept writes.
func TestDumpSeededBranching(t *testing.T) {
	if os.Getenv("PGBRANCH_IT") != "1" {
		t.Skip("set PGBRANCH_IT=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	host, port, network, hostConn := pgctl.StartSourcePGVersion(t, ctx, "16")
	mustExec(t, ctx, hostConn, `CREATE TABLE items(id int primary key, name text);
		INSERT INTO items SELECT i, 'item-' || i FROM generate_series(1,5000) i;
		CREATE SCHEMA internal;
		CREATE TABLE internal.secrets(id int primary key, token text);
		INSERT INTO internal.secrets VALUES (1, 'do-not-dump')`)

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

	src := &registry.Source{Name: "dump-main", PGVersion: "17",
		ConnHost: host, ConnPort: port, ConnUser: "postgres", ConnDB: "postgres", Network: network,
		SeedVia: registry.SeedViaDump, DumpSchemas: []string{"public"}}
	start := time.Now()
	if err := e.AddSource(ctx, src, "secret"); err != nil {
		t.Fatal(err)
	}
	t.Logf("dump-seeded source in %s", time.Since(start))
	t.Cleanup(func() { d.RemoveVolume(context.Background(), src.Volume) })

	got, err := r.GetSourceByName("dump-main")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != registry.SourceReady || got.SeedVia != registry.SeedViaDump {
		t.Fatalf("source after seed: %+v", got)
	}

	b, err := e.CreateBranch(ctx, "dump-pr-1", "dump-main", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.DestroyBranch(context.Background(), "dump-pr-1"); err != nil {
			t.Errorf("destroy dump-pr-1: %v", err)
		}
	})

	// the dumped schema arrived in full
	if n := mustQueryInt(t, ctx, branchConn(b), `SELECT count(*) FROM items`); n != 5000 {
		t.Fatalf("branch rows = %d want 5000", n)
	}
	// the scoped-out schema did not
	if n := mustQueryInt(t, ctx, branchConn(b), `SELECT count(*) FROM pg_namespace WHERE nspname = 'internal'`); n != 0 {
		t.Fatal("scoped-out schema 'internal' present in dump-seeded branch")
	}
	// branches are writable and isolated from the remote
	mustExec(t, ctx, branchConn(b), `UPDATE items SET name = 'rewritten'`)
	if n := mustQueryInt(t, ctx, branchConn(b), `SELECT count(*) FROM items WHERE name = 'rewritten'`); n != 5000 {
		t.Fatalf("branch write lost: %d rewritten rows", n)
	}
	if n := mustQueryInt(t, ctx, hostConn, `SELECT count(*) FROM items WHERE name = 'rewritten'`); n != 0 {
		t.Fatalf("remote mutated by branch write: %d rows", n)
	}
}
