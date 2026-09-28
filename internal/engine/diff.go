package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/abd-ulbasit/pgoverlay/internal/diffutil"
	"github.com/abd-ulbasit/pgoverlay/internal/registry"
)

// UnknownRows is the BaseRows/BranchRows value of a side whose row count is
// unknown (see TableDelta).
const UnknownRows int64 = -1

// TableDelta is one table's row-count comparison between a branch and its
// base. Counts are planner estimates (pg_class.reltuples), not exact counts,
// with one exception: a table the planner has no estimate for on either side
// (never analyzed or vacuumed, reltuples -1) is counted exactly on both sides
// where its heap is small (at most 64 MiB). A side that is still unknown
// after that reports UnknownRows, RowsUnknown is set and Delta is 0 — never a
// made-up 0 row count. A table present on one side only counts 0 on the other.
type TableDelta struct {
	// Schema is the table's schema and Table its name within it: same-named
	// tables in different schemas are separate entries.
	Schema     string `json:"schema"`
	Table      string `json:"table"`
	BaseRows   int64  `json:"base_rows"`
	BranchRows int64  `json:"branch_rows"`
	Delta      int64  `json:"delta"`
	// RowsUnknown is set when either side's count is unknown (UnknownRows);
	// Delta is then 0 and carries no information.
	RowsUnknown bool `json:"rows_unknown,omitempty"`
	// SampleRows is a bounded set of branch-only rows (present on the branch,
	// absent on the base, matched by primary key) — populated only when the
	// diff is requested with data sampling (engine.WithDataSample) and only for
	// tables that grew (see Grew). Tables with no primary key are skipped
	// (sampling needs a stable key to diff by).
	SampleRows []map[string]any `json:"sample_rows,omitempty"`
}

// Name is the table's display name: bare in the public schema, otherwise
// schema-qualified.
func (t TableDelta) Name() string {
	if t.Schema == "" || t.Schema == "public" {
		return t.Table
	}
	return t.Schema + "." + t.Table
}

// Grew reports whether the branch holds more rows than the base, as far as
// the counts can tell (unknown counts never classify as grown). Data sampling
// covers exactly these tables.
func (t TableDelta) Grew() bool {
	return !t.RowsUnknown && t.BranchRows > t.BaseRows
}

// Cells renders the counts for display: "?" for an unknown side or delta,
// "0" for no change, a signed delta otherwise.
func (t TableDelta) Cells() (base, branch, delta string) {
	count := func(n int64) string {
		if n < 0 {
			return "?"
		}
		return strconv.FormatInt(n, 10)
	}
	switch {
	case t.RowsUnknown:
		delta = "?"
	case t.Delta == 0:
		delta = "0"
	default:
		delta = fmt.Sprintf("%+d", t.Delta)
	}
	return count(t.BaseRows), count(t.BranchRows), delta
}

// DiffResult is what changed in a branch relative to its base: a unified
// schema diff (pg_dump --schema-only of base vs branch; empty = identical)
// and per-table row-count deltas.
type DiffResult struct {
	SchemaDiff string       `json:"schema_diff"`
	Tables     []TableDelta `json:"tables"`
}

// tableStatsSQL lists every user table with its planner row estimate
// (reltuples, -1 = never analyzed) and heap size, as one JSON array so no
// identifier can break the parse. Other sessions' temp tables are skipped.
const tableStatsSQL = `SELECT coalesce(json_agg(json_build_object('schema', n.nspname, 'table', c.relname, 'rows', c.reltuples::bigint, 'bytes', pg_relation_size(c.oid))), '[]') FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE c.relkind='r' AND c.relpersistence<>'t' AND n.nspname NOT IN ('pg_catalog','information_schema')`

// exactCountMaxBytes bounds the exact-count fallback for tables without a
// planner estimate: only heaps up to this size are scanned with count(*), so
// a diff never reads a large table end to end.
const exactCountMaxBytes = 64 << 20

// countChunk is how many tables one exact-count statement covers, keeping
// the psql argument far below exec argument-size limits.
const countChunk = 100

// diffOptions holds the optional tuning for DiffBranch.
type diffOptions struct {
	// sample is the per-table cap on branch-only sample rows; 0 disables
	// data sampling entirely.
	sample int
}

// DiffOption tunes DiffBranch.
type DiffOption func(*diffOptions)

