package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/abd-ulbasit/pgbranch/internal/diffutil"
	"github.com/abd-ulbasit/pgbranch/internal/registry"
	"github.com/abd-ulbasit/pgbranch/internal/runtime"
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
		if strings.HasPrefix(s.Name, "pgbranch-br-diff-") {
			return s
		}
	}
	t.Fatalf("no throwaway diff branch started: %v", d.branches)
	return runtime.BranchSpec{}
}

// diffFake returns ExecOutput behavior serving schema dumps and row
// estimates: the throwaway base clone (id embeds pgbranch-br-diff-) sees the
// original schema, the target branch an extended one.
func diffFake() func(id string, cmd []string) (string, error) {
	return func(id string, cmd []string) (string, error) {
		isBase := strings.Contains(id, "pgbranch-br-diff-")
		if len(cmd) > 0 && cmd[0] == "pg_dump" {
			if isBase {
				return "CREATE TABLE users (\n    id integer\n);\n", nil
			}
			return "CREATE TABLE users (\n    id integer\n);\nCREATE TABLE diffdemo (\n    x integer\n);\n", nil
		}
		if isBase {
			return "fresh|-1\nusers|100\n", nil
		}
		return "diffdemo|42\nfresh|-1\nusers|90\n", nil
	}
}

func TestDiffBranchHappyPath(t *testing.T) {
	d := newFake()
	d.execOutFn = diffFake()
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
	// other side, reltuples -1 (never analyzed) clamps to 0
	want := []TableDelta{
		{Table: "diffdemo", BaseRows: 0, BranchRows: 42, Delta: 42},
		{Table: "fresh", BaseRows: 0, BranchRows: 0, Delta: 0},
		{Table: "users", BaseRows: 100, BranchRows: 90, Delta: -10},
	}
	if fmt.Sprintf("%v", res.Tables) != fmt.Sprintf("%v", want) {
		t.Errorf("Tables = %+v, want %+v", res.Tables, want)
	}

	// dump + estimate commands ran in-container over the local socket
	var dumpCmds, psqlCmds int
	for _, c := range d.execOuts {
		switch c[0] {
		case "pg_dump":
			dumpCmds++
			wantCmd := "pg_dump -U postgres -h /var/run/postgresql --schema-only --no-owner --no-acl postgres"
			if got := strings.Join(c, " "); got != wantCmd {
				t.Errorf("pg_dump cmd = %q, want %q", got, wantCmd)
			}
		case "psql":
			psqlCmds++
			if got := strings.Join(c, " "); !strings.Contains(got, "-tA") || !strings.Contains(got, "reltuples") {
				t.Errorf("row estimate cmd = %q", got)
			}
		}
	}
	if dumpCmds != 2 || psqlCmds != 2 {
		t.Errorf("dump cmds = %d, psql cmds = %d, want 2 each", dumpCmds, psqlCmds)
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

func TestDiffBranchClonesTargetOwnBaseGeneration(t *testing.T) {
	d := newFake()
	d.execOutFn = diffFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	d.volumes["pgbranch-src-main"] = true
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
	if len(spec.Mounts) == 0 || spec.Mounts[0].Volume != "pgbranch-src-main" {
		t.Errorf("throwaway lower0 = %+v, want the target's own gen-1 volume pgbranch-src-main", spec.Mounts)
	}
	for _, m := range spec.Mounts {
		if m.Volume == "pgbranch-src-main-g2" {
			t.Errorf("throwaway mounted the CURRENT generation volume: %+v", spec.Mounts)
		}
	}
}

func TestDiffBranchClonesTargetLayerChain(t *testing.T) {
	d := newFake()
	d.execOutFn = diffFake()
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
	if len(vols) != 3 || vols[0] != "pgbranch-src-main" || vols[1] != "pgbranch-br-p-rw" {
		t.Errorf("throwaway stack = %v, want [pgbranch-src-main pgbranch-br-p-rw <its own rw>]", vols)
	}
	if names := liveDiffBranches(t, r); len(names) != 0 {
		t.Errorf("throwaway rows left: %v", names)
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

// TestDiffBranchNoDataSampleByDefault: without WithDataSample, no table
// carries SampleRows and no PK/jsonb queries are issued.
func TestDiffBranchNoDataSampleByDefault(t *testing.T) {
	d := newFake()
	d.execOutFn = diffFake()
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
		if joined := strings.Join(c, " "); strings.Contains(joined, "to_jsonb") || strings.Contains(joined, "indisprimary") {
			t.Errorf("unexpected sampling query without WithDataSample: %q", joined)
		}
	}
}

// dataSampleFake serves schema dumps, row estimates, PK lookups and jsonb row
// samples. diffdemo grows (base 0 -> branch rows) and has PK x; nopk also
// grows but has no PK (skipped); users shrinks (never sampled).
func dataSampleFake() func(id string, cmd []string) (string, error) {
	return func(id string, cmd []string) (string, error) {
		isBase := strings.Contains(id, "pgbranch-br-diff-")
		joined := strings.Join(cmd, " ")
		switch {
		case len(cmd) > 0 && cmd[0] == "pg_dump":
			return "CREATE TABLE diffdemo (x int);\n", nil
		case strings.Contains(joined, "reltuples"):
			if isBase {
				return "diffdemo|0\nnopk|0\nusers|100\n", nil
			}
			return "diffdemo|3\nnopk|5\nusers|90\n", nil
		case strings.Contains(joined, "indisprimary"):
			// PK columns by table; nopk returns nothing
			switch {
			case strings.Contains(joined, "'diffdemo'"):
				return "x\n", nil
			case strings.Contains(joined, "'nopk'"):
				return "", nil
			}
			return "", nil
		case strings.Contains(joined, "to_jsonb"):
			if isBase {
				// base has only x=1
				return `{"x": 1}` + "\n", nil
			}
			// branch has x=1,2,3 — x=2,3 are branch-only
			return `{"x": 1}` + "\n" + `{"x": 2}` + "\n" + `{"x": 3}` + "\n", nil
		}
		return "", nil
	}
}

func TestDiffBranchDataSampleBranchOnlyByPK(t *testing.T) {
	d := newFake()
	d.execOutFn = dataSampleFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}

	res, err := e.DiffBranch(context.Background(), "pr-1", WithDataSample(20))
	if err != nil {
		t.Fatal(err)
	}

	byName := map[string]TableDelta{}
	for _, td := range res.Tables {
		byName[td.Table] = td
	}

	// diffdemo grew: branch-only rows x=2,3 (x=1 exists on both)
	demo := byName["diffdemo"]
	if len(demo.SampleRows) != 2 {
		t.Fatalf("diffdemo SampleRows = %v, want 2 branch-only rows", demo.SampleRows)
	}
	gotX := []float64{}
	for _, row := range demo.SampleRows {
		x, ok := row["x"].(float64)
		if !ok {
			t.Fatalf("diffdemo row missing numeric x: %v", row)
		}
		gotX = append(gotX, x)
	}
	if !(gotX[0] == 2 && gotX[1] == 3) {
		t.Errorf("diffdemo branch-only x = %v, want [2 3]", gotX)
	}

	// nopk grew but has no primary key: skipped (nil samples)
	if byName["nopk"].SampleRows != nil {
		t.Errorf("nopk has SampleRows despite no PK: %v", byName["nopk"].SampleRows)
	}

	// users shrank: not a grown table, never sampled
	if byName["users"].SampleRows != nil {
		t.Errorf("users (shrank) has SampleRows: %v", byName["users"].SampleRows)
	}

	// the sample SELECT ordered by the PK and capped with LIMIT
	var sawJSONB bool
	for _, c := range d.execOuts {
		j := strings.Join(c, " ")
		if strings.Contains(j, "to_jsonb") {
			sawJSONB = true
			if !strings.Contains(j, `ORDER BY "x"`) || !strings.Contains(j, "LIMIT 20") {
				t.Errorf("jsonb sample query not ordered/capped: %q", j)
			}
		}
	}
	if !sawJSONB {
		t.Error("no to_jsonb sample query issued")
	}
}

// WithDataSample(0) falls back to the default cap rather than disabling.
func TestWithDataSampleDefaultCap(t *testing.T) {
	d := newFake()
	d.execOutFn = dataSampleFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DiffBranch(context.Background(), "pr-1", WithDataSample(0)); err != nil {
		t.Fatal(err)
	}
	for _, c := range d.execOuts {
		j := strings.Join(c, " ")
		if strings.Contains(j, "to_jsonb") && !strings.Contains(j, fmt.Sprintf("LIMIT %d", defaultSampleRows)) {
			t.Errorf("WithDataSample(0) did not use default cap: %q", j)
		}
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
