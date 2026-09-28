package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/pgoverlay/internal/diffutil"
	"github.com/abd-ulbasit/pgoverlay/internal/registry"
	"github.com/abd-ulbasit/pgoverlay/internal/runtime"
)

// liveDiffBranches returns the names of live registry rows with the internal
// diff- prefix (there must be none once DiffBranch returns).
func liveDiffBranches(t *testing.T, r *registry.Registry) []string {
	t.Helper()
	live, err := r.ListLiveBranches()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, b := range live {
		if strings.HasPrefix(b.Name, "diff-") {
			out = append(out, b.Name)
		}
	}
	return out
}

// throwawaySpec returns the recorded StartBranch spec of the diff throwaway.
func throwawaySpec(t *testing.T, d *fakeDriver) runtime.BranchSpec {
	t.Helper()
	for _, s := range d.branches {
		if strings.HasPrefix(s.Name, "pgoverlay-br-diff-") {
			return s
		}
	}
	t.Fatalf("no throwaway diff branch started: %v", d.branches)
	return runtime.BranchSpec{}
}

// fakeTable is one table of an in-memory fake Postgres.
type fakeTable struct {
	pk        []string         // primary-key columns (nil = no primary key)
	reltuples int64            // planner estimate (-1 = never analyzed)
	bytes     int64            // heap size (0 = 8 KiB)
	rows      []map[string]any // actual rows; key values are numbers
}

// fakeDB is one instance's user tables.
type fakeDB map[tableKey]*fakeTable

// fakePG answers the diff's in-container commands from two fake databases:
// base (the throwaway clone — its container id carries pgoverlay-br-diff-)
// and branch (the diffed branch). It understands exactly the statement shapes
// diff.go issues and fails loudly on anything else.
type fakePG struct {
	base, branch         fakeDB
	baseDump, branchDump string
	// fail, when set, can fail a statement (base = run on the throwaway).
	fail func(base bool, sql string) error
}

var (
	fakeQualifiedRe = regexp.MustCompile(`"((?:[^"]|"")*)"\."((?:[^"]|"")*)"`)
	fakeTableRe     = regexp.MustCompile(`"((?:[^"]|"")*)"\."((?:[^"]|"")*)" AS t\b`)
	fakeLiteralRe   = regexp.MustCompile(`E'((?:[^']|'')*)'`)
	fakeLimitRe     = regexp.MustCompile(`LIMIT (\d+)$`)
)