// defaultSampleRows is the per-table sample cap used when WithDataSample is
// requested with a non-positive n.
const defaultSampleRows = 20

// maxSampleRows caps the per-table sample: a larger request is clamped, so a
// caller cannot make the engine pull whole tables into memory.
const maxSampleRows = 1000

// WithDataSample turns on bounded data sampling: for each table that grew
// (TableDelta.Grew), DiffBranch returns up to n branch-only rows (matched by
// primary key) in TableDelta.SampleRows. A non-positive n uses the default
// cap (20); n is clamped to 1000. Tables without a primary key are skipped.
// Off by default.
func WithDataSample(n int) DiffOption {
	return func(o *diffOptions) {
		if n <= 0 {
			n = defaultSampleRows
		}
		o.sample = min(n, maxSampleRows)
	}
}

// DiffBranch reports what changed in a ready branch relative to its base —
// the state a reset would return it to. It provisions an internal throwaway
// branch ("diff-<6 hex>") from the target's OWN base — the recorded source
// volume/generation and frozen-layer chain, not the source's current
// generation — then runs pg_dump --schema-only and a row-estimate query
// inside both instances over the local socket (no credentials involved, so
// rotated branch passwords don't matter) and diffs host-side. The throwaway
// is a normal registry row (TTL'd, so the reaper cleans strays if branchd dies
// mid-diff) and is destroyed before returning, success or not. Expect a few
// seconds of wall time: a full branch provision plus two dumps.
func (e *Engine) DiffBranch(ctx context.Context, name string, opts ...DiffOption) (_ *DiffResult, err error) {
	defer e.observeOp("diff", &err)()
	var o diffOptions
	for _, opt := range opts {
		opt(&o)
	}
	b, err := e.reg.GetBranchByName(name)
	if err != nil {
		return nil, err
	}
	if b.State != registry.BranchReady {
		return nil, fmt.Errorf("branch %q is %s, not ready", name, b.State)
	}
	src, err := e.reg.GetSourceByID(b.SourceID)
	if err != nil {
		return nil, err
	}
	suffix := make([]byte, 3)
	if _, err := rand.Read(suffix); err != nil {
		return nil, fmt.Errorf("diff %q: %w", name, err)
	}
	twName := "diff-" + hex.EncodeToString(suffix)
	// The throwaway copies the target's base coordinates: SourceVolume pins
	// the source generation (zfs/csi children: the parent's dataset/PVC) and
	// BaseLayerID the frozen overlay chain — exactly what reset re-provisions
	// onto.
	tw := &registry.Branch{
		Name:         twName,
		SourceID:     b.SourceID,
		RWVolume:     e.planner.BranchLayerName(twName),
		SourceVolume: b.SourceVolume,
		BaseLayerID:  b.BaseLayerID,
		ExpiresAt:    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}
	if err := e.reg.CreateBranchCtx(ctx, tw); err != nil {
		return nil, fmt.Errorf("diff %q: %w", name, err)
	}
	// Best-effort teardown on every path; a failure leaves a TTL'd row the
	// reaper retries within the hour.
	defer func() {
		e.logCompensationErr("cleanup", "diff: destroy throwaway base branch",
			e.DestroyBranch(context.WithoutCancel(ctx), tw.Name), "branch", tw.Name, "branch_id", tw.ID)
	}()
	if err := e.provision(ctx, tw, src); err != nil {
		e.logCompensationErr("transition", "diff: mark throwaway base branch failed after provision failed",
			e.reg.TransitionBranchCtx(ctx, tw.ID, registry.BranchFailed, err.Error()), "branch", tw.Name, "branch_id", tw.ID)
		return nil, fmt.Errorf("diff %q: provision base clone: %w", name, err)
	}
	twRow, err := e.reg.GetBranchByName(tw.Name)
	if err != nil {
		return nil, err
	}

	baseDump, err := e.drv.ExecOutput(ctx, twRow.ContainerID, pgDumpSchemaCmd(src))
	if err != nil {
		return nil, fmt.Errorf("diff %q: dump base schema: %w", name, err)
	}
	branchDump, err := e.drv.ExecOutput(ctx, b.ContainerID, pgDumpSchemaCmd(src))
	if err != nil {
		return nil, fmt.Errorf("diff %q: dump branch schema: %w", name, err)
	}
	baseStats, err := e.tableStats(ctx, twRow.ContainerID, src)
	if err != nil {
		return nil, fmt.Errorf("diff %q: base row estimates: %w", name, err)
	}
	branchStats, err := e.tableStats(ctx, b.ContainerID, src)
	if err != nil {
		return nil, fmt.Errorf("diff %q: branch row estimates: %w", name, err)
	}
	e.countUnestimated(ctx, twRow.ContainerID, b.ContainerID, src, baseStats, branchStats)

	res := &DiffResult{
		SchemaDiff: diffutil.Unified(stripDumpNoise(baseDump), stripDumpNoise(branchDump)),
		Tables:     tableDeltas(baseStats, branchStats),
	}
	if o.sample > 0 {
		if err := e.sampleNewRows(ctx, res, b.ContainerID, twRow.ContainerID, src, baseStats, o.sample); err != nil {
			return nil, fmt.Errorf("diff %q: sample rows: %w", name, err)
		}
	}
	return res, nil
}

