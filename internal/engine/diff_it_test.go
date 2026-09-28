package engine

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/pgoverlay/internal/pgctl"
	"github.com/abd-ulbasit/pgoverlay/internal/registry"
	"github.com/abd-ulbasit/pgoverlay/internal/runtime"
)

// TestDiffBranchEndToEnd exercises DiffBranch against real docker: branch a
// source, apply DDL + rows inside the branch, and expect the diff to surface
// the new table in both the schema diff and the row-estimate deltas, with the
// source untouched and no stray throwaway branches left behind. Names use a
// dft- prefix so they cannot collide with the engine's internal diff- ones.
func TestDiffBranchEndToEnd(t *testing.T) {
	if os.Getenv("PGOVERLAY_IT") != "1" {
		t.Skip("set PGOVERLAY_IT=1")
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
		if strings.Contains(c.Labels["pgoverlay.branch.name"], "diff-") {
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

// TestDiffBranchDataSampleEndToEnd exercises WithDataSample against real
// docker: branch a source, INSERT new rows into a PK table and into a no-PK
// table on the branch only, and expect the diff to return the new PK-table
// rows as samples while skipping the no-PK table, with the source untouched.
// Names use a diffd- prefix so they cannot collide with the engine's internal
// diff- throwaways.
func TestDiffBranchDataSampleEndToEnd(t *testing.T) {
	if os.Getenv("PGOVERLAY_IT") != "1" {
		t.Skip("set PGOVERLAY_IT=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	host, port, network, hostConn := pgctl.StartSourcePG(t, ctx)
	mustExec(t, ctx, hostConn, `CREATE TABLE diffd_users(id int primary key, email text);
		INSERT INTO diffd_users SELECT i, 'u' || i FROM generate_series(1,10) i;
		CREATE TABLE diffd_log(msg text);
		INSERT INTO diffd_log SELECT 'm' || i FROM generate_series(1,5) i;
		ANALYZE`)

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

	src := &registry.Source{Name: "diffd-main", PGVersion: "17", ConnHost: host, ConnPort: port, ConnUser: "postgres", Network: network}
	if err := e.AddSource(ctx, src, "secret"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.RemoveVolume(context.Background(), src.Volume) })

	b, err := e.CreateBranch(ctx, "diffd-pr-1", "diffd-main", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.DestroyBranch(context.Background(), "diffd-pr-1"); err != nil {
			t.Errorf("destroy diffd-pr-1: %v", err)
		}
	})

	// branch-only inserts: new PK rows (id 11,12,13) and new no-PK log rows
	mustExec(t, ctx, branchConn(b), `INSERT INTO diffd_users VALUES (11,'new11'),(12,'new12'),(13,'new13');
		INSERT INTO diffd_log VALUES ('newlog1'),('newlog2');
		ANALYZE`)

	res, err := e.DiffBranch(ctx, "diffd-pr-1", WithDataSample(20))
	if err != nil {
		t.Fatal(err)
	}

	var users, log *TableDelta
	for i := range res.Tables {
		switch res.Tables[i].Table {
		case "diffd_users":
			users = &res.Tables[i]
		case "diffd_log":
			log = &res.Tables[i]
		}
	}
	if users == nil {
		t.Fatalf("diffd_users not in deltas: %+v", res.Tables)
	}
	// the three branch-only PK rows come back as samples
	if len(users.SampleRows) != 3 {
		t.Fatalf("diffd_users SampleRows = %v, want 3 new rows", users.SampleRows)
	}
	gotIDs := map[float64]bool{}
	for _, row := range users.SampleRows {
		id, ok := row["id"].(float64)
		if !ok {
			t.Fatalf("sample row missing numeric id: %v", row)
		}
		gotIDs[id] = true
	}
	for _, want := range []float64{11, 12, 13} {
		if !gotIDs[want] {
			t.Errorf("sample rows missing id %v: %v", want, users.SampleRows)
		}
	}
	// the no-PK table grew but is skipped for sampling
	if log != nil && log.SampleRows != nil {
		t.Errorf("no-PK diffd_log got SampleRows: %v", log.SampleRows)
	}

	// source untouched: still 10 users, no id 11
	if n := mustQueryInt(t, ctx, hostConn, `SELECT count(*) FROM diffd_users`); n != 10 {
		t.Errorf("source diffd_users grew to %d rows, want 10", n)
	}
	if n := mustQueryInt(t, ctx, hostConn, `SELECT count(*) FROM diffd_users WHERE id=11`); n != 0 {
		t.Errorf("source grew an id=11 row (n=%d)", n)
	}

	// no stray throwaway branches
	live, err := r.ListLiveBranches()
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].Name != "diffd-pr-1" {
		t.Errorf("live branches after diff = %+v, want [diffd-pr-1]", live)
	}
}

// TestDiffBranchSchemasAndUnanalyzedEndToEnd covers the review findings on
// real Postgres: a branch-only PK table is sampled (it used to fail the whole
// diff with 'relation does not exist'), tables are keyed and queried by
// schema (a grown table outside search_path used to fail sampling, and
// same-named tables used to collide), a '|' in a table name parses, a
// never-analyzed table (reltuples -1) is counted instead of reported as 0,
// and sampling finds exactly the new keys in a table larger than the cap
// whose branch also deleted a row. Names use a diffs- prefix.
func TestDiffBranchSchemasAndUnanalyzedEndToEnd(t *testing.T) {
	if os.Getenv("PGOVERLAY_IT") != "1" {
		t.Skip("set PGOVERLAY_IT=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	host, port, network, hostConn := pgctl.StartSourcePG(t, ctx)
	mustExec(t, ctx, hostConn, `CREATE SCHEMA app;
		CREATE TABLE app.orders(id bigint primary key, note text);
		INSERT INTO app.orders SELECT i, 'o' || i FROM generate_series(1,100) i;
		CREATE TABLE public.orders(id int primary key);
		INSERT INTO public.orders SELECT generate_series(1,5);
		CREATE TABLE "odd|name"(x int);
		INSERT INTO "odd|name" VALUES (1);
		ANALYZE app.orders; ANALYZE public.orders; ANALYZE "odd|name";
		CREATE TABLE unanalyzed(x int) WITH (autovacuum_enabled = false);
		INSERT INTO unanalyzed SELECT generate_series(1,300)`)

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

	src := &registry.Source{Name: "diffs-main", PGVersion: "17", ConnHost: host, ConnPort: port, ConnUser: "postgres", Network: network}
	if err := e.AddSource(ctx, src, "secret"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.RemoveVolume(context.Background(), src.Volume) })

	b, err := e.CreateBranch(ctx, "diffs-pr-1", "diffs-main", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.DestroyBranch(context.Background(), "diffs-pr-1"); err != nil {
			t.Errorf("destroy diffs-pr-1: %v", err)
		}
	})

	mustExec(t, ctx, branchConn(b), `INSERT INTO app.orders SELECT i, 'new' || i FROM generate_series(101,103) i;
		DELETE FROM app.orders WHERE id = 7;
		ANALYZE app.orders;
		CREATE TABLE audit_log(id bigint primary key, entry text) WITH (autovacuum_enabled = false);
		INSERT INTO audit_log SELECT i, 'e' || i FROM generate_series(1,40) i;
		INSERT INTO unanalyzed SELECT generate_series(301,310)`)

	res, err := e.DiffBranch(ctx, "diffs-pr-1", WithDataSample(5))
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	get := func(schema, table string) TableDelta {
		t.Helper()
		for _, td := range res.Tables {
			if td.Schema == schema && td.Table == table {
				return td
			}
		}
		t.Fatalf("%s.%s not in deltas: %+v", schema, table, res.Tables)
		return TableDelta{}
	}
	ids := func(td TableDelta) string {
		var out []float64
		for _, row := range td.SampleRows {
			out = append(out, row["id"].(float64))
		}
		return fmt.Sprint(out)
	}

	if o := get("app", "orders"); o.BaseRows != 100 || o.BranchRows != 102 || ids(o) != "[101 102 103]" {
		t.Errorf("app.orders = %+v (samples %s), want 100 -> 102 with new ids [101 102 103]", o, ids(o))
	}
	if o := get("public", "orders"); o.BaseRows != 5 || o.BranchRows != 5 {
		t.Errorf("public.orders = %+v, want 5/5 (separate from app.orders)", o)
	}
	if o := get("public", "odd|name"); o.BaseRows != 1 || o.BranchRows != 1 {
		t.Errorf(`"odd|name" = %+v, want 1/1`, o)
	}
	if u := get("public", "unanalyzed"); u.RowsUnknown || u.BaseRows != 300 || u.BranchRows != 310 || u.Delta != 10 {
		t.Errorf("unanalyzed = %+v, want exact 300 -> 310 (+10)", u)
	}
	if a := get("public", "audit_log"); a.BaseRows != 0 || a.BranchRows != 40 || ids(a) != "[36 37 38 39 40]" {
		t.Errorf("audit_log = %+v (samples %s), want 0 -> 40 with the 5 newest ids", a, ids(a))
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
