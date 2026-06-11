package engine

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/pgbranch/internal/pgctl"
	"github.com/abd-ulbasit/pgbranch/internal/registry"
	"github.com/abd-ulbasit/pgbranch/internal/runtime"
)

// TestDiffBranchEndToEnd exercises DiffBranch against real docker: branch a
// source, apply DDL + rows inside the branch, and expect the diff to surface
// the new table in both the schema diff and the row-estimate deltas, with the
// source untouched and no stray throwaway branches left behind. Names use a
// dft- prefix so they cannot collide with the engine's internal diff- ones.
func TestDiffBranchEndToEnd(t *testing.T) {
	if os.Getenv("PGBRANCH_IT") != "1" {
		t.Skip("set PGBRANCH_IT=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	host, port, network, hostConn := pgctl.StartSourcePG(t, ctx)
	mustExec(t, ctx, hostConn, `CREATE TABLE dft_users(id int primary key, email text);
		INSERT INTO dft_users SELECT i, 'u' || i FROM generate_series(1,500) i;
		ANALYZE dft_users`)

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

	src := &registry.Source{Name: "dft-main", PGVersion: "17", ConnHost: host, ConnPort: port, ConnUser: "postgres", Network: network}
	if err := e.AddSource(ctx, src, "secret"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.RemoveVolume(context.Background(), src.Volume) })

	b, err := e.CreateBranch(ctx, "dft-pr-1", "dft-main", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.DestroyBranch(context.Background(), "dft-pr-1"); err != nil {
			t.Errorf("destroy dft-pr-1: %v", err)
		}
	})

	// mutate the branch only: new table with rows, plus a column on the
	// seeded table; ANALYZE so reltuples reflects the inserts
	mustExec(t, ctx, branchConn(b), `CREATE TABLE diffdemo(x int);
		INSERT INTO diffdemo SELECT generate_series(1,100);
		ALTER TABLE dft_users ADD COLUMN extra text;
		ANALYZE`)

	start := time.Now()
	res, err := e.DiffBranch(ctx, "dft-pr-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("DiffBranch completed in %s", time.Since(start))

	// schema diff: the new table and the new column appear as insertions
	if !strings.Contains(res.SchemaDiff, "+CREATE TABLE public.diffdemo (") {
		t.Errorf("SchemaDiff missing the created table:\n%s", res.SchemaDiff)
	}
	if !strings.Contains(res.SchemaDiff, "+    extra text") {
		t.Errorf("SchemaDiff missing the added column:\n%s", res.SchemaDiff)
	}

	// row estimates: diffdemo only exists in the branch, with ~100 rows
	var demo *TableDelta
	for i := range res.Tables {
		if res.Tables[i].Table == "diffdemo" {
			demo = &res.Tables[i]
		}
	}
	if demo == nil {
		t.Fatalf("diffdemo not in table deltas: %+v", res.Tables)
	}
	if demo.BaseRows != 0 || demo.BranchRows <= 0 || demo.Delta <= 0 {
		t.Errorf("diffdemo delta = %+v, want base 0 and positive branch rows/delta", *demo)
	}

	// the source (and so the base) is untouched by the diff
	if n := mustQueryInt(t, ctx, hostConn, `SELECT count(*) FROM pg_class WHERE relname='diffdemo'`); n != 0 {
		t.Errorf("source grew a diffdemo table (n=%d)", n)
	}
	if n := mustQueryInt(t, ctx, hostConn, `SELECT count(*) FROM information_schema.columns WHERE table_name='dft_users'`); n != 2 {
		t.Errorf("source dft_users has %d columns, want 2", n)
	}

	// no stray throwaway branches: registry holds only dft-pr-1
	live, err := r.ListLiveBranches()
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].Name != "dft-pr-1" {
		names := make([]string, 0, len(live))
		for _, lb := range live {
			names = append(names, lb.Name)
		}
		t.Errorf("live branches after diff = %v, want [dft-pr-1]", names)
	}
	for _, c := range mustListManaged(t, ctx, d) {
		if strings.Contains(c.Labels["pgbranch.branch.name"], "diff-") {
			t.Errorf("throwaway container leaked: %+v", c)
		}
	}

	// a second diff on the same branch works (throwaway names don't collide)
	res2, err := e.DiffBranch(ctx, "dft-pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res2.SchemaDiff, "+CREATE TABLE public.diffdemo (") {
		t.Errorf("second diff lost the schema change:\n%s", res2.SchemaDiff)
	}
}

func mustListManaged(t *testing.T, ctx context.Context, d runtime.Driver) []runtime.ContainerInfo {
	t.Helper()
	cs, err := d.ListManaged(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return cs
}