// tableKey identifies a user table the same way on both instances.
type tableKey struct{ schema, table string }

// sql is the key as a schema-qualified, quoted SQL name.
func (k tableKey) sql() string { return quoteIdent(k.schema) + "." + quoteIdent(k.table) }

// tableStat is one table's row count on one instance.
type tableStat struct {
	rows  int64 // planner estimate, exact count, or UnknownRows
	bytes int64 // heap size; bounds the exact-count fallback
}

// tableStats runs tableStatsSQL inside the instance. Negative reltuples
// (never analyzed) come back as UnknownRows, not 0.
func (e *Engine) tableStats(ctx context.Context, cid string, src *registry.Source) (map[tableKey]tableStat, error) {
	out, err := e.psqlOutput(ctx, cid, src, tableStatsSQL)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Schema string `json:"schema"`
		Table  string `json:"table"`
		Rows   int64  `json:"rows"`
		Bytes  *int64 `json:"bytes"` // NULL if the table vanished mid-query
	}
	if out = strings.TrimSpace(out); out != "" {
		if err := json.Unmarshal([]byte(out), &rows); err != nil {
			return nil, fmt.Errorf("unparseable table stats %q: %w", truncate(out, 200), err)
		}
	}
	stats := make(map[tableKey]tableStat, len(rows))
	for _, r := range rows {
		s := tableStat{rows: r.Rows}
		if s.rows < 0 {
			s.rows = UnknownRows
		}
		if r.Bytes != nil {
			s.bytes = *r.Bytes
		}
		stats[tableKey{r.Schema, r.Table}] = s
	}
	return stats, nil
}

// countUnestimated replaces missing planner estimates with exact counts. A
// table with no estimate on EITHER side is counted on both sides (so its delta
// compares like with like) wherever its heap is at most exactCountMaxBytes.
// Whatever cannot be counted keeps its estimate, or stays UnknownRows. A
// failed count only loses precision, so it is logged, not returned.
func (e *Engine) countUnestimated(ctx context.Context, baseCID, branchCID string, src *registry.Source, base, branch map[tableKey]tableStat) {
	need := map[tableKey]bool{}
	for _, stats := range []map[tableKey]tableStat{base, branch} {
		for k, s := range stats {
			if s.rows < 0 {
				need[k] = true
			}
		}
	}
	if len(need) == 0 {
		return
	}
	keys := make([]tableKey, 0, len(need))
	for k := range need {
		keys = append(keys, k)
	}
	sortKeys(keys)
	e.countExact(ctx, baseCID, src, base, keys)
	e.countExact(ctx, branchCID, src, branch, keys)
}

