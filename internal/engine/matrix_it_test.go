package engine

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/pgoverlay/internal/cow"
	"github.com/abd-ulbasit/pgoverlay/internal/pgctl/pgctltest"
	"github.com/abd-ulbasit/pgoverlay/internal/registry"
	"github.com/abd-ulbasit/pgoverlay/internal/runtime"
)

// TestVersionMatrix runs a compact seed -> branch -> verify -> destroy cycle
// against every Postgres image tag in PGOVERLAY_MATRIX_VERSIONS (default
// "14 18", the edges of the supported 14-18 range): a major ("17") runs
// postgres:<major>, a tag with a variant ("17-alpine") runs that image, which
// exercises the musl build of the lazyrw shim and the Alpine postgres user
// (uid 70). Every branch must run with the shim (cow-mode lazyrw, the build
// for the image's libc) and read a table without copying it. Gated
// separately from the regular docker IT because it pulls one postgres image
// per version.
//
//	PGOVERLAY_MATRIX_IT=1 PGOVERLAY_MATRIX_VERSIONS="14 15 16 17 18 17-alpine" \
//	  go test ./internal/engine/ -run Matrix -count=1 -v -timeout 25m
func TestVersionMatrix(t *testing.T) {
	if os.Getenv("PGOVERLAY_MATRIX_IT") != "1" {
		t.Skip("set PGOVERLAY_MATRIX_IT=1")
	}
	versions := strings.Fields(os.Getenv("PGOVERLAY_MATRIX_VERSIONS"))
	if len(versions) == 0 {
		versions = []string{"14", "18"}
	}
	for _, v := range versions {
		t.Run("pg"+v, func(t *testing.T) { runMatrixCycle(t, v) })
	}
}

// runMatrixCycle is one full lifecycle for a single image tag; all resource
// names are prefixed mx<tag>- so versions never collide.
func runMatrixCycle(t *testing.T, ver string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	major, variant, _ := strings.Cut(ver, "-")
	host, port, network, hostConn := pgctltest.StartSourcePGVersion(t, ctx, ver)
	mustExec(t, ctx, hostConn, `CREATE TABLE accounts(id int primary key, balance int);
		INSERT INTO accounts SELECT i, 100 FROM generate_series(1,1000) i;
		CREATE TABLE big AS SELECT g AS id, repeat(md5(g::text), 4) AS pad FROM generate_series(1, 60000) g`)

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
	src := &registry.Source{Name: srcName, PGVersion: major, ConnHost: host, ConnPort: port, ConnUser: "postgres", Network: network}
	if variant != "" {
		src.Image = "postgres:" + ver
	}
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
	got := mustQueryInt(t, ctx, branchConn(b), `SELECT current_setting('server_version_num')::int / 10000`)
	if want, _ := strconv.Atoi(major); got != want {
		t.Fatalf("branch server major = %d, want %s", got, major)
	}
	// with the lazyrw shim built for the image's libc
	mode, err := d.ExecOutput(ctx, b.ContainerID, []string{"cat", cow.CowModePath})
	if err != nil {
		t.Fatal(err)
	}
	libc := "glibc"
	if variant == "alpine" {
		libc = "musl"
	}
	if !strings.HasPrefix(mode, cow.CowModeLazyRW+"\n") || !strings.Contains(mode, "liblazyrw-"+libc+"-") {
		t.Fatalf("pg%s cow-mode = %q, want lazyrw with the %s build", ver, mode, libc)
	}
	// branch sees the seeded data, and reading it copies nothing
	mustExec(t, ctx, branchConn(b), `CHECKPOINT`)
	before, err := e.BranchUsage(ctx, brName)
	if err != nil {
		t.Fatal(err)
	}
	if n := mustQueryInt(t, ctx, branchConn(b), `SELECT count(*) FROM accounts`); n != 1000 {
		t.Fatalf("pg%s branch rows = %d, want 1000", ver, n)
	}
	if n := mustQueryInt(t, ctx, branchConn(b), `SELECT count(*) FROM big WHERE length(pad) = 128`); n != 60000 {
		t.Fatalf("pg%s big rows = %d, want 60000", ver, n)
	}
	mustExec(t, ctx, branchConn(b), `CHECKPOINT`)
	after, err := e.BranchUsage(ctx, brName)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("pg%s: %s; a read of accounts and big grew the branch by %d bytes", ver, strings.ReplaceAll(strings.TrimSpace(mode), "\n", " "), after-before)
	if after-before >= 2<<20 {
		t.Errorf("pg%s: a read grew the branch by %d bytes, want < 2 MiB", ver, after-before)
	}
	// writes stay in the branch; the source is untouched
	mustExec(t, ctx, branchConn(b), `UPDATE accounts SET balance = 0`)
	if n := mustQueryInt(t, ctx, hostConn, `SELECT sum(balance) FROM accounts`); n != 100*1000 {
		t.Fatalf("pg%s source mutated! sum=%d", ver, n)
	}
	if n := mustQueryInt(t, ctx, branchConn(b), `SELECT sum(balance) FROM accounts`); n != 0 {
		t.Fatalf("pg%s branch write lost: sum=%d", ver, n)
	}
}