func fakeUnquote(s string) string { return strings.ReplaceAll(s, `""`, `"`) }
func fakeUnliteral(s string) string {
	return strings.NewReplacer(`''`, `'`, `\\`, `\`).Replace(s)
}

// lookup resolves a quoted "schema"."table" match, like Postgres would.
func (db fakeDB) lookup(m []string) (*fakeTable, error) {
	t, ok := db[tableKey{fakeUnquote(m[1]), fakeUnquote(m[2])}]
	if !ok {
		return nil, fmt.Errorf(`ERROR: relation "%s.%s" does not exist`, fakeUnquote(m[1]), fakeUnquote(m[2]))
	}
	return t, nil
}

// keyOf renders a row's primary-key values as a comparable string.
func (t *fakeTable) keyOf(row map[string]any) string {
	parts := make([]string, len(t.pk))
	for i, c := range t.pk {
		parts[i] = fmt.Sprint(row[c])
	}
	return strings.Join(parts, "\x00")
}

func (t *fakeTable) sortedRows(desc bool) []map[string]any {
	rows := append([]map[string]any(nil), t.rows...)
	sort.SliceStable(rows, func(i, j int) bool {
		for _, c := range t.pk {
			a, b := rows[i][c].(float64), rows[j][c].(float64)
			if a != b {
				return (a < b) != desc
			}
		}
		return false
	})
	return rows
}

// fakeKeys extracts the JSON key list a presence/row query embeds.
func fakeKeys(sql string) []map[string]any {
	m := fakeLiteralRe.FindStringSubmatch(sql)
	var keys []map[string]any
	if m == nil || json.Unmarshal([]byte(fakeUnliteral(m[1])), &keys) != nil {
		panic("fake PG: no key list in " + sql)
	}
	return keys
}

func (f *fakePG) exec(id string, cmd []string) (string, error) {
	isBase := strings.Contains(id, "pgoverlay-br-diff-")
	db := f.branch
	if isBase {
		db = f.base
	}
	if len(cmd) > 0 && cmd[0] == "pg_dump" {
		if isBase {
			return f.baseDump, nil
		}
		return f.branchDump, nil
	}
	sql := cmd[len(cmd)-1]
	if f.fail != nil {
		if err := f.fail(isBase, sql); err != nil {
			return "", err
		}
	}
	var out strings.Builder
	switch {
	case strings.Contains(sql, "reltuples"):
		type stat struct {
			Schema string `json:"schema"`
			Table  string `json:"table"`
			Rows   int64  `json:"rows"`
			Bytes  int64  `json:"bytes"`
		}
		var stats []stat
		for k, t := range db {
			bytes := t.bytes
			if bytes == 0 {
				bytes = 8192
			}
			stats = append(stats, stat{k.schema, k.table, t.reltuples, bytes})
		}
		b, _ := json.Marshal(stats)
		return string(b), nil
	case strings.HasPrefix(sql, "SELECT to_json(ARRAY["):
		counts := []int{}
		for _, m := range fakeQualifiedRe.FindAllStringSubmatch(sql, -1) {
			t, err := db.lookup(m)
			if err != nil {
				return "", err
			}
			counts = append(counts, len(t.rows))
		}
		b, _ := json.Marshal(counts)
		return string(b), nil
	case strings.Contains(sql, "indisprimary"):
		lits := fakeLiteralRe.FindAllStringSubmatch(sql, -1)
		pairs := [][2]string{}
		if t := db[tableKey{fakeUnliteral(lits[0][1]), fakeUnliteral(lits[1][1])}]; t != nil {
			for _, c := range t.pk {
				pairs = append(pairs, [2]string{c, "bigint"})
			}
		}
		b, _ := json.Marshal(pairs)
		return string(b), nil
	case strings.Contains(sql, "jsonb_build_object("):
		t, err := db.lookup(fakeTableRe.FindStringSubmatch(sql))
		if err != nil {
			return "", err
		}
		limit, _ := strconv.Atoi(fakeLimitRe.FindStringSubmatch(sql)[1])
		for i, row := range t.sortedRows(true) {
			if i == limit {
				break
			}
			key := map[string]any{}
			for _, c := range t.pk {
				key[c] = row[c]
			}
			b, _ := json.Marshal(key)
			out.Write(append(b, '\n'))
		}
		return out.String(), nil
	case strings.Contains(sql, "WITH ORDINALITY"):
		t, err := db.lookup(fakeTableRe.FindStringSubmatch(sql))
		if err != nil {
			return "", err
		}
		have := map[string]bool{}
		for _, row := range t.rows {
			have[t.keyOf(row)] = true
		}
		for i, k := range fakeKeys(sql) {
			if have[t.keyOf(k)] {
				fmt.Fprintf(&out, "%d\n", i+1)
			}
		}
		return out.String(), nil
	case strings.Contains(sql, "to_jsonb(t.*)"):
		t, err := db.lookup(fakeTableRe.FindStringSubmatch(sql))
		if err != nil {
			return "", err
		}
		want := map[string]bool{}
		for _, k := range fakeKeys(sql) {
			want[t.keyOf(k)] = true
		}
		for _, row := range t.sortedRows(false) {
			if want[t.keyOf(row)] {
				b, _ := json.Marshal(row)
				out.Write(append(b, '\n'))
			}
		}
		return out.String(), nil
	}
	return "", fmt.Errorf("fake PG: unexpected SQL %q", sql)
}

// idRows returns rows {"id": n} for each n.
func idRows(ids ...int) []map[string]any {
	out := make([]map[string]any, len(ids))
	for i, id := range ids {
		out[i] = map[string]any{"id": float64(id), "note": fmt.Sprintf("row %d", id)}
	}
	return out
}

func idRange(from, to int) []int {
	var out []int
	for i := from; i <= to; i++ {
		out = append(out, i)
	}
	return out
}

func pub(table string) tableKey { return tableKey{"public", table} }

// diffFake is the default two-instance setup: the branch added table
// diffdemo, shrank users, and both sides have a never-analyzed small table
// "fresh" (reltuples -1) that must be counted, not reported as 0.
func diffFake() *fakePG {
	return &fakePG{
		baseDump:   "CREATE TABLE users (\n    id integer\n);\n",
		branchDump: "CREATE TABLE users (\n    id integer\n);\nCREATE TABLE diffdemo (\n    x integer\n);\n",
		base: fakeDB{
			pub("fresh"): {reltuples: -1, rows: idRows(1, 2, 3)},
			pub("users"): {reltuples: 100},
		},
		branch: fakeDB{
			pub("diffdemo"): {reltuples: 42},
			pub("fresh"):    {reltuples: -1, rows: idRows(1, 2, 3)},
			pub("users"):    {reltuples: 90},
		},
	}
}

// tablesByName indexes a diff's tables by display name.
func tablesByName(res *DiffResult) map[string]TableDelta {
	out := map[string]TableDelta{}
	for _, td := range res.Tables {
		out[td.Name()] = td
	}
	return out
}

// sqlsRun lists the SQL of every psql ExecOutput, tagged base/branch.
func sqlsRun(d *fakeDriver) (base, branch []string) {
	for i, c := range d.execOuts {
		if c[0] != "psql" {
			continue
		}
		if strings.Contains(d.execOutIDs[i], "pgoverlay-br-diff-") {
			base = append(base, c[len(c)-1])
		} else {
			branch = append(branch, c[len(c)-1])
		}
	}
	return base, branch
}

func TestDiffBranchHappyPath(t *testing.T) {
	d := newFake()
	d.execOutFn = diffFake().exec
	e, r := testEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}

	res, err := e.DiffBranch(context.Background(), "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	// schema diff is base -> branch: the added table shows as an insertion
	for _, want := range []string{"+CREATE TABLE diffdemo (", "+    x integer", " CREATE TABLE users ("} {
		if !strings.Contains(res.SchemaDiff, want+"\n") {
			t.Errorf("SchemaDiff missing %q:\n%s", want, res.SchemaDiff)
		}
	}
	if strings.Contains(res.SchemaDiff, "-CREATE") {
		t.Errorf("SchemaDiff has unexpected deletions:\n%s", res.SchemaDiff)
	}
	// tables: union of both sides, sorted, one-sided tables count 0 on the
	// other side; "fresh" has no estimate anywhere and is counted exactly
	want := []TableDelta{
		{Schema: "public", Table: "diffdemo", BaseRows: 0, BranchRows: 42, Delta: 42},
		{Schema: "public", Table: "fresh", BaseRows: 3, BranchRows: 3, Delta: 0},
		{Schema: "public", Table: "users", BaseRows: 100, BranchRows: 90, Delta: -10},
	}
	if fmt.Sprintf("%v", res.Tables) != fmt.Sprintf("%v", want) {
		t.Errorf("Tables = %+v, want %+v", res.Tables, want)
	}

	// dump + stats commands ran in-container over the local socket
	var dumpCmds, statsCmds int
	for _, c := range d.execOuts {
		got := strings.Join(c, " ")
		switch {
		case c[0] == "pg_dump":
			dumpCmds++
			wantCmd := "pg_dump -U postgres -h /var/run/postgresql --schema-only --no-owner --no-acl postgres"
			if got != wantCmd {
				t.Errorf("pg_dump cmd = %q, want %q", got, wantCmd)
			}
		case strings.Contains(got, "reltuples"):
			statsCmds++
			if !strings.HasPrefix(got, "psql -tA -v ON_ERROR_STOP=1 -U postgres -d postgres -h /var/run/postgresql -c ") {
				t.Errorf("row estimate cmd = %q", got)
			}
		}
	}
	if dumpCmds != 2 || statsCmds != 2 {
		t.Errorf("dump cmds = %d, stats cmds = %d, want 2 each", dumpCmds, statsCmds)
	}

	// throwaway cleaned up: container, volume and registry row all gone
	if names := liveDiffBranches(t, r); len(names) != 0 {
		t.Errorf("throwaway rows left: %v", names)
	}
	for v := range d.volumes {
		if strings.Contains(v, "diff-") {
			t.Errorf("throwaway volume leaked: %s", v)
		}
	}
	for c := range d.containers {
		if strings.Contains(c, "diff-") {
			t.Errorf("throwaway container leaked: %s", c)
		}
	}
}

// DIFF-05: a table the planner has no estimate for (reltuples -1) is never
// reported as 0 rows. Small tables are counted exactly on both sides; a big
// one stays unknown ("?"), with no delta and no grown classification.
func TestDiffBranchUnknownRowCountsAreNotZero(t *testing.T) {
	pg := &fakePG{
		base: fakeDB{
			pub("big"):   {reltuples: -1, bytes: 1 << 30, pk: []string{"id"}},
			pub("small"): {reltuples: -1, rows: idRows(1, 2, 3, 4, 5)},
			pub("known"): {reltuples: 100, rows: idRows(1, 2)},
		},
		branch: fakeDB{
			pub("big"):   {reltuples: 99978, bytes: 1 << 30, pk: []string{"id"}},
			pub("small"): {reltuples: 9, rows: idRows(1, 2, 3, 4, 5, 6, 7)},
			pub("known"): {reltuples: 100, rows: idRows(1, 2)},
			pub("newt"):  {reltuples: -1, rows: idRows(1, 2, 3, 4)},
		},
	}
	d := newFake()
	d.execOutFn = pg.exec
	e, r := testEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}

	res, err := e.DiffBranch(context.Background(), "pr-1", WithDataSample(20))
	if err != nil {
		t.Fatal(err)
	}
	got := tablesByName(res)

	// the live case: never analyzed on the base, analyzed on the branch, too
	// big to count — unknown, not "+99978"
	big := got["big"]
	if !big.RowsUnknown || big.BaseRows != UnknownRows || big.BranchRows != 99978 || big.Delta != 0 {
		t.Errorf("big = %+v, want unknown base (-1), branch 99978, delta 0", big)
	}
	if b, br, dl := big.Cells(); b != "?" || br != "99978" || dl != "?" {
		t.Errorf("big cells = %q %q %q, want ? 99978 ?", b, br, dl)
	}
	if big.Grew() || big.SampleRows != nil {
		t.Errorf("unknown table classified as grown / sampled: %+v", big)
	}
	// small and unestimated on one side: counted exactly on BOTH sides
	if s := got["small"]; s.RowsUnknown || s.BaseRows != 5 || s.BranchRows != 7 || s.Delta != 2 {
		t.Errorf("small = %+v, want exact 5 -> 7 (+2)", s)
	}
	// estimated on both sides: left alone
	if k := got["known"]; k.BaseRows != 100 || k.BranchRows != 100 || k.Delta != 0 {
		t.Errorf("known = %+v, want estimates 100/100", k)
	}
	// new on the branch, never analyzed: counted exactly
	if n := got["newt"]; n.RowsUnknown || n.BaseRows != 0 || n.BranchRows != 4 || n.Delta != 4 {
		t.Errorf("newt = %+v, want 0 -> 4 (+4)", n)
	}

	// the big table's heap is never scanned; only unestimated tables counted
	base, branch := sqlsRun(d)
	for _, sql := range append(base, branch...) {
		if strings.Contains(sql, "count(*)") && (strings.Contains(sql, `"big"`) || strings.Contains(sql, `"known"`)) {
			t.Errorf("counted a big or already-estimated table: %q", sql)
		}
		if strings.Contains(sql, "E'big'") {
			t.Errorf("sampled a table with an unknown count: %q", sql)
		}
	}
}

// A failed exact count only costs precision: estimates are kept and a side
// with none stays unknown; the diff still succeeds.
func TestDiffBranchExactCountFailureKeepsEstimates(t *testing.T) {
	pg := &fakePG{
		base:   fakeDB{pub("small"): {reltuples: -1, rows: idRows(1, 2)}},
		branch: fakeDB{pub("small"): {reltuples: 3, rows: idRows(1, 2, 3)}},
		fail: func(base bool, sql string) error {
			if base && strings.Contains(sql, "count(*)") {
				return errors.New("ERROR: permission denied")
			}
			return nil
		},
	}
	d := newFake()
	d.execOutFn = pg.exec
	e, r := testEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}
	res, err := e.DiffBranch(context.Background(), "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	s := tablesByName(res)["small"]
	if !s.RowsUnknown || s.BaseRows != UnknownRows || s.BranchRows != 3 {
		t.Errorf("small = %+v, want base unknown, branch exact 3", s)
	}
}

// DIFF-03: tables are keyed by schema — same-named tables in different
// schemas stay separate, names with a '|' parse, and every sampling query is
// schema-qualified (a table outside search_path must not break the diff).
func TestDiffBranchSchemaScopedTables(t *testing.T) {
	pg := &fakePG{
		base: fakeDB{
			{"app", "orders"}:     {reltuples: 10, pk: []string{"id"}, rows: idRows(idRange(1, 10)...)},
			{"archive", "orders"}: {reltuples: 500, pk: []string{"id"}},
			pub("a|b"):            {reltuples: 3},
		},
		branch: fakeDB{
			{"app", "orders"}:     {reltuples: 13, pk: []string{"id"}, rows: idRows(idRange(1, 13)...)},
			{"archive", "orders"}: {reltuples: 500, pk: []string{"id"}},
			pub("a|b"):            {reltuples: 5},
		},
	}
	d := newFake()
	d.execOutFn = pg.exec
	e, r := testEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}

	res, err := e.DiffBranch(context.Background(), "pr-1", WithDataSample(20))
	if err != nil {
		t.Fatal(err)
	}
	want := []TableDelta{
		{Schema: "app", Table: "orders", BaseRows: 10, BranchRows: 13, Delta: 3},
		{Schema: "archive", Table: "orders", BaseRows: 500, BranchRows: 500, Delta: 0},
		{Schema: "public", Table: "a|b", BaseRows: 3, BranchRows: 5, Delta: 2},
	}
	if len(res.Tables) != len(want) {
		t.Fatalf("tables = %+v, want %+v", res.Tables, want)
	}
	for i, w := range want {
		g := res.Tables[i]
		if g.Schema != w.Schema || g.Table != w.Table || g.BaseRows != w.BaseRows || g.BranchRows != w.BranchRows || g.Delta != w.Delta {
			t.Errorf("tables[%d] = %+v, want %+v", i, g, w)
		}
	}
	if n := res.Tables[0].Name(); n != "app.orders" {
		t.Errorf("Name() = %q, want app.orders", n)
	}
	if n := res.Tables[2].Name(); n != "a|b" {
		t.Errorf("Name() = %q, want a|b (public stays bare)", n)
	}
	// app.orders grew by ids 11-13, found via schema-qualified queries
	var ids []float64
	for _, row := range res.Tables[0].SampleRows {
		ids = append(ids, row["id"].(float64))
	}
	if fmt.Sprint(ids) != "[11 12 13]" {
		t.Errorf("app.orders samples = %v, want [11 12 13]", ids)
	}
	base, branch := sqlsRun(d)
	for _, sql := range append(base, branch...) {
		if strings.Contains(sql, "indisprimary") && strings.Contains(sql, "E'orders'") && !strings.Contains(sql, "n.nspname=E'app'") {
			t.Errorf("primary-key lookup not scoped by schema: %q", sql)
		}
		if (strings.Contains(sql, "jsonb_build_object(") || strings.Contains(sql, "WITH ORDINALITY") || strings.Contains(sql, "to_jsonb(t.*)")) &&
			!strings.Contains(sql, `"app"."orders" AS t`) {
			t.Errorf("sampling query not schema-qualified: %q", sql)
		}
	}
}

// DIFF-02: a table that exists only on the branch is sampled without ever
// querying the base for it — every branch row is new.
func TestDiffBranchDataSampleBranchOnlyTable(t *testing.T) {
	pg := &fakePG{
		base: fakeDB{pub("users"): {reltuples: 10}},
		branch: fakeDB{
			pub("users"):     {reltuples: 10},
			pub("audit_log"): {reltuples: 30, pk: []string{"id"}, rows: idRows(idRange(1, 30)...)},
		},
	}
	d := newFake()
	d.execOutFn = pg.exec
	e, r := testEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}

	res, err := e.DiffBranch(context.Background(), "pr-1", WithDataSample(5))
	if err != nil {
		t.Fatalf("diff with a branch-only PK table: %v", err)
	}
	audit := tablesByName(res)["audit_log"]
	var ids []float64
	for _, row := range audit.SampleRows {
		ids = append(ids, row["id"].(float64))
	}
	// capped at 5: the newest keys, shown in key order
	if fmt.Sprint(ids) != "[26 27 28 29 30]" {
		t.Errorf("audit_log samples = %v, want [26 27 28 29 30]", ids)
	}
	base, _ := sqlsRun(d)
	for _, sql := range base {
		if strings.Contains(sql, "audit_log") {
			t.Errorf("queried the base for a table it does not have: %q", sql)
		}
	}
}

// Sampling finds exactly the rows whose key the base lacks, even when the
// table holds more rows than the cap and the branch deleted rows too (a
// first-N-by-key comparison reports phantom "new" rows here).
func TestDiffBranchDataSampleBranchOnlyByPK(t *testing.T) {
	branchItems := idRows(append(append(idRange(1, 4), idRange(6, 50)...), 51, 52, 53)...)
	pg := &fakePG{
		base: fakeDB{
			pub("items"): {reltuples: 50, pk: []string{"id"}, rows: idRows(idRange(1, 50)...)},
			pub("nopk"):  {reltuples: 0},
			pub("users"): {reltuples: 100, pk: []string{"id"}},
		},
		branch: fakeDB{
			pub("items"): {reltuples: 52, pk: []string{"id"}, rows: branchItems},
			pub("nopk"):  {reltuples: 5},
			pub("users"): {reltuples: 90, pk: []string{"id"}},
		},
	}
	d := newFake()
	d.execOutFn = pg.exec
	e, r := testEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}

	res, err := e.DiffBranch(context.Background(), "pr-1", WithDataSample(20))
	if err != nil {
		t.Fatal(err)
	}
	byName := tablesByName(res)

	var ids []float64
	for _, row := range byName["items"].SampleRows {
		ids = append(ids, row["id"].(float64))
		if row["note"] == nil {
			t.Errorf("sample row is not the full row: %v", row)
		}
	}
	if fmt.Sprint(ids) != "[51 52 53]" {
		t.Errorf("items branch-only ids = %v, want [51 52 53]", ids)
	}
	// nopk grew but has no primary key: skipped (nil samples)
	if byName["nopk"].SampleRows != nil {
		t.Errorf("nopk has SampleRows despite no PK: %v", byName["nopk"].SampleRows)
	}
	// users shrank: not a grown table, never sampled
	if byName["users"].SampleRows != nil {
		t.Errorf("users (shrank) has SampleRows: %v", byName["users"].SampleRows)
	}
	// the key scan reads the highest keys, bounded
	var sawScan bool
	_, branch := sqlsRun(d)
	for _, sql := range branch {
		if strings.Contains(sql, "jsonb_build_object(") {
			sawScan = true
			if !strings.Contains(sql, `ORDER BY t."id" DESC LIMIT `+strconv.Itoa(sampleScanKeys(20))) {
				t.Errorf("key scan not ordered/capped: %q", sql)
			}
		}
	}
	if !sawScan {
		t.Error("no key scan issued")
	}
}

// A table whose sampling fails (here: its key column differs on the base) is
// left without samples; the other tables and the diff itself are unaffected.
func TestDiffBranchDataSampleSkipsFailingTable(t *testing.T) {
	pg := &fakePG{
		base: fakeDB{
			pub("a"): {reltuples: 1, pk: []string{"id"}, rows: idRows(1)},
			pub("b"): {reltuples: 1, pk: []string{"id"}, rows: idRows(1)},
		},
		branch: fakeDB{
			pub("a"): {reltuples: 2, pk: []string{"id"}, rows: idRows(1, 2)},
			pub("b"): {reltuples: 2, pk: []string{"id"}, rows: idRows(1, 2)},
		},
		fail: func(base bool, sql string) error {
			if base && strings.Contains(sql, `"public"."a" AS t`) {
				return errors.New(`ERROR: column t.id does not exist`)
			}
			return nil
		},
	}
	d := newFake()
	d.execOutFn = pg.exec
	e, r := testEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}
	res, err := e.DiffBranch(context.Background(), "pr-1", WithDataSample(20))
	if err != nil {
		t.Fatalf("one table's sampling failure failed the diff: %v", err)
	}
	byName := tablesByName(res)
	if byName["a"].SampleRows != nil {
		t.Errorf("a has samples despite its sampling failing: %v", byName["a"].SampleRows)
	}
	if len(byName["b"].SampleRows) != 1 {
		t.Errorf("b samples = %v, want the one new row", byName["b"].SampleRows)
	}
}

// TestDiffBranchNoDataSampleByDefault: without WithDataSample, no table
// carries SampleRows and no sampling queries are issued.
func TestDiffBranchNoDataSampleByDefault(t *testing.T) {
	d := newFake()
	d.execOutFn = diffFake().exec
	e, r := testEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}

	res, err := e.DiffBranch(context.Background(), "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, td := range res.Tables {
		if td.SampleRows != nil {
			t.Errorf("table %s has SampleRows without WithDataSample: %v", td.Table, td.SampleRows)
		}
	}
	for _, c := range d.execOuts {
		if joined := strings.Join(c, " "); strings.Contains(joined, "jsonb") || strings.Contains(joined, "indisprimary") {
			t.Errorf("unexpected sampling query without WithDataSample: %q", joined)
		}
	}
}

// WithDataSample(0) falls back to the default cap rather than disabling, and
// huge requests are clamped so a caller cannot pull whole tables.
func TestWithDataSampleCaps(t *testing.T) {
	for n, want := range map[int]int{0: defaultSampleRows, -4: defaultSampleRows, 7: 7, 1 << 30: maxSampleRows} {
		var o diffOptions
		WithDataSample(n)(&o)
		if o.sample != want {
			t.Errorf("WithDataSample(%d) = %d, want %d", n, o.sample, want)
		}
	}
}

func TestTableDeltaDisplay(t *testing.T) {
	for _, tc := range []struct {
		td                        TableDelta
		name, base, branch, delta string
		grew                      bool
	}{
		{TableDelta{Schema: "public", Table: "users", BaseRows: 10, BranchRows: 12, Delta: 2}, "users", "10", "12", "+2", true},
		{TableDelta{Table: "legacy", BaseRows: 5, BranchRows: 5}, "legacy", "5", "5", "0", false},
		{TableDelta{Schema: "app", Table: "orders", BaseRows: 9, BranchRows: 4, Delta: -5}, "app.orders", "9", "4", "-5", false},
		{TableDelta{Schema: "public", Table: "big", BaseRows: UnknownRows, BranchRows: 7, RowsUnknown: true}, "big", "?", "7", "?", false},
	} {
		base, branch, delta := tc.td.Cells()
		if tc.td.Name() != tc.name || base != tc.base || branch != tc.branch || delta != tc.delta || tc.td.Grew() != tc.grew {
			t.Errorf("%+v: got %q %q %q %q grew=%v", tc.td, tc.td.Name(), base, branch, delta, tc.td.Grew())
		}
	}
}

func TestDiffBranchClonesTargetOwnBaseGeneration(t *testing.T) {
	d := newFake()
	d.execOutFn = diffFake().exec
	e, r := testEngine(t, d)
	readySource(t, r)
	d.volumes["pgoverlay-src-main"] = true
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}
	// the source moves on to generation 2; pr-1 stays pinned to gen 1
	if err := e.RefreshSource(context.Background(), "main", "secret"); err != nil {
		t.Fatal(err)
	}

	if _, err := e.DiffBranch(context.Background(), "pr-1"); err != nil {
		t.Fatal(err)
	}
	spec := throwawaySpec(t, d)
	if len(spec.Mounts) == 0 || spec.Mounts[0].Volume != "pgoverlay-src-main" {
		t.Errorf("throwaway lower0 = %+v, want the target's own gen-1 volume pgoverlay-src-main", spec.Mounts)
	}
	for _, m := range spec.Mounts {
		if m.Volume == "pgoverlay-src-main-g2" {
			t.Errorf("throwaway mounted the CURRENT generation volume: %+v", spec.Mounts)
		}
	}
}

func TestDiffBranchClonesTargetLayerChain(t *testing.T) {
	d := newFake()
	d.execOutFn = diffFake().exec
	e, r := testEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "p", "main", 0); err != nil {
		t.Fatal(err)
	}
	// freeze p into a layer; child bases on [frozen p rw, source]
	if _, err := e.CreateBranchFrom(context.Background(), "c", "p", 0); err != nil {
		t.Fatal(err)
	}

	if _, err := e.DiffBranch(context.Background(), "c"); err != nil {
		t.Fatal(err)
	}
	spec := throwawaySpec(t, d)
	var vols []string
	for _, m := range spec.Mounts {
		vols = append(vols, m.Volume)
	}
	// same stack as the child: source at lower0, frozen parent rw at lower1
	if len(vols) != 3 || vols[0] != "pgoverlay-src-main" || vols[1] != "pgoverlay-br-p-rw" {
		t.Errorf("throwaway stack = %v, want [pgoverlay-src-main pgoverlay-br-p-rw <its own rw>]", vols)
	}
	if names := liveDiffBranches(t, r); len(names) != 0 {
		t.Errorf("throwaway rows left: %v", names)
	}
}

// DIFF-04a: a csi child's base is its parent's live PVC, so the diff's clone
// quiesces the parent exactly like a child reset: CHECKPOINT, stop, clone,
// restart — and the parent ends up ready again.
func TestCSIDiffChildQuiescesParent(t *testing.T) {
	d := newFake()
	d.execOutFn = diffFake().exec
	e, r := csiEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateBranchFrom(context.Background(), "pr-2", "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	mark := len(d.log)

	if _, err := e.DiffBranch(context.Background(), "pr-2", WithParentQuiesce()); err != nil {
		t.Fatal(err)
	}
	idx := func(pred func(string) bool) int {
		for i := mark; i < len(d.log); i++ {
			if pred(d.log[i]) {
				return i
			}
		}
		return -1
	}
	ckpt := idx(func(s string) bool { return s == "exec:psql:cid-pgoverlay-br-pr-1" })
	stop := idx(func(s string) bool { return s == "stop:cid-pgoverlay-br-pr-1" })
	clone := idx(func(s string) bool {
		return strings.HasPrefix(s, "clone:pgoverlay-br-pr-1-rw>pgoverlay-br-diff-")
	})
	restart := idx(func(s string) bool { return s == "start:pgoverlay-br-pr-1" })
	if !(ckpt >= 0 && ckpt < stop && stop < clone && clone < restart) {
		t.Fatalf("order ckpt=%d stop=%d clone=%d restart=%d; log=%v", ckpt, stop, clone, restart, d.log[mark:])
	}
	p, err := r.GetBranchByName("pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if p.State != registry.BranchReady || !d.containers[p.ContainerID] {
		t.Fatalf("parent after the child's diff: %+v", p)
	}
	if names := liveDiffBranches(t, r); len(names) != 0 {
		t.Errorf("throwaway rows left: %v", names)
	}
}

// Stopping the parent is a reset-like effect on another branch, so a csi
// child's diff needs WithParentQuiesce (the API grants it to operators only).
// Without it the diff is refused up-front: nothing is created, the parent is
// neither checkpointed nor stopped, and no clone is made. A csi branch of the
// source clones the source PVC and needs no permission.
func TestCSIDiffChildRefusesParentQuiesceUnlessAllowed(t *testing.T) {
	d := newFake()
	d.execOutFn = diffFake().exec
	e, r := csiEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateBranchFrom(context.Background(), "pr-2", "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	before, err := r.GetBranchByName("pr-1")
	if err != nil {
		t.Fatal(err)
	}
	mark, clones := len(d.log), len(d.clones)

	_, err = e.DiffBranch(context.Background(), "pr-2", WithDataSample(5))
	if !errors.Is(err, ErrParentQuiesce) || !strings.Contains(err.Error(), `parent branch "pr-1"`) {
		t.Fatalf("err = %v, want ErrParentQuiesce naming the parent", err)
	}
	if len(d.clones) != clones || len(d.log) != mark {
		t.Fatalf("a refused diff touched the runtime: clones=%v log=%v", d.clones[clones:], d.log[mark:])
	}
	after, err := r.GetBranchByName("pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if after.State != registry.BranchReady || after.ContainerID != before.ContainerID || !d.containers[after.ContainerID] {
		t.Fatalf("parent after a refused diff: %+v (was %+v)", after, before)
	}
	if names := liveDiffBranches(t, r); len(names) != 0 {
		t.Errorf("throwaway rows left: %v", names)
	}

	if _, err := e.DiffBranch(context.Background(), "pr-1"); err != nil {
		t.Fatalf("diff of a csi branch of the source: %v", err)
	}
}

// DIFF-04b: a csi parent may be destroyed while its children live, but a
// child's reset and diff re-clone the parent's PVC — both are refused
// up-front, before the child's own PVC is touched.
func TestCSIChildOfDestroyedParentRefusesResetAndDiff(t *testing.T) {
	d := newFake()
	d.execOutFn = diffFake().exec
	e, r := csiEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateBranchFrom(context.Background(), "pr-2", "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	if err := e.DestroyBranch(context.Background(), "pr-1"); err != nil {
		t.Fatal(err)
	}
	clones := len(d.clones)

	if _, err := e.DiffBranch(context.Background(), "pr-2"); err == nil || !strings.Contains(err.Error(), "destroyed") {
		t.Fatalf("diff err = %v, want a refusal naming the destroyed parent", err)
	}
	if _, err := e.ResetBranch(context.Background(), "pr-2"); err == nil || !strings.Contains(err.Error(), "destroyed") {
		t.Fatalf("reset err = %v, want a refusal naming the destroyed parent", err)
	}
	if len(d.clones) != clones {
		t.Fatalf("a clone was attempted: %v", d.clones[clones:])
	}
	c, err := r.GetBranchByName("pr-2")
	if err != nil {
		t.Fatal(err)
	}
	if c.State != registry.BranchReady || !d.volumes["pgoverlay-br-pr-2-rw"] || !d.containers[c.ContainerID] {
		t.Fatalf("child after refused reset: %+v (volume kept=%v)", c, d.volumes["pgoverlay-br-pr-2-rw"])
	}
}

// A new branch reusing the destroyed parent's name (and so its PVC name) is
// not the child's base: reset and diff are still refused.
func TestCSIChildRefusesResetWhenParentNameReused(t *testing.T) {
	d := newFake()
	d.execOutFn = diffFake().exec
	e, r := csiEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateBranchFrom(context.Background(), "pr-2", "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	if err := e.DestroyBranch(context.Background(), "pr-1"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond) // created_at has millisecond resolution
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}

	if _, err := e.ResetBranch(context.Background(), "pr-2"); err == nil || !strings.Contains(err.Error(), "destroyed") {
		t.Fatalf("reset err = %v, want a refusal: the new pr-1 is not pr-2's base", err)
	}
	if _, err := e.DiffBranch(context.Background(), "pr-2"); err == nil {
		t.Fatal("diff against a reused parent name succeeded")
	}
	// the live parent path still works
	if _, err := e.CreateBranchFrom(context.Background(), "pr-3", "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ResetBranch(context.Background(), "pr-3"); err != nil {
		t.Fatalf("reset of a child with a live parent: %v", err)
	}
}

func TestDiffBranchDestroysThrowawayWhenDumpFails(t *testing.T) {
	d := newFake()
	d.execOutFn = func(id string, cmd []string) (string, error) {
		if len(cmd) > 0 && cmd[0] == "pg_dump" {
			return "", errors.New("pg_dump: boom")
		}
		return "", nil
	}
	e, r := testEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}

	if _, err := e.DiffBranch(context.Background(), "pr-1"); err == nil {
		t.Fatal("want error when pg_dump fails")
	}
	if names := liveDiffBranches(t, r); len(names) != 0 {
		t.Errorf("throwaway rows left after failed dump: %v", names)
	}
	for v := range d.volumes {
		if strings.Contains(v, "diff-") {
			t.Errorf("throwaway volume leaked: %s", v)
		}
	}
	for c := range d.containers {
		if strings.Contains(c, "diff-") {
			t.Errorf("throwaway container leaked: %s", c)
		}
	}
	// the target branch is untouched
	b, err := r.GetBranchByName("pr-1")
	if err != nil || b.State != registry.BranchReady {
		t.Fatalf("target after failed diff: %+v, %v", b, err)
	}
}

func TestDiffBranchDestroysThrowawayWhenProvisionFails(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}
	d.failStart = true // throwaway instance never starts

	if _, err := e.DiffBranch(context.Background(), "pr-1"); err == nil {
		t.Fatal("want error when the throwaway cannot be provisioned")
	}
	if names := liveDiffBranches(t, r); len(names) != 0 {
		t.Errorf("throwaway rows left after failed provision: %v", names)
	}
	for v := range d.volumes {
		if strings.Contains(v, "diff-") {
			t.Errorf("throwaway volume leaked: %s", v)
		}
	}
}

func TestDiffBranchRequiresReadyTarget(t *testing.T) {
	d := newFake()
	d.failStart = true
	e, r := testEngine(t, d)
	readySource(t, r)
	e.CreateBranch(context.Background(), "pr-1", "main", 0) // fails -> failed state

	_, err := e.DiffBranch(context.Background(), "pr-1")
	if err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("err = %v, want not-ready refusal", err)
	}
	if got := len(d.branches); got != 0 {
		t.Fatalf("throwaway provisioned for a non-ready target: %v", d.branches)
	}

	if _, err := e.DiffBranch(context.Background(), "nope"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound for unknown branch", err)
	}
}

func TestStripDumpNoise(t *testing.T) {
	in := "--\nCREATE TABLE t (id int);\n\\restrict aB3xQ\nSET x=1;\n\\unrestrict zZ9kP\n"
	got := stripDumpNoise(in)
	if strings.Contains(got, "restrict") {
		t.Fatalf("restrict/unrestrict lines not stripped:\n%s", got)
	}
	for _, keep := range []string{"CREATE TABLE t (id int);", "SET x=1;"} {
		if !strings.Contains(got, keep) {
			t.Fatalf("stripped a real line %q:\n%s", keep, got)
		}
	}
	// two dumps differing only by the random nonce token must diff to empty
	a := "CREATE TABLE t (id int);\n\\restrict TOKENAAA\n"
	b := "CREATE TABLE t (id int);\n\\restrict TOKENBBB\n"
	if d := diffutil.Unified(stripDumpNoise(a), stripDumpNoise(b)); d != "" {
		t.Fatalf("nonce-only difference produced a diff:\n%s", d)
	}
}

func TestSQLLiteralEscapesQuotesAndBackslashes(t *testing.T) {
	if got, want := sqlLiteral(`it's a\b`), `E'it''s a\\b'`; got != want {
		t.Errorf("sqlLiteral = %s, want %s", got, want)
	}
	if got, want := (tableKey{`we"ird`, "t"}).sql(), `"we""ird"."t"`; got != want {
		t.Errorf("tableKey.sql = %s, want %s", got, want)
	}
}