// countExact counts the rows of every listed table present in stats whose
// heap is small enough, in statements of up to countChunk tables each, and
// records the counts in stats.
func (e *Engine) countExact(ctx context.Context, cid string, src *registry.Source, stats map[tableKey]tableStat, keys []tableKey) {
	var todo []tableKey
	for _, k := range keys {
		if s, ok := stats[k]; ok && s.bytes <= exactCountMaxBytes {
			todo = append(todo, k)
		}
	}
	for len(todo) > 0 {
		chunk := todo[:min(countChunk, len(todo))]
		todo = todo[len(chunk):]
		subs := make([]string, len(chunk))
		for i, k := range chunk {
			subs[i] = "(SELECT count(*) FROM " + k.sql() + ")"
		}
		out, err := e.psqlOutput(ctx, cid, src, "SELECT to_json(ARRAY["+strings.Join(subs, ", ")+"])")
		var counts []int64
		if err == nil {
			if err = json.Unmarshal([]byte(strings.TrimSpace(out)), &counts); err == nil && len(counts) != len(chunk) {
				err = fmt.Errorf("got %d counts for %d tables", len(counts), len(chunk))
			}
		}
		if err != nil {
			slog.Warn("diff: exact row count failed; keeping planner estimates", "container", cid, "tables", len(chunk), "err", err)
			continue
		}
		for i, k := range chunk {
			s := stats[k]
			s.rows = counts[i]
			stats[k] = s
		}
	}
}

// sampleScanKeys is how many of a grown table's highest primary keys are
// checked against the base when looking for branch-only rows.
func sampleScanKeys(capN int) int { return max(10*capN, 1000) }

// keyChunkBytes bounds the JSON key list passed in one presence check.
const keyChunkBytes = 32 << 10

// sampleNewRows fills TableDelta.SampleRows for every table that grew. Per
// table it reads the branch's primary key, takes the highest
// sampleScanKeys(capN) keys on the branch (serial, identity and time-ordered
// keys put new rows there), asks the base which of those keys it has, and
// returns up to capN branch rows whose key the base lacks — every row, when
// the table does not exist on the base at all. Base and branch are separate
// instances, so the comparison happens host-side, key by key. A table whose
// sampling fails (e.g. its key columns differ on the base) is logged and
// left without samples rather than failing the diff.
func (e *Engine) sampleNewRows(ctx context.Context, res *DiffResult, branchCID, baseCID string, src *registry.Source, base map[tableKey]tableStat, capN int) error {
	for i := range res.Tables {
		td := &res.Tables[i]
		if !td.Grew() {
			continue
		}
		k := tableKey{td.Schema, td.Table}
		_, onBase := base[k]
		rows, err := e.sampleTable(ctx, branchCID, baseCID, src, k, onBase, capN)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Warn("diff: skipping data sample for table", "table", k.sql(), "err", err)
			continue
		}
		td.SampleRows = rows
	}
	return nil
}

// pkColumn is one primary-key column: its name and SQL type (format_type).
type pkColumn struct{ name, typ string }

// sampleTable returns up to capN rows of table k present on the branch but
// not on the base (nil when there are none or the table has no primary key),
// ordered by primary key.
func (e *Engine) sampleTable(ctx context.Context, branchCID, baseCID string, src *registry.Source, k tableKey, onBase bool, capN int) ([]map[string]any, error) {
	pk, err := e.primaryKey(ctx, branchCID, src, k)
	if err != nil || len(pk) == 0 {
		return nil, err // no PK: nothing stable to diff by
	}
	keys, err := e.highestKeys(ctx, branchCID, src, k, pk, sampleScanKeys(capN))
	if err != nil {
		return nil, err
	}
	var fresh []string
	if !onBase {
		fresh = keys[:min(capN, len(keys))]
	} else {
		for start := 0; start < len(keys) && len(fresh) < capN; {
			end, size := start, 0
			for end < len(keys) && (end == start || size+len(keys[end]) <= keyChunkBytes) {
				size += len(keys[end]) + 1
				end++
			}
			present, err := e.keysPresent(ctx, baseCID, src, k, pk, keys[start:end])
			if err != nil {
				return nil, err
			}
			for i, key := range keys[start:end] {
				if !present[i] && len(fresh) < capN {
					fresh = append(fresh, key)
				}
			}
			start = end
		}
	}
	if len(fresh) == 0 {
		return nil, nil
	}
	return e.rowsByKey(ctx, branchCID, src, k, pk, fresh)
}

// primaryKeySQL lists a table's primary-key columns in key order with their
// SQL types, as one JSON array of [name, type] pairs.
const primaryKeySQL = `SELECT coalesce(json_agg(json_build_array(a.attname, format_type(a.atttypid, a.atttypmod)) ORDER BY array_position(i.indkey, a.attnum)), '[]') FROM pg_index i JOIN pg_class c ON c.oid=i.indrelid JOIN pg_namespace n ON n.oid=c.relnamespace JOIN pg_attribute a ON a.attrelid=c.oid AND a.attnum=ANY(i.indkey) WHERE i.indisprimary AND n.nspname=%s AND c.relname=%s`

