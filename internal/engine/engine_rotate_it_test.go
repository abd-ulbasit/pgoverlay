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

// mustConnFail asserts that a connection string is rejected (wrong password
// must not authenticate).
func mustConnFail(t *testing.T, ctx context.Context, conn, why string) {
	t.Helper()
	c, err := pgx.Connect(ctx, conn)
	if err == nil {
		c.Close(ctx)
		t.Fatalf("connect unexpectedly succeeded: %s", why)
	}
}

// TestRotateCredentialsEndToEnd exercises per-branch credential rotation
// against real docker: the branch accepts ONLY its rotated password (the
// source's password stops working on the branch), the source is untouched,
// and a reset rotates again (the first rotated password stops working).
// Branch/source names are rot- prefixed for parallel-run safety.
func TestRotateCredentialsEndToEnd(t *testing.T) {
	if os.Getenv("PGBRANCH_IT") != "1" {
		t.Skip("set PGBRANCH_IT=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	host, port, network, hostConn := pgctl.StartSourcePG(t, ctx)
	mustExec(t, ctx, hostConn, `CREATE TABLE accounts(id int primary key);
		INSERT INTO accounts SELECT i FROM generate_series(1,100) i`)

	d, err := runtime.NewDockerDriver()
	if err != nil {
		t.Fatal(err)
	}
	r, err := registry.Open(t.TempDir() + "/it.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	e := New(r, d, "postgres:17", WithCredentialRotation())

	src := &registry.Source{Name: "rot-main", PGVersion: "17", ConnHost: host, ConnPort: port, ConnUser: "postgres", Network: network}
	if err := e.AddSource(ctx, src, "secret"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.RemoveVolume(context.Background(), src.Volume) })

	b, err := e.CreateBranch(ctx, "rot-pr-1", "rot-main", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.DestroyBranch(context.Background(), "rot-pr-1"); err != nil {
			t.Errorf("destroy rot-pr-1: %v", err)
		}
	})
	if !hex32.MatchString(b.Password) {
		t.Fatalf("branch Password=%q want 32 hex chars", b.Password)
	}

	conn := func(pw string) string {
		return fmt.Sprintf("postgres://postgres:%s@localhost:%d/postgres", pw, b.Port)
	}
	// the NEW password works on the branch
	if n := mustQueryInt(t, ctx, conn(b.Password), `SELECT count(*) FROM accounts`); n != 100 {
		t.Fatalf("branch rows = %d", n)
	}
	// the SOURCE password does NOT work on the branch anymore
	mustConnFail(t, ctx, conn("secret"), "source password on a rotated branch")
	// the source still accepts its own password
	if n := mustQueryInt(t, ctx, hostConn, `SELECT count(*) FROM accounts`); n != 100 {
		t.Fatalf("source rows = %d", n)
	}

	// reset rotates a FRESH password: the first one stops working
	first := b.Password
	b2, err := e.ResetBranch(ctx, "rot-pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if !hex32.MatchString(b2.Password) || b2.Password == first {
		t.Fatalf("reset Password=%q (first %q) want a fresh 32-hex secret", b2.Password, first)
	}
	conn2 := func(pw string) string {
		return fmt.Sprintf("postgres://postgres:%s@localhost:%d/postgres", pw, b2.Port)
	}
	if n := mustQueryInt(t, ctx, conn2(b2.Password), `SELECT count(*) FROM accounts`); n != 100 {
		t.Fatalf("branch rows after reset = %d", n)
	}
	mustConnFail(t, ctx, conn2(first), "pre-reset rotated password after reset")
	mustConnFail(t, ctx, conn2("secret"), "source password after reset")
}
