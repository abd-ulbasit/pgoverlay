package pgbranchtest_test

// Integration test for the public SDK: starts the REST API in-process on a
// real engine + docker driver (same bootstrap as internal/api's IT), points
// PGBRANCH_SERVER at it, and exercises Acquire end to end — connect, write,
// and automatic destruction when the subtest ends.
//
// Run: PGBRANCH_IT=1 go test ./pgbranchtest/ -v -timeout 15m

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/abd-ulbasit/pgbranch/internal/api"
	"github.com/abd-ulbasit/pgbranch/internal/engine"
	"github.com/abd-ulbasit/pgbranch/internal/pgctl"
	"github.com/abd-ulbasit/pgbranch/internal/registry"
	"github.com/abd-ulbasit/pgbranch/internal/runtime"
	"github.com/abd-ulbasit/pgbranch/pgbranchtest"
)

const sdkITToken = "sdk-it-token"

func TestSDKAcquire(t *testing.T) {
	if os.Getenv("PGBRANCH_IT") != "1" {
		t.Skip("set PGBRANCH_IT=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	host, port, network, hostConn := pgctl.StartSourcePG(t, ctx)
	seedConn, err := pgx.Connect(ctx, hostConn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seedConn.Exec(ctx, `CREATE TABLE sdk_probe(id int primary key, note text);
		INSERT INTO sdk_probe VALUES (1, 'seeded')`); err != nil {
		t.Fatal(err)
	}
	seedConn.Close(ctx)

	drv, err := runtime.NewDockerDriver()
	if err != nil {
		t.Fatal(err)
	}
	reg, err := registry.Open(t.TempDir() + "/sdk-it.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	eng := engine.New(reg, drv, "postgres:17")

	ts := httptest.NewServer(api.New(eng, reg, sdkITToken).Handler())
	t.Cleanup(ts.Close)

	// sdk- prefix keeps the source name disjoint from other IT packages'
	// resources when the whole suite runs in one docker daemon.
	if err := eng.AddSource(ctx, &registry.Source{
		Name: "sdk-main", PGVersion: "17", ConnHost: host, ConnPort: port,
		ConnUser: "postgres", ConnDB: "postgres", Network: network,
		SeedVia: registry.SeedViaBasebackup,
	}, "secret"); err != nil {
		t.Fatalf("seed source: %v", err)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer ccancel()
		if err := eng.RemoveSource(cctx, "sdk-main"); err != nil {
			t.Errorf("remove source: %v", err)
		}
	})

	t.Setenv("PGBRANCH_SERVER", ts.URL)
	t.Setenv("PGBRANCH_TOKEN", sdkITToken)
	t.Setenv("PGBRANCH_TEST_SOURCE", "sdk-main")
	t.Setenv("PGBRANCH_PASSWORD", "secret")

	var branchName string
	t.Run("acquire-write", func(t *testing.T) {
		b := pgbranchtest.Acquire(t)
		branchName = b.Name
		t.Logf("acquired branch %q at %s:%d", b.Name, b.Host, b.Port)

		db, err := sql.Open("pgx", b.DSN)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var note string
		if err := db.QueryRowContext(ctx, `SELECT note FROM sdk_probe WHERE id = 1`).Scan(&note); err != nil {
			t.Fatalf("read seeded row via %s: %v", b.DSN, err)
		}
		if note != "seeded" {
			t.Fatalf("seeded note = %q", note)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO sdk_probe VALUES (2, 'written-in-branch')`); err != nil {
			t.Fatalf("write in branch: %v", err)
		}
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sdk_probe`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 2 {
			t.Fatalf("rows in branch = %d, want 2", n)
		}
	})
	if branchName == "" {
		t.Fatal("subtest did not record a branch name")
	}

	// the subtest's cleanup ran: the branch must be gone server-side
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v1/branches/"+branchName, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+sdkITToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s after subtest = %d, want 404 (branch not destroyed)", branchName, resp.StatusCode)
	}

	// the branch write never reached the source
	srcConn, err := pgx.Connect(ctx, hostConn)
	if err != nil {
		t.Fatal(err)
	}
	defer srcConn.Close(ctx)
	var n int
	if err := srcConn.QueryRow(ctx, `SELECT count(*) FROM sdk_probe`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("source rows = %d, want 1 (branch write leaked)", n)
	}
}
