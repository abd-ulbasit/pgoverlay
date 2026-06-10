package engine

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/abd-ulbasit/pgbranch/internal/pgctl"
	"github.com/abd-ulbasit/pgbranch/internal/registry"
	"github.com/abd-ulbasit/pgbranch/internal/runtime"
)

func branchConn(b *registry.Branch) string {
	return fmt.Sprintf("postgres://postgres:secret@localhost:%d/postgres", b.Port)
}

func mustQueryInt(t *testing.T, ctx context.Context, conn string, q string) int {
	t.Helper()
	c, err := pgx.Connect(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	var n int
	if err := c.QueryRow(ctx, q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func mustExec(t *testing.T, ctx context.Context, conn string, q string) {
	t.Helper()
	c, err := pgx.Connect(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	if _, err := c.Exec(ctx, q); err != nil {
		t.Fatal(err)
	}
}

func TestEndToEndBranching(t *testing.T) {
	if os.Getenv("PGBRANCH_IT") != "1" {
		t.Skip("set PGBRANCH_IT=1")
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
	// Registered before the destroy cleanups below so it runs after them
	// (cleanups are LIFO; a plain defer would close the registry first and
	// silently break every DestroyBranch in cleanup).
	t.Cleanup(func() { r.Close() })
	e := New(r, d, "postgres:17")

	src := &registry.Source{Name: "e2e-main", PGVersion: "17", ConnHost: host, ConnPort: port, ConnUser: "postgres", Network: network}
	if err := e.AddSource(ctx, src, "secret"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.RemoveVolume(context.Background(), src.Volume) })

	start := time.Now()
	b1, err := e.CreateBranch(ctx, "e2e-pr-1", "e2e-main", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("branch pr-1 created in %s", time.Since(start))
	t.Cleanup(func() {
		if err := e.DestroyBranch(context.Background(), "e2e-pr-1"); err != nil {
			t.Errorf("destroy pr-1: %v", err)
		}
	})

	// branch sees source data
	if n := mustQueryInt(t, ctx, branchConn(b1), `SELECT count(*) FROM accounts`); n != 10000 {
		t.Fatalf("branch rows = %d", n)
	}
	// writes to branch do not affect source
	mustExec(t, ctx, branchConn(b1), `UPDATE accounts SET balance = 0`)
	if n := mustQueryInt(t, ctx, hostConn, `SELECT sum(balance) FROM accounts`); n != 1000000 {
		t.Fatalf("source mutated! sum=%d", n)
	}
	// second branch is isolated from first
	b2, err := e.CreateBranch(ctx, "e2e-pr-2", "e2e-main", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.DestroyBranch(context.Background(), "e2e-pr-2"); err != nil {
			t.Errorf("destroy pr-2: %v", err)
		}
	})
	if n := mustQueryInt(t, ctx, branchConn(b2), `SELECT sum(balance) FROM accounts`); n != 1000000 {
		t.Fatalf("pr-2 saw pr-1 writes, sum=%d", n)
	}
}

func TestMaskingAppliedToBranchNotSource(t *testing.T) {
	if os.Getenv("PGBRANCH_IT") != "1" {
		t.Skip("set PGBRANCH_IT=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	host, port, network, hostConn := pgctl.StartSourcePG(t, ctx)
	mustExec(t, ctx, hostConn, `CREATE TABLE accounts(id int primary key, email text);
		INSERT INTO accounts SELECT i, 'user' || i || '@corp.example' FROM generate_series(1,1000) i`)

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

	src := &registry.Source{Name: "mask-main", PGVersion: "17", ConnHost: host, ConnPort: port, ConnUser: "postgres", Network: network}
	if err := e.AddSource(ctx, src, "secret"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.RemoveVolume(context.Background(), src.Volume) })

	if err := r.SetMaskScripts(src.ID, []registry.MaskScript{{
		Name: "mask-emails.sql",
		SQL:  `UPDATE accounts SET email = 'masked-' || id || '@example.invalid'`,
	}}); err != nil {
		t.Fatal(err)
	}

	b, err := e.CreateBranch(ctx, "mask-pr-1", "mask-main", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.DestroyBranch(context.Background(), "mask-pr-1"); err != nil {
			t.Errorf("destroy mask-pr-1: %v", err)
		}
	})

	// every branch row is masked before the branch was marked ready
	if n := mustQueryInt(t, ctx, branchConn(b), `SELECT count(*) FROM accounts WHERE email NOT LIKE 'masked-%@example.invalid'`); n != 0 {
		t.Fatalf("%d unmasked rows in branch", n)
	}
	if n := mustQueryInt(t, ctx, branchConn(b), `SELECT count(*) FROM accounts`); n != 1000 {
		t.Fatalf("branch rows = %d", n)
	}
	// the source keeps its original data
	if n := mustQueryInt(t, ctx, hostConn, `SELECT count(*) FROM accounts WHERE email LIKE 'user%@corp.example'`); n != 1000 {
		t.Fatalf("source mutated: only %d original rows", n)
	}
}
