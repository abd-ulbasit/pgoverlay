package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/jackc/pgx/v5"

	"github.com/abd-ulbasit/pgoverlay/internal/cow"
	"github.com/abd-ulbasit/pgoverlay/internal/pgctl/pgctltest"
	"github.com/abd-ulbasit/pgoverlay/internal/registry"
	"github.com/abd-ulbasit/pgoverlay/internal/runtime"
)

// The copy-on-write torture suite (#49): real branches of a settled seed of
// two ~220 MB tables, one frozen on the source and one never vacuumed (with
// dead tuples), on a real kernel with the lazyrw shim. It checks that reads
// copy nothing, that a write copies at most the segment it touches, that
// TRUNCATE, DROP and rewrites never copy the old file, that CREATE DATABASE,
// pgbench with a kill -9, reconcile's restart, branch-from-branch, reset and
// diff all work on top of it, and that --lazyrw=off still copies eagerly.
//
// Every resource is named cowt-* (databases cowt_*), so the suite can run
// next to the other integration tests. PGOVERLAY_IT_COW_ROWS overrides the
// rows per big table (default 1300000, about 220 MB each). The branches are
// reached over localhost, so run it on the Docker host.
//
//	PGOVERLAY_IT=1 go test ./internal/engine/ -run TestCowTorture -count=1 -v -timeout 60m
func TestCowTorture(t *testing.T) {
	if os.Getenv("PGOVERLAY_IT") != "1" {
		t.Skip("set PGOVERLAY_IT=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Minute)
	defer cancel()

	rows := cowtRows(t)
	host, port, network, hostConn := pgctltest.StartSourcePG(t, ctx)
	cowtPopulate(t, ctx, hostConn, rows)

	d, err := runtime.NewDockerDriver()
	if err != nil {
		t.Fatal(err)
	}
	r, err := registry.Open(t.TempDir() + "/it.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	h := &cowHarness{t: t, ctx: ctx, d: d, r: r, rows: rows,
		e:    New(r, d, "postgres:17"),
		eOff: New(r, d, "postgres:17", WithLazyRW(false)), // --lazyrw=off, same registry
	}
	src := &registry.Source{Name: "cowt-src", PGVersion: "17", ConnHost: host, ConnPort: port, ConnUser: "postgres", Network: network}
	start := time.Now()
	if err := h.e.AddSource(ctx, src, "secret"); err != nil {
		t.Fatal(err)
	}
	t.Logf("seed (pg_basebackup + settle) of the %d-row tables took %s", rows, time.Since(start).Round(time.Millisecond))
	t.Cleanup(func() {
		if err := h.e.RemoveSource(context.Background(), src.Name); err != nil {
			t.Errorf("remove source: %v", err)
		}
	})
	h.src = src.Name
	h.frozenBytes = int64(mustQueryInt(t, ctx, hostConn, `SELECT pg_relation_size('cow_frozen')::int`))
	h.rawBytes = int64(mustQueryInt(t, ctx, hostConn, `SELECT pg_relation_size('cow_raw')::int`))
	t.Logf("cow_frozen %d bytes, cow_raw %d bytes", h.frozenBytes, h.rawBytes)

	t.Run("reads-copy-nothing", h.reads)
	t.Run("one-row-update-copies-the-segment", h.oneRowUpdate)
	t.Run("truncate-and-drop", h.truncateDrop)
	t.Run("create-index-and-vacuum-full", h.rewrites)
	t.Run("create-database", h.createDatabase)
	t.Run("pgbench-kill9-reconcile-restart", h.pgbenchCrash)
	t.Run("branch-from-branch", h.branchFromBranch)
	t.Run("diff-and-reset", h.diffReset)
	t.Run("lazyrw-off-copies-eagerly", h.lazyrwOff)
}

// cowtRows is the row count of each big table: about 170 bytes a row.
func cowtRows(t *testing.T) int {
	t.Helper()
	if v := os.Getenv("PGOVERLAY_IT_COW_ROWS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1000 {
			t.Fatalf("PGOVERLAY_IT_COW_ROWS=%q: want a row count >= 1000", v)
		}
		return n
	}
	return 1300000
}

// cowtPopulate fills the source: cow_frozen, frozen and checkpointed, with a
// primary key; cow_raw the same size but never vacuumed, with a tenth of its
// rows updated (dead tuples, unset hint bits); cow_small for small writes;
// and a small template database for CREATE DATABASE.
func cowtPopulate(t *testing.T, ctx context.Context, conn string, rows int) {
	t.Helper()
	mustExec(t, ctx, conn, fmt.Sprintf(`
		CREATE TABLE cow_frozen (id int PRIMARY KEY, v int, pad text);
		INSERT INTO cow_frozen SELECT g, g %% 1000, repeat(md5(g::text), 4) FROM generate_series(1, %d) g;
		CREATE TABLE cow_raw (id int, v int, pad text) WITH (autovacuum_enabled = off);
		INSERT INTO cow_raw SELECT g, g %% 1000, repeat(md5(g::text), 4) FROM generate_series(1, %d) g;
		UPDATE cow_raw SET v = v + 1 WHERE id %% 10 = 0;
		CREATE TABLE cow_small (id int PRIMARY KEY, v int);
		INSERT INTO cow_small SELECT g, g FROM generate_series(1, 1000) g`, rows, rows*10/11))
	mustExec(t, ctx, conn, `VACUUM (FREEZE, ANALYZE) cow_frozen`)
	mustExec(t, ctx, conn, `CREATE DATABASE cowt_tpl`)
	tpl := strings.TrimSuffix(conn, "/postgres") + "/cowt_tpl"
	mustExec(t, ctx, tpl, `CREATE TABLE tpl_t AS SELECT g AS id, md5(g::text) AS h FROM generate_series(1, 20000) g`)
	mustExec(t, ctx, conn, `CHECKPOINT`)
}

type cowHarness struct {
	t                     *testing.T
	ctx                   context.Context
	d                     *runtime.DockerDriver
	r                     *registry.Registry
	e, eOff               *Engine
	src                   string
	rows                  int
	frozenBytes, rawBytes int64
}

// create makes a branch off the source with eng and destroys it at the end.
func (h *cowHarness) create(t *testing.T, eng *Engine, name string) *registry.Branch {
	t.Helper()
	start := time.Now()
	b, err := eng.CreateBranch(h.ctx, name, h.src, 0)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Logf("branch %s created in %s", name, time.Since(start).Round(time.Millisecond))
	h.destroyAtEnd(t, eng, name)
	return b
}

func (h *cowHarness) destroyAtEnd(t *testing.T, eng *Engine, name string) {
	t.Cleanup(func() {
		if err := eng.DestroyBranch(context.Background(), name); err != nil && !errors.Is(err, registry.ErrNotFound) {
			t.Errorf("destroy %s: %v", name, err)
		}
	})
}

// reload re-reads a branch row (its container and port change on restarts).
func (h *cowHarness) reload(t *testing.T, b *registry.Branch) *registry.Branch {
	t.Helper()
	nb, err := h.r.GetBranchByName(b.Name)
	if err != nil {
		t.Fatal(err)
	}
	return nb
}

// mode reads the branch's cow-mode file (first line).
func (h *cowHarness) mode(t *testing.T, b *registry.Branch) string {
	t.Helper()
	out, err := h.d.ExecOutput(h.ctx, h.reload(t, b).ContainerID, []string{"cat", cow.CowModePath})
	if err != nil {
		t.Fatalf("read cow-mode of %s: %v", b.Name, err)
	}
	m, _, _ := strings.Cut(out, "\n")
	return m
}

func (h *cowHarness) wantMode(t *testing.T, b *registry.Branch, want string) {
	t.Helper()
	if m := h.mode(t, b); m != want {
		t.Fatalf("%s cow-mode = %q, want %q", b.Name, m, want)
	}
}

func (h *cowHarness) usage(t *testing.T, eng *Engine, b *registry.Branch) int64 {
	t.Helper()
	n, err := eng.BranchUsage(h.ctx, b.Name)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// growth is how much fn grows the branch's writable layer, measured after a
// CHECKPOINT on both sides (so dirtied pages are on disk, and the WAL
// segment the first checkpoint record copies up is counted before).
func (h *cowHarness) growth(t *testing.T, eng *Engine, b *registry.Branch, fn func(conn string)) int64 {
	t.Helper()
	b = h.reload(t, b)
	conn := branchConn(b)
	mustExec(t, h.ctx, conn, `CHECKPOINT`)
	before := h.usage(t, eng, b)
	fn(conn)
	mustExec(t, h.ctx, conn, `CHECKPOINT`)
	after := h.usage(t, eng, b)
	if after-before > 1<<20 {
		t.Logf("%s grew %d bytes; largest files in its upper layer:\n%s", b.Name, after-before, h.largestUpper(t, b))
	}
	return after - before
}

// dataGrowth is growth for writes: how much fn grows the relation data in
// the branch's upper layer (base/ and global/, apparent bytes), which leaves
// out the WAL a write legitimately produces (a VACUUM FULL logs every new
// page; crossing into a new segment adds 16 MiB). total is BranchUsage's
// growth, WAL included, for the log.
func (h *cowHarness) dataGrowth(t *testing.T, eng *Engine, b *registry.Branch, fn func(conn string)) (data, total int64) {
	t.Helper()
	var before int64
	total = h.growth(t, eng, b, func(conn string) {
		before = h.dataBytes(t, b)
		fn(conn)
		mustExec(t, h.ctx, conn, `CHECKPOINT`)
	})
	return h.dataBytes(t, b) - before, total
}

// dataBytes is the apparent size of the relation files in the branch's upper
// layer.
func (h *cowHarness) dataBytes(t *testing.T, b *registry.Branch) int64 {
	t.Helper()
	out, err := h.d.RunHelper(h.ctx, runtime.HelperSpec{
		Image:  runtime.UtilityImage,
		Cmd:    []string{"sh", "-c", "cd " + cow.RWPath + "/upper && du -sbc base global 2>/dev/null | tail -n 1"},
		Mounts: []runtime.Mount{{Volume: h.reload(t, b).RWVolume, Target: cow.RWPath, ReadOnly: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var n int64
	if _, err := fmt.Sscan(out, &n); err != nil {
		t.Fatalf("du output %q: %v", out, err)
	}
	return n
}

// largestUpper lists the largest files in a branch's upper layer (for
// failure messages; a helper's output keeps only its last lines).
func (h *cowHarness) largestUpper(t *testing.T, b *registry.Branch) string {
	t.Helper()
	out, err := h.d.RunHelper(h.ctx, runtime.HelperSpec{
		Image:  runtime.UtilityImage,
		Cmd:    []string{"sh", "-c", "cd " + cow.RWPath + "/upper && find . -type f -exec du -b {} + | sort -rn | head -n 12"},
		Mounts: []runtime.Mount{{Volume: h.reload(t, b).RWVolume, Target: cow.RWPath, ReadOnly: true}},
	})
	if err != nil {
		return err.Error()
	}
	return out
}

// inUpper reports whether a data-dir-relative path has an entry in the
// branch's upper layer (a copied-up or new file, or a whiteout).
func (h *cowHarness) inUpper(t *testing.T, b *registry.Branch, rel string) bool {
	t.Helper()
	out, err := h.d.RunHelper(h.ctx, runtime.HelperSpec{
		Image:  runtime.UtilityImage,
		Cmd:    []string{"sh", "-c", `if [ -e "$1" ]; then echo yes; else echo no; fi`, "in-upper", cow.RWPath + "/upper/" + rel},
		Mounts: []runtime.Mount{{Volume: h.reload(t, b).RWVolume, Target: cow.RWPath, ReadOnly: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(out) == "yes"
}

func relPath(t *testing.T, ctx context.Context, conn, rel string) string {
	t.Helper()
	c, err := pgx.Connect(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	var p string
	if err := c.QueryRow(ctx, "SELECT pg_relation_filepath($1::regclass)", rel).Scan(&p); err != nil {
		t.Fatal(err)
	}
	return p
}

// readPass reads a big table the ways an application or a person would:
// count, plan, size, a full scan and (cow_frozen) an index lookup.
func (h *cowHarness) readPass(t *testing.T, conn, table string) {
	t.Helper()
	want := h.rows
	if table == "cow_raw" {
		want = h.rows * 10 / 11
	}
	if n := mustQueryInt(t, h.ctx, conn, `SELECT count(*) FROM `+table); n != want {
		t.Fatalf("count(*) of %s = %d, want %d", table, n, want)
	}
	mustExec(t, h.ctx, conn, `EXPLAIN SELECT * FROM `+table+` WHERE v = 5`)
	mustQueryInt(t, h.ctx, conn, `SELECT pg_relation_size('`+table+`')::int`)
	mustQueryInt(t, h.ctx, conn, `SELECT sum(length(pad))::int FROM `+table)
	if table == "cow_frozen" {
		if n := mustQueryInt(t, h.ctx, conn, `SELECT v FROM cow_frozen WHERE id = 777`); n != 777 {
			t.Fatalf("index lookup = %d", n)
		}
	}
}

// Reads copy nothing: on the frozen table and on the one the source never
// vacuumed (the settle froze it in the seed), a first pass grows the branch
// by less than 2 MiB and a second by nothing; neither table's file (nor the
// index) reaches the upper layer.
func (h *cowHarness) reads(t *testing.T) {
	b := h.create(t, h.e, "cowt-read")
	h.wantMode(t, b, cow.CowModeLazyRW)
	conn := branchConn(b)
	for _, table := range []string{"cow_frozen", "cow_raw"} {
		start := time.Now()
		g1 := h.growth(t, h.e, b, func(conn string) { h.readPass(t, conn, table) })
		took := time.Since(start)
		g2 := h.growth(t, h.e, b, func(conn string) { h.readPass(t, conn, table) })
		t.Logf("%s: first read pass +%d bytes (%s incl. checkpoints), second pass +%d bytes", table, g1, took.Round(time.Millisecond), g2)
		if g1 >= 2<<20 {
			t.Errorf("%s: the first read pass grew the branch by %d bytes, want < 2 MiB", table, g1)
		}
		if g2 != 0 {
			t.Errorf("%s: the second read pass grew the branch by %d bytes, want 0", table, g2)
		}
		for _, rel := range []string{table, "cow_frozen_pkey"} {
			if p := relPath(t, h.ctx, conn, rel); h.inUpper(t, b, p) {
				t.Errorf("reading %s copied %s (%s) into the branch", table, rel, p)
			}
		}
	}
	// the statistics catalog the planner read is still the seed's
	if p := relPath(t, h.ctx, conn, "pg_statistic"); h.inUpper(t, b, p) {
		t.Errorf("planning copied pg_statistic (%s) into the branch; the settle should have frozen it", p)
	}
}

// A one-row UPDATE copies the segment it touches (the whole file here: both
// tables are under 1 GiB), once, and no other table; a second UPDATE of the
// same segment copies nothing more.
func (h *cowHarness) oneRowUpdate(t *testing.T) {
	b := h.create(t, h.e, "cowt-write")
	conn := branchConn(b)
	start := time.Now()
	g, total := h.dataGrowth(t, h.e, b, func(conn string) {
		mustExec(t, h.ctx, conn, `UPDATE cow_raw SET v = v + 1 WHERE id = 42`)
	})
	t.Logf("one-row UPDATE of cow_raw (%d bytes): data +%d bytes, usage +%d bytes (%s incl. checkpoints)", h.rawBytes, g, total, time.Since(start).Round(time.Millisecond))
	if g > h.rawBytes+2<<20 {
		t.Errorf("a one-row UPDATE grew the branch's data by %d bytes, more than the touched segment (%d) plus 2 MiB", g, h.rawBytes)
	}
	if !h.inUpper(t, b, relPath(t, h.ctx, conn, "cow_raw")) {
		t.Error("the updated table's segment is not in the branch")
	}
	if h.inUpper(t, b, relPath(t, h.ctx, conn, "cow_frozen")) {
		t.Error("an UPDATE of cow_raw copied cow_frozen")
	}
	g2, total := h.dataGrowth(t, h.e, b, func(conn string) {
		mustExec(t, h.ctx, conn, `UPDATE cow_raw SET v = v + 1 WHERE id = 43`)
	})
	t.Logf("second one-row UPDATE of the same segment: data +%d bytes, usage +%d bytes", g2, total)
	if g2 >= 2<<20 {
		t.Errorf("a second UPDATE of an already copied segment grew the branch's data by %d bytes", g2)
	}
	gs, total := h.dataGrowth(t, h.e, b, func(conn string) {
		mustExec(t, h.ctx, conn, `UPDATE cow_small SET v = -1 WHERE id = 1`)
	})
	t.Logf("one-row UPDATE of cow_small: data +%d bytes, usage +%d bytes", gs, total)
	if gs >= 2<<20 {
		t.Errorf("a one-row UPDATE of a small table grew the branch's data by %d bytes", gs)
	}
	if n := mustQueryInt(t, h.ctx, conn, `SELECT v FROM cow_raw WHERE id = 42`); n != 42%1000+1 {
		t.Errorf("updated row reads v = %d", n)
	}
}

// TRUNCATE and DROP of tables in the seed rewrite nothing: Postgres
// truncates the old file to 0 bytes before unlinking it, which on an overlay
// would copy the whole file up first; the shim turns it into an O_TRUNC open.
func (h *cowHarness) truncateDrop(t *testing.T) {
	b := h.create(t, h.e, "cowt-trunc")
	var truncTook, dropTook time.Duration
	g, total := h.dataGrowth(t, h.e, b, func(conn string) {
		start := time.Now()
		mustExec(t, h.ctx, conn, `TRUNCATE cow_raw`)
		truncTook = time.Since(start)
		start = time.Now()
		mustExec(t, h.ctx, conn, `DROP TABLE cow_frozen`)
		dropTook = time.Since(start)
	})
	t.Logf("TRUNCATE cow_raw took %s, DROP TABLE cow_frozen took %s; together data +%d bytes (the catalog rows they change), usage +%d bytes",
		truncTook.Round(time.Millisecond), dropTook.Round(time.Millisecond), g, total)
	if truncTook > 2*time.Second || dropTook > 2*time.Second {
		t.Errorf("TRUNCATE took %s and DROP %s, want < 2 s each", truncTook, dropTook)
	}
	if g >= 2<<20 {
		t.Errorf("TRUNCATE and DROP grew the branch's data by %d bytes, want < 2 MiB", g)
	}
	conn := branchConn(h.reload(t, b))
	if n := mustQueryInt(t, h.ctx, conn, `SELECT count(*) FROM cow_raw`); n != 0 {
		t.Errorf("cow_raw has %d rows after TRUNCATE", n)
	}
	if n := mustQueryInt(t, h.ctx, conn, `SELECT count(*) FROM pg_class WHERE relname = 'cow_frozen'`); n != 0 {
		t.Error("cow_frozen still exists after DROP")
	}
}

// CREATE INDEX reads the table and writes only the index; VACUUM FULL writes
// a new copy of the table and never copies the old file up.
func (h *cowHarness) rewrites(t *testing.T) {
	b := h.create(t, h.e, "cowt-ddl")
	conn := branchConn(b)
	g, total := h.dataGrowth(t, h.e, b, func(conn string) {
		mustExec(t, h.ctx, conn, `CREATE INDEX cow_raw_v ON cow_raw (v)`)
	})
	idx := int64(mustQueryInt(t, h.ctx, conn, `SELECT pg_relation_size('cow_raw_v')::int`))
	t.Logf("CREATE INDEX: data +%d bytes (index %d bytes), usage +%d bytes", g, idx, total)
	if g > idx+2<<20 {
		t.Errorf("CREATE INDEX grew the branch's data by %d bytes, more than its index (%d) plus 2 MiB", g, idx)
	}
	if h.inUpper(t, b, relPath(t, h.ctx, conn, "cow_raw")) {
		t.Error("CREATE INDEX copied the indexed table into the branch")
	}
	old := relPath(t, h.ctx, conn, "cow_frozen")
	g, total = h.dataGrowth(t, h.e, b, func(conn string) {
		mustExec(t, h.ctx, conn, `VACUUM FULL cow_frozen`)
	})
	rewritten := int64(mustQueryInt(t, h.ctx, conn, `SELECT pg_total_relation_size('cow_frozen')::int`))
	t.Logf("VACUUM FULL cow_frozen: data +%d bytes (new table and index %d bytes), usage +%d bytes (WAL included)", g, rewritten, total)
	if g > rewritten+4<<20 {
		t.Errorf("VACUUM FULL grew the branch's data by %d bytes, more than the rewritten table (%d) plus 4 MiB: the old file was copied", g, rewritten)
	}
	if relPath(t, h.ctx, conn, "cow_frozen") == old {
		t.Error("VACUUM FULL kept the table's file")
	}
	if n := mustQueryInt(t, h.ctx, conn, `SELECT count(*) FROM cow_frozen`); n != h.rows {
		t.Errorf("cow_frozen has %d rows after VACUUM FULL, want %d", n, h.rows)
	}
	if n := mustQueryInt(t, h.ctx, conn, `SELECT count(*) FROM cow_raw WHERE v = 5`); n == 0 {
		t.Error("the new index finds nothing")
	}
}

// CREATE DATABASE copies a template that lives in the seed, with both
// strategies: FILE_COPY copies the files, WAL_LOG copies through WAL.
func (h *cowHarness) createDatabase(t *testing.T) {
	b := h.create(t, h.e, "cowt-db")
	conn := branchConn(b)
	for _, strategy := range []string{"FILE_COPY", "WAL_LOG"} {
		db := "cowt_" + strings.ToLower(strategy)
		start := time.Now()
		mustExec(t, h.ctx, conn, `CREATE DATABASE `+db+` TEMPLATE cowt_tpl STRATEGY `+strategy)
		t.Logf("CREATE DATABASE STRATEGY %s took %s", strategy, time.Since(start).Round(time.Millisecond))
		dbConn := strings.TrimSuffix(conn, "/postgres") + "/" + db
		if n := mustQueryInt(t, h.ctx, dbConn, `SELECT count(*) FROM tpl_t`); n != 20000 {
			t.Errorf("%s: the new database has %d template rows, want 20000", strategy, n)
		}
		mustExec(t, h.ctx, dbConn, `INSERT INTO tpl_t VALUES (0, 'new')`)
	}
	// the template itself is untouched
	tplConn := strings.TrimSuffix(conn, "/postgres") + "/cowt_tpl"
	if n := mustQueryInt(t, h.ctx, tplConn, `SELECT count(*) FROM tpl_t`); n != 20000 {
		t.Errorf("the template has %d rows, want 20000", n)
	}
}

// pgbench initializes and then runs TPC-B for up to 60 s; part-way the
// container is killed with SIGKILL. The branch comes back (docker's restart
// policy, else reconcile), recovers from the crash, and is then stopped and
// restarted by reconcile. Every committed transaction survives, TPC-B's
// balances add up, pg_amcheck --heapallindexed finds nothing, and the shim
// is active again after each restart.
func (h *cowHarness) pgbenchCrash(t *testing.T) {
	b := h.create(t, h.e, "cowt-bench")
	if out, err := h.d.ExecOutput(h.ctx, b.ContainerID, []string{"pgbench", "-i", "-s", "10", "-q", "postgres"}); err != nil {
		t.Fatalf("pgbench -i: %v\n%s", err, out)
	}
	done := make(chan error, 1)
	go func() {
		_, err := h.d.ExecOutput(h.ctx, b.ContainerID, []string{"pgbench", "-c", "4", "-j", "2", "-T", "60", "postgres"})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("pgbench ended before the kill: %v", err)
	case <-time.After(25 * time.Second):
	}
	cli := itDockerClient(t)
	if err := cli.ContainerKill(h.ctx, b.ContainerID, "KILL"); err != nil {
		t.Fatal(err)
	}
	<-done
	t.Log("killed the branch container with SIGKILL 25 s into a 60 s TPC-B run")
	b = h.awaitBack(t, b, false)
	h.checkBench(t, b)

	// reconcile's restart: a stopped container comes back on the same
	// volumes, with the shim
	timeout := 10
	if err := cli.ContainerStop(h.ctx, b.ContainerID, container.StopOptions{Timeout: &timeout}); err != nil {
		t.Fatal(err)
	}
	b = h.awaitBack(t, b, true)
	h.checkBench(t, b)
}

// awaitBack waits for a branch whose container died or stopped to serve
// again. mustReconcile says the runtime will not restart it on its own (a
// docker stop), so reconcile has to.
func (h *cowHarness) awaitBack(t *testing.T, b *registry.Branch, mustReconcile bool) *registry.Branch {
	t.Helper()
	oldID := b.ContainerID
	var passes []string
	for deadline := time.Now().Add(3 * time.Minute); ; {
		info, err := h.d.Inspect(h.ctx, oldID)
		switch {
		case err == nil && info.Running:
			// not stopped yet, or back through docker's restart policy
		default:
			plan, err := h.e.ApplyReconcile(h.ctx, time.Now(), 10*time.Minute)
			if err == nil && hasAction(plan, ActionRestartBranch, b.Name) {
				t.Logf("reconcile restarted %s", b.Name)
			} else {
				passes = append(passes, fmt.Sprintf("%+v (err %v)", plan.Actions, err))
			}
		}
		nb := h.reload(t, b)
		if nb.State == registry.BranchReady {
			if c, err := pgx.Connect(h.ctx, branchConn(nb)); err == nil {
				var one int
				qerr := c.QueryRow(h.ctx, "SELECT 1").Scan(&one)
				c.Close(h.ctx)
				if qerr == nil && (nb.ContainerID != oldID || !mustReconcile) {
					if nb.ContainerID == oldID {
						t.Log("docker's restart policy brought the container back")
					}
					return nb
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not come back: row %+v, reconcile passes %v", b.Name, nb, passes)
		}
		time.Sleep(time.Second)
	}
}

// checkBench verifies a TPC-B database after a restart.
func (h *cowHarness) checkBench(t *testing.T, b *registry.Branch) {
	t.Helper()
	h.wantMode(t, b, cow.CowModeLazyRW)
	conn := branchConn(b)
	if n := mustQueryInt(t, h.ctx, conn, `SELECT count(*) FROM pgbench_accounts`); n != 1000000 {
		t.Errorf("pgbench_accounts has %d rows, want 1000000", n)
	}
	// every committed TPC-B transaction added its delta to one account, one
	// teller, one branch and the history
	if n := mustQueryInt(t, h.ctx, conn, `SELECT count(*) FROM (
		SELECT (SELECT coalesce(sum(delta), 0) FROM pgbench_history) h,
		       (SELECT sum(abalance) FROM pgbench_accounts) a,
		       (SELECT sum(tbalance) FROM pgbench_tellers) t,
		       (SELECT sum(bbalance) FROM pgbench_branches) b) s
		WHERE h = a AND a = t AND t = b`); n != 1 {
		t.Error("TPC-B balances do not add up after the crash")
	}
	history := mustQueryInt(t, h.ctx, conn, `SELECT count(*) FROM pgbench_history`)
	out, err := h.d.ExecOutput(h.ctx, b.ContainerID, []string{"pg_amcheck", "--heapallindexed", "--install-missing", "-d", "postgres"})
	if err != nil {
		t.Fatalf("pg_amcheck --heapallindexed: %v\n%s", err, out)
	}
	t.Logf("after the restart: %d TPC-B transactions committed, balances add up, pg_amcheck --heapallindexed clean", history)
}

// A child branched off a branch reads the parent's writes, its reads copy
// nothing, and the two stay isolated.
func (h *cowHarness) branchFromBranch(t *testing.T) {
	parent := h.create(t, h.e, "cowt-parent")
	mustExec(t, h.ctx, branchConn(parent), `UPDATE cow_small SET v = -1 WHERE id = 1;
		INSERT INTO cow_small VALUES (1001, 1001);
		UPDATE cow_raw SET pad = 'parent' WHERE id = 7`)
	start := time.Now()
	child, err := h.e.CreateBranchFrom(h.ctx, "cowt-child", parent.Name, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("branch-from-branch took %s", time.Since(start).Round(time.Millisecond))
	h.destroyAtEnd(t, h.e, child.Name)
	parent = h.reload(t, parent)
	h.wantMode(t, parent, cow.CowModeLazyRW)
	h.wantMode(t, child, cow.CowModeLazyRW)
	cc := branchConn(child)
	if n := mustQueryInt(t, h.ctx, cc, `SELECT count(*) FROM cow_small WHERE (id = 1 AND v = -1) OR id = 1001`); n != 2 {
		t.Fatalf("the child sees %d of the parent's 2 small-table writes", n)
	}
	if n := mustQueryInt(t, h.ctx, cc, `SELECT count(*) FROM cow_raw WHERE id = 7 AND pad = 'parent'`); n != 1 {
		t.Fatal("the child does not see the parent's write to cow_raw")
	}
	for _, table := range []string{"cow_frozen", "cow_raw"} {
		g := h.growth(t, h.e, child, func(conn string) { h.readPass(t, conn, table) })
		t.Logf("child: read pass over %s +%d bytes", table, g)
		if g >= 2<<20 {
			t.Errorf("a read of %s in the child grew it by %d bytes, want < 2 MiB", table, g)
		}
	}
	g := h.growth(t, h.e, parent, func(conn string) { h.readPass(t, conn, "cow_frozen") })
	if g >= 2<<20 {
		t.Errorf("a read in the restarted parent grew it by %d bytes, want < 2 MiB", g)
	}
	mustExec(t, h.ctx, cc, `UPDATE cow_small SET v = -2 WHERE id = 2`)
	mustExec(t, h.ctx, branchConn(parent), `UPDATE cow_small SET v = -3 WHERE id = 3`)
	if n := mustQueryInt(t, h.ctx, branchConn(parent), `SELECT v FROM cow_small WHERE id = 2`); n != 2 {
		t.Errorf("the parent sees the child's write (v = %d)", n)
	}
	if n := mustQueryInt(t, h.ctx, cc, `SELECT v FROM cow_small WHERE id = 3`); n != 3 {
		t.Errorf("the child sees the parent's write made after the fork (v = %d)", n)
	}
}

// Diff reports a branch's schema change; reset discards its writes and the
// reset branch runs the shim and reads without copying.
func (h *cowHarness) diffReset(t *testing.T) {
	b := h.create(t, h.e, "cowt-reset")
	mustExec(t, h.ctx, branchConn(b), `CREATE TABLE cowt_new (x int); INSERT INTO cowt_new SELECT generate_series(1, 10);
		UPDATE cow_small SET v = 0; ANALYZE`)
	diff, err := h.e.DiffBranch(h.ctx, b.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff.SchemaDiff, "cowt_new") {
		t.Errorf("diff misses the new table:\n%s", diff.SchemaDiff)
	}
	before := h.usage(t, h.e, b)
	b, err = h.e.ResetBranch(h.ctx, b.Name)
	if err != nil {
		t.Fatal(err)
	}
	after := h.usage(t, h.e, b)
	t.Logf("reset: usage %d -> %d bytes", before, after)
	h.wantMode(t, b, cow.CowModeLazyRW)
	conn := branchConn(b)
	if n := mustQueryInt(t, h.ctx, conn, `SELECT count(*) FROM pg_class WHERE relname = 'cowt_new'`); n != 0 {
		t.Error("the new table survived the reset")
	}
	if n := mustQueryInt(t, h.ctx, conn, `SELECT count(*) FROM cow_small WHERE v = 0`); n != 0 {
		t.Error("the update survived the reset")
	}
	g := h.growth(t, h.e, b, func(conn string) { h.readPass(t, conn, "cow_raw") })
	if g >= 2<<20 {
		t.Errorf("a read in the reset branch grew it by %d bytes, want < 2 MiB", g)
	}
}

// The control: with --lazyrw=off the same read copies the table.
func (h *cowHarness) lazyrwOff(t *testing.T) {
	b := h.create(t, h.eOff, "cowt-off")
	h.wantMode(t, b, cow.CowModeOff)
	g, _ := h.dataGrowth(t, h.eOff, b, func(conn string) { h.readPass(t, conn, "cow_frozen") })
	t.Logf("--lazyrw=off: a read pass over cow_frozen (%d bytes) grew the branch's data by %d bytes", h.frozenBytes, g)
	if g < h.frozenBytes/2 {
		t.Errorf("with lazyrw off a read grew the branch by only %d bytes; the %d-byte table should have been copied", g, h.frozenBytes)
	}
}

// TestCowVolumeRoot: with --volume-root on a filesystem that reflinks
// (PGOVERLAY_IT_VOLUME_ROOT, a directory on the Docker host on XFS with
// reflink=1 or on btrfs), copy-up clones, the lazyrw shim still keeps reads
// from copying, and a write costs blocks, not the segment: branch usage
// (exclusive bytes) grows by a few KiB while du -sb sees the whole clone.
func TestCowVolumeRoot(t *testing.T) {
	if os.Getenv("PGOVERLAY_IT") != "1" {
		t.Skip("set PGOVERLAY_IT=1")
	}
	root := os.Getenv("PGOVERLAY_IT_VOLUME_ROOT")
	if root == "" {
		t.Skip("set PGOVERLAY_IT_VOLUME_ROOT to a directory on a reflink filesystem on the Docker host")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	rows := cowtRows(t) / 2
	host, port, network, hostConn := pgctltest.StartSourcePG(t, ctx)
	mustExec(t, ctx, hostConn, fmt.Sprintf(`CREATE TABLE cow_raw (id int, v int, pad text) WITH (autovacuum_enabled = off);
		INSERT INTO cow_raw SELECT g, g %% 1000, repeat(md5(g::text), 4) FROM generate_series(1, %d) g`, rows))
	tableBytes := int64(mustQueryInt(t, ctx, hostConn, `SELECT pg_relation_size('cow_raw')::int`))

	d, err := runtime.NewDockerDriver(runtime.WithVolumeRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.CheckVolumeRoot(ctx); err != nil {
		t.Fatal(err)
	}
	r, err := registry.Open(t.TempDir() + "/it.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	e := New(r, d, "postgres:17")
	res, err := e.DetectCopyUp(ctx, CopyUpOptions{Root: root, CowExtSize: DefaultCowExtSize})
	if err != nil {
		t.Fatalf("DetectCopyUp: %v (%s)", err, res)
	}
	t.Logf("copy-up probe on %s: %s", root, res)
	if res.Mode != cow.CopyUpClone {
		t.Fatalf("copy-up mode on %s = %s, want clone: put PGOVERLAY_IT_VOLUME_ROOT on XFS (reflink=1) or btrfs", root, res.Mode)
	}
	h := &cowHarness{t: t, ctx: ctx, d: d, r: r, e: e, rows: rows}
	src := &registry.Source{Name: "cowt-vr-src", PGVersion: "17", ConnHost: host, ConnPort: port, ConnUser: "postgres", Network: network}
	if err := e.AddSource(ctx, src, "secret"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.RemoveSource(context.Background(), src.Name); err != nil {
			t.Errorf("remove source: %v", err)
		}
	})
	h.src = src.Name
	b := h.create(t, e, "cowt-vr-b1")
	h.wantMode(t, b, cow.CowModeLazyRW)

	read := h.growth(t, e, b, func(conn string) {
		if n := mustQueryInt(t, ctx, conn, `SELECT count(*) FROM cow_raw`); n != rows {
			t.Fatalf("count = %d, want %d", n, rows)
		}
	})
	du0 := mustDuSb(t, ctx, d, b.RWVolume)
	write := h.growth(t, e, b, func(conn string) {
		mustExec(t, ctx, conn, `UPDATE cow_raw SET v = -1 WHERE id = 42`)
	})
	du1 := mustDuSb(t, ctx, d, b.RWVolume)
	t.Logf("volume root %s: read +%d bytes; one-row UPDATE of the %d-byte table: usage +%d bytes, du -sb +%d bytes", root, read, tableBytes, write, du1-du0)
	if read >= 2<<20 {
		t.Errorf("a read grew the branch by %d bytes, want < 2 MiB", read)
	}
	if du1-du0 < tableBytes/2 {
		t.Errorf("du -sb grew %d bytes: the write did not copy the table's file up", du1-du0)
	}
	if write >= 1<<20 {
		t.Errorf("clone mode: a one-row UPDATE grew the branch's exclusive bytes by %d, want < 1 MiB (block-level copy-on-write)", write)
	}
	if n := mustQueryInt(t, ctx, branchConn(h.reload(t, b)), `SELECT v FROM cow_raw WHERE id = 42`); n != -1 {
		t.Errorf("updated row reads %d", n)
	}
}