// primaryKey returns the table's primary-key columns (empty when it has
// none).
func (e *Engine) primaryKey(ctx context.Context, cid string, src *registry.Source, k tableKey) ([]pkColumn, error) {
	out, err := e.psqlOutput(ctx, cid, src, fmt.Sprintf(primaryKeySQL, sqlLiteral(k.schema), sqlLiteral(k.table)))
	if err != nil {
		return nil, err
	}
	var pairs [][2]string
	if out = strings.TrimSpace(out); out != "" {
		if err := json.Unmarshal([]byte(out), &pairs); err != nil {
			return nil, fmt.Errorf("unparseable primary key %q: %w", truncate(out, 200), err)
		}
	}
	cols := make([]pkColumn, len(pairs))
	for i, p := range pairs {
		cols[i] = pkColumn{name: p[0], typ: p[1]}
	}
	return cols, nil
}

// highestKeys returns up to n primary keys of the table, highest first, each
// as a one-line JSON object {column: value}.
func (e *Engine) highestKeys(ctx context.Context, cid string, src *registry.Source, k tableKey, pk []pkColumn, n int) ([]string, error) {
	fields := make([]string, len(pk))
	order := make([]string, len(pk))
	for i, c := range pk {
		fields[i] = sqlLiteral(c.name) + ", t." + quoteIdent(c.name)
		order[i] = "t." + quoteIdent(c.name) + " DESC"
	}
	sql := fmt.Sprintf("SELECT jsonb_build_object(%s) FROM %s AS t ORDER BY %s LIMIT %d",
		strings.Join(fields, ", "), k.sql(), strings.Join(order, ", "), n)
	out, err := e.psqlOutput(ctx, cid, src, sql)
	if err != nil {
		return nil, err
	}
	return nonEmptyLines(out), nil
}

// keyRecordSQL is a FROM clause expanding a JSON array of key objects into
// typed rows p(<pk columns>) — jsonb_to_record reads back what jsonb wrote for
// every type — numbered e.i (from 1) when ordinality is set. Passing keys as
// JSON keeps the values exact and the SQL free of per-type literal quoting.
func keyRecordSQL(keysJSON string, pk []pkColumn, ordinality bool) string {
	cols := make([]string, len(pk))
	for i, c := range pk {
		cols[i] = quoteIdent(c.name) + " " + c.typ
	}
	elems := fmt.Sprintf("jsonb_array_elements(%s::jsonb) AS e(k)", sqlLiteral(keysJSON))
	if ordinality {
		elems = fmt.Sprintf("jsonb_array_elements(%s::jsonb) WITH ORDINALITY AS e(k, i)", sqlLiteral(keysJSON))
	}
	return elems + ", LATERAL jsonb_to_record(e.k) AS p(" + strings.Join(cols, ", ") + ")"
}

// pkMatch is the condition joining key rows p to table rows t by primary key.
func pkMatch(pk []pkColumn) string {
	conds := make([]string, len(pk))
	for i, c := range pk {
		conds[i] = "t." + quoteIdent(c.name) + " = p." + quoteIdent(c.name)
	}
	return strings.Join(conds, " AND ")
}

// keysPresent reports, per key, whether the table on this instance has a row
// with that primary key.
func (e *Engine) keysPresent(ctx context.Context, cid string, src *registry.Source, k tableKey, pk []pkColumn, keys []string) ([]bool, error) {
	sql := fmt.Sprintf("SELECT e.i FROM %s WHERE EXISTS (SELECT 1 FROM %s AS t WHERE %s)",
		keyRecordSQL("["+strings.Join(keys, ",")+"]", pk, true), k.sql(), pkMatch(pk))
	out, err := e.psqlOutput(ctx, cid, src, sql)
	if err != nil {
		return nil, err
	}
	present := make([]bool, len(keys))
	for _, line := range nonEmptyLines(out) {
		i, err := strconv.Atoi(line)
		if err != nil || i < 1 || i > len(keys) {
			return nil, fmt.Errorf("unexpected key ordinal %q", line)
		}
		present[i-1] = true
	}
	return present, nil
}

