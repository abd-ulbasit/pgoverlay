package engine

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/pgbranch/internal/pgctl"
	"github.com/abd-ulbasit/pgbranch/internal/registry"
	"github.com/abd-ulbasit/pgbranch/internal/runtime"
)

// TestVersionMatrix runs a compact seed -> branch -> verify -> destroy cycle
// against every Postgres major in PGBRANCH_MATRIX_VERSIONS (default "14 18",
// the edges of the supported 14-18 range). Gated separately from the regular
// docker IT because it pulls one postgres image per version.
//
//	PGBRANCH_MATRIX_IT=1 PGBRANCH_MATRIX_VERSIONS="14 15 16 17 18" \
//	  go test ./internal/engine/ -run Matrix -count=1 -v -timeout 25m
func TestVersionMatrix(t *testing.T) {
	if os.Getenv("PGBRANCH_MATRIX_IT") != "1" {
		t.Skip("set PGBRANCH_MATRIX_IT=1")
	}
	versions := strings.Fields(os.Getenv("PGBRANCH_MATRIX_VERSIONS"))
	if len(versions) == 0 {
		versions = []string{"14", "18"}
	}
	for _, v := range versions {
		t.Run("pg"+v, func(t *testing.T) { runMatrixCycle(t, v) })
	}
}

// runMatrixCycle is one full lifecycle for a single major; all resource
// names are prefixed mx<ver>- so versions never collide.
func runMatrixCycle(t *testing.T, ver string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	host, port, network, hostConn := pgctl.StartSourcePGVersion(t, ctx, ver)
	mustExec(t, ctx, hostConn, `CREATE TABLE accounts(id int primary key, balance int);
		INSERT INTO accounts SELECT i, 100 FROM generate_series(1,1000) i`)

	d, err := runtime.NewDockerDriver()
	if err != nil {
		t.Fatal(err)
	}
	r, err := registry.Open(t.TempDir() + "/it.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() }) // LIFO: closes after the destroy cleanups below
	e := New(r, d, "postgres:17")

	srcName, brName := "mx"+ver+"-main", "mx"+ver+"-pr-1"
	src := &registry.Source{Name: srcName, PGVersion: ver, ConnHost: host, ConnPort: port, ConnUser: "postgres", Network: network}
	if err := e.AddSource(ctx, src, "secret"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.RemoveVolume(context.Background(), src.Volume) })

	start := time.Now()
	b, err := e.CreateBranch(ctx, brName, srcName, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("pg%s: branch created in %s", ver, time.Since(start))
	t.Cleanup(func() {
		if err := e.DestroyBranch(context.Background(), brName); err != nil {
			t.Errorf("destroy %s: %v", brName, err)
		}
	})

	// the branch runs the requested major, not the default image
	major := mustQueryInt(t, ctx, branchConn(b), `SELECT current_setting('server_version_num')::int / 10000`)
	if want, _ := strconv.Atoi(ver); major != want {
		t.Fatalf("branch server major = %d, want %s", major, ver)
	}
	// branch sees the seeded data
	if n := mustQueryInt(t, ctx, branchConn(b), `SELECT count(*) FROM accounts`); n != 1000 {
		t.Fatalf("pg%s branch rows = %d, want 1000", ver, n)
	}
	// writes stay in the branch; the source is untouched
	mustExec(t, ctx, branchConn(b), `UPDATE accounts SET balance = 0`)
	if n := mustQueryInt(t, ctx, hostConn, `SELECT sum(balance) FROM accounts`); n != 100*1000 {
		t.Fatalf("pg%s source mutated! sum=%d", ver, n)
	}
}
