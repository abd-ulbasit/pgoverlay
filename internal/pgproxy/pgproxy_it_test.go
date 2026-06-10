package pgproxy_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/abd-ulbasit/pgbranch/internal/engine"
	"github.com/abd-ulbasit/pgbranch/internal/pgctl"
	"github.com/abd-ulbasit/pgbranch/internal/pgproxy"
	"github.com/abd-ulbasit/pgbranch/internal/registry"
	"github.com/abd-ulbasit/pgbranch/internal/runtime"
)

// TestProxyIntegration routes a real pgx connection through the proxy to a
// real branch: database=postgres@proxy-pr-1, SCRAM password auth relayed untouched.
func TestProxyIntegration(t *testing.T) {
	if os.Getenv("PGBRANCH_IT") != "1" {
		t.Skip("set PGBRANCH_IT=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	host, port, network, hostConn := pgctl.StartSourcePG(t, ctx)
	{
		c, err := pgx.Connect(ctx, hostConn)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(ctx, `CREATE TABLE accounts(id int primary key, balance int);
			INSERT INTO accounts SELECT i, 100 FROM generate_series(1,1000) i`); err != nil {
			t.Fatal(err)
		}
		c.Close(ctx)
	}

	d, err := runtime.NewDockerDriver()
	if err != nil {
		t.Fatal(err)
	}
	reg, err := registry.Open(t.TempDir() + "/it.db")
	if err != nil {
		t.Fatal(err)
	}
	// Registered before the destroy cleanups below so it runs after them
	// (cleanups are LIFO).
	t.Cleanup(func() { reg.Close() })
	e := engine.New(reg, d, "postgres:17")

	src := &registry.Source{Name: "proxy-main", PGVersion: "17", ConnHost: host, ConnPort: port, ConnUser: "postgres", Network: network}
	if err := e.AddSource(ctx, src, "secret"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.RemoveVolume(context.Background(), src.Volume) })

	if _, err := e.CreateBranch(ctx, "proxy-pr-1", "proxy-main", 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.DestroyBranch(context.Background(), "proxy-pr-1"); err != nil {
			t.Errorf("destroy pr-1: %v", err)
		}
	})

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxyCtx, stopProxy := context.WithCancel(context.Background())
	t.Cleanup(stopProxy)
	go pgproxy.New(&pgproxy.RegistryResolver{Reg: reg}).Serve(proxyCtx, lis)
	proxyAddr := lis.Addr().String()

	// happy path: pgx sends SSLRequest (sslmode=prefer) -> 'N' -> plaintext
	// startup with database=postgres@proxy-pr-1 -> SCRAM auth relayed -> query.
	conn, err := pgx.Connect(ctx, fmt.Sprintf("postgres://postgres:secret@%s/postgres@proxy-pr-1", proxyAddr))
	if err != nil {
		t.Fatalf("connect through proxy: %v", err)
	}
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM accounts`).Scan(&n); err != nil {
		t.Fatalf("query through proxy: %v", err)
	}
	if n != 1000 {
		t.Fatalf("rows through proxy = %d, want 1000", n)
	}
	// write through the proxy stays on the branch
	if _, err := conn.Exec(ctx, `UPDATE accounts SET balance = 0 WHERE id = 1`); err != nil {
		t.Fatalf("write through proxy: %v", err)
	}
	conn.Close(ctx)

	// negative: no @ in the database name -> 3D000 with guidance
	_, err = pgx.Connect(ctx, fmt.Sprintf("postgres://postgres:secret@%s/postgres", proxyAddr))
	if err == nil {
		t.Fatal("connect without @branch suffix should fail")
	}
	if !strings.Contains(err.Error(), "pgbranch: connect with dbname@branch") {
		t.Errorf("missing-suffix error %q lacks guidance", err)
	}

	// negative: unknown branch -> 3D000 naming the branch
	_, err = pgx.Connect(ctx, fmt.Sprintf("postgres://postgres:secret@%s/postgres@nope", proxyAddr))
	if err == nil {
		t.Fatal("connect to unknown branch should fail")
	}
	if !strings.Contains(err.Error(), `"nope"`) {
		t.Errorf("unknown-branch error %q does not name the branch", err)
	}
}