// rowsByKey fetches the full rows for the given keys, ordered by primary key.
func (e *Engine) rowsByKey(ctx context.Context, cid string, src *registry.Source, k tableKey, pk []pkColumn, keys []string) ([]map[string]any, error) {
	order := make([]string, len(pk))
	for i, c := range pk {
		order[i] = "t." + quoteIdent(c.name)
	}
	// t.* (not bare t, which a column named "t" would shadow) is the whole row
	sql := fmt.Sprintf("SELECT to_jsonb(t.*) FROM %s, %s AS t WHERE %s ORDER BY %s",
		keyRecordSQL("["+strings.Join(keys, ",")+"]", pk, false), k.sql(), pkMatch(pk), strings.Join(order, ", "))
	out, err := e.psqlOutput(ctx, cid, src, sql)
	if err != nil {
		return nil, err
	}
	var rows []map[string]any
	for _, line := range nonEmptyLines(out) {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			return nil, fmt.Errorf("decode sample row %q: %w", truncate(line, 200), err)
		}
		rows = append(rows, m)
	}
	return rows, nil
}

// psqlOutput runs a single SQL statement in the instance over the local socket
// in unaligned tuples-only mode and returns the raw output.
func (e *Engine) psqlOutput(ctx context.Context, cid string, src *registry.Source, sql string) (string, error) {
	user, db := src.ConnUser, src.ConnDB
	if user == "" {
		user = "postgres"
	}
	if db == "" {
		db = "postgres"
	}
	cmd := []string{"psql", "-tA", "-v", "ON_ERROR_STOP=1", "-U", user, "-d", db, "-h", "/var/run/postgresql", "-c", sql}
	return e.drv.ExecOutput(ctx, cid, cmd)
}

// sqlLiteral quotes s as an escape-string literal (E'...'), which reads the
// same whatever standard_conforming_strings is set to.
func sqlLiteral(s string) string {
	return "E'" + strings.NewReplacer(`\`, `\\`, `'`, `''`).Replace(s) + "'"
}

// quoteIdent wraps s as a SQL identifier (double quotes doubled).
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// stripDumpNoise removes pg_dump lines that differ between two dumps of the
// same schema for reasons unrelated to structure. pg_dump 16+ brackets its
// output with `\restrict <token>` / `\unrestrict <token>` session-lock
// directives whose token is randomised per run, so they would otherwise show
// as a spurious diff hunk every time.
func stripDumpNoise(dump string) string {
	lines := strings.Split(dump, "\n")
	out := lines[:0]
	for _, ln := range lines {
		if strings.HasPrefix(ln, `\restrict `) || strings.HasPrefix(ln, `\unrestrict `) {
			continue
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}

// pgDumpSchemaCmd builds the in-container schema-only dump over the local
// socket: owners and ACLs are stripped so the diff shows structure, not
// grants noise.
func pgDumpSchemaCmd(src *registry.Source) []string {
	user, db := src.ConnUser, src.ConnDB
	if user == "" {
		user = "postgres"
	}
	if db == "" {
		db = "postgres"
	}
	return []string{"pg_dump", "-U", user, "-h", "/var/run/postgresql", "--schema-only", "--no-owner", "--no-acl", db}
}

func sortKeys(keys []tableKey) {
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].schema != keys[j].schema {
			return keys[i].schema < keys[j].schema
		}
		return keys[i].table < keys[j].table
	})
}

// tableDeltas joins both sides into a union sorted by schema and name. A
// table present on one side only counts 0 on the other; an unknown side
// keeps UnknownRows and marks the delta unknown.
func tableDeltas(base, branch map[tableKey]tableStat) []TableDelta {
	names := map[tableKey]bool{}
	for k := range base {
		names[k] = true
	}
	for k := range branch {
		names[k] = true
	}
	keys := make([]tableKey, 0, len(names))
	for k := range names {
		keys = append(keys, k)
	}
	sortKeys(keys)
	out := make([]TableDelta, 0, len(keys))
	for _, k := range keys {
		// absent = 0 rows (the zero tableStat)
		b, r := base[k].rows, branch[k].rows
		td := TableDelta{Schema: k.schema, Table: k.table, BaseRows: b, BranchRows: r}
		if b < 0 || r < 0 {
			td.RowsUnknown = true
		} else {
			td.Delta = r - b
		}
		out = append(out, td)
	}
	return out
}
