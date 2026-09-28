package diffutil

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestUnifiedIdenticalInputsAreEmpty(t *testing.T) {
	for _, s := range []string{"", "a\n", "a\nb\nc\n"} {
		if got := Unified(s, s); got != "" {
			t.Errorf("Unified(%q, %q) = %q, want empty", s, s, got)
		}
	}
}

func TestUnifiedPureInsert(t *testing.T) {
	a := "one\ntwo\nthree\n"
	b := "one\ntwo\nnew line\nthree\n"
	got := Unified(a, b)
	want := strings.Join([]string{
		"@@ -1,3 +1,4 @@",
		" one",
		" two",
		"+new line",
		" three",
		"",
	}, "\n")
	if got != want {
		t.Errorf("Unified insert:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestUnifiedPureDelete(t *testing.T) {
	a := "one\ntwo\nthree\n"
	b := "one\nthree\n"
	got := Unified(a, b)
	want := strings.Join([]string{
		"@@ -1,3 +1,2 @@",
		" one",
		"-two",
		" three",
		"",
	}, "\n")
	if got != want {
		t.Errorf("Unified delete:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestUnifiedChange(t *testing.T) {
	a := "CREATE TABLE users (\n    id integer\n);\n"
	b := "CREATE TABLE users (\n    id bigint\n);\n"
	got := Unified(a, b)
	want := strings.Join([]string{
		"@@ -1,3 +1,3 @@",
		" CREATE TABLE users (",
		"-    id integer",
		"+    id bigint",
		" );",
		"",
	}, "\n")
	if got != want {
		t.Errorf("Unified change:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestUnifiedMultipleHunks(t *testing.T) {
	// two changes separated by far more than 2*context equal lines -> two hunks
	mid := make([]string, 20)
	for i := range mid {
		mid[i] = "same"
	}
	aLines := append(append([]string{"first-old"}, mid...), "last-old")
	bLines := append(append([]string{"first-new"}, mid...), "last-new")
	got := Unified(strings.Join(aLines, "\n")+"\n", strings.Join(bLines, "\n")+"\n")

	if n := strings.Count(got, "@@ -"); n != 2 {
		t.Fatalf("hunks = %d, want 2:\n%s", n, got)
	}
	for _, want := range []string{"-first-old", "+first-new", "-last-old", "+last-new"} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("diff missing %q:\n%s", want, got)
		}
	}
	// context is limited: the 20 unchanged middle lines must not all appear
	if n := strings.Count(got, " same\n"); n > 6 {
		t.Errorf("context lines = %d, want <= 6 (3 per hunk side):\n%s", n, got)
	}
	// second hunk header points at the right region (line 19 onward)
	if !strings.Contains(got, "@@ -19,4 +19,4 @@") {
		t.Errorf("second hunk header wrong:\n%s", got)
	}
}

func TestUnifiedInsertIntoEmpty(t *testing.T) {
	got := Unified("", "a\nb\n")
	want := strings.Join([]string{
		"@@ -0,0 +1,2 @@",
		"+a",
		"+b",
		"",
	}, "\n")
	if got != want {
		t.Errorf("Unified into empty:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestUnifiedDeleteAll(t *testing.T) {
	got := Unified("a\nb\n", "")
	want := strings.Join([]string{
		"@@ -1,2 +0,0 @@",
		"-a",
		"-b",
		"",
	}, "\n")
	if got != want {
		t.Errorf("Unified delete all:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// applyOps rebuilds both sides from an edit script: ' ' and '-' lines give a,
// ' ' and '+' lines give b.
func applyOps(ops []op) (a, b []string) {
	for _, o := range ops {
		if o.kind != '+' {
			a = append(a, o.line)
		}
		if o.kind != '-' {
			b = append(b, o.line)
		}
	}
	return a, b
}

func editCount(ops []op) int {
	n := 0
	for _, o := range ops {
		if o.kind != ' ' {
			n++
		}
	}
	return n
}

// lcsLen is the textbook quadratic LCS length, used only as a reference on
// small inputs: a minimal script has len(a)+len(b)-2*LCS edits.
func lcsLen(a, b []string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				cur[j] = prev[j+1] + 1
			} else {
				cur[j] = max(prev[j], cur[j+1])
			}
		}
		prev, cur = cur, prev
	}
	return prev[0]
}

func equalLines(x, y []string) bool {
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// checkScript asserts ops is a valid edit script from a to b whose change
// runs list every deletion before any insertion.
func checkScript(t *testing.T, a, b []string, ops []op) {
	t.Helper()
	gotA, gotB := applyOps(ops)
	if !equalLines(gotA, a) || !equalLines(gotB, b) {
		t.Fatalf("edit script does not reproduce the inputs:\na=%q\nb=%q\nops=%v", a, b, ops)
	}
	for i := 1; i < len(ops); i++ {
		if ops[i-1].kind == '+' && ops[i].kind == '-' {
			t.Fatalf("insertion before deletion inside a change run at op %d: %v", i, ops)
		}
	}
}

// lcg is a tiny deterministic generator so the randomized cases are stable.
type lcg uint64

func (g *lcg) intn(n int) int {
	*g = *g*6364136223846793005 + 1442695040888963407
	return int(uint64(*g>>33) % uint64(n))
}

func randomLines(g *lcg, n, alphabet int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("l%d", g.intn(alphabet))
	}
	return out
}

// The script is valid and minimal (matches the reference LCS) on random
// inputs, including heavy line repetition like schema dumps' ");" and "".
func TestDiffOpsMinimalOnRandomInputs(t *testing.T) {
	g := lcg(1)
	for iter := 0; iter < 2000; iter++ {
		a := randomLines(&g, g.intn(40), 1+g.intn(8))
		b := randomLines(&g, g.intn(40), 1+g.intn(8))
		ops := diffOps(a, b)
		checkScript(t, a, b, ops)
		if want := len(a) + len(b) - 2*lcsLen(a, b); editCount(ops) != want {
			t.Fatalf("edits = %d, want minimal %d\na=%q\nb=%q", editCount(ops), want, a, b)
		}
	}
}

// Past the cost cap the script stays valid (just not necessarily minimal).
func TestDiffOpsCostCapStillValid(t *testing.T) {
	g := lcg(7)
	for iter := 0; iter < 2000; iter++ {
		a := randomLines(&g, g.intn(60), 1+g.intn(6))
		b := randomLines(&g, g.intn(60), 1+g.intn(6))
		for _, costCap := range []int{1, 2, 3} {
			checkScript(t, a, b, diffOpsCap(a, b, costCap))
		}
	}
}

// schemaDump synthesises a pg_dump-shaped schema of n tables: CREATE TABLEs
// first, then the constraints section, then the indexes section, so one new
// table touches three far-apart places — the case where prefix/suffix
// trimming leaves nearly the whole dump in play.
func schemaDump(n int, extra string) string {
	var sb strings.Builder
	tables := func(emit func(name string)) {
		for i := 0; i < n; i++ {
			emit(fmt.Sprintf("t%04d", i))
			if extra != "" && i == n/2 {
				emit(extra)
			}
		}
	}
	tables(func(name string) {
		fmt.Fprintf(&sb, "--\n-- Name: %s; Type: TABLE\n--\n\nCREATE TABLE public.%s (\n    id bigint NOT NULL,\n    note text,\n    created_at timestamp with time zone\n);\n\n", name, name)
	})
	tables(func(name string) {
		fmt.Fprintf(&sb, "ALTER TABLE ONLY public.%s\n    ADD CONSTRAINT %s_pkey PRIMARY KEY (id);\n\n", name, name)
	})
	tables(func(name string) {
		fmt.Fprintf(&sb, "CREATE INDEX %s_created_idx ON public.%s USING btree (created_at);\n\n", name, name)
	})
	return sb.String()
}

// A 1000-table dump (~16k lines; the old full LCS table needed ~2 GiB for it)
// with one table added in three far-apart sections diffs in bounded memory
// and yields exactly the three insertions.
func TestUnifiedLargeSchemaSpreadEditsBoundedMemory(t *testing.T) {
	a := schemaDump(1000, "")
	b := schemaDump(1000, "zz_new")
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	got := Unified(a, b)
	runtime.ReadMemStats(&after)

	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 32<<20 {
		t.Errorf("Unified allocated %d MiB, want < 32 MiB", alloc>>20)
	}
	if n := strings.Count(got, "@@ -"); n != 3 {
		t.Fatalf("hunks = %d, want 3 (table, constraint, index):\n%s", n, got)
	}
	for _, want := range []string{
		"+CREATE TABLE public.zz_new (",
		"+    ADD CONSTRAINT zz_new_pkey PRIMARY KEY (id);",
		"+CREATE INDEX zz_new_created_idx ON public.zz_new USING btree (created_at);",
	} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("diff missing %q:\n%s", want, got)
		}
	}
	for _, l := range strings.Split(got, "\n") {
		if strings.HasPrefix(l, "-") {
			t.Fatalf("pure addition produced a deletion %q:\n%s", l, got)
		}
	}
}

// 20k-line inputs: spread-out edits stay cheap and minimal, and a worst case
// (every line shared but in reverse order) is cut off by the cost cap instead
// of running quadratically; both scripts are valid.
func TestDiffOpsTwentyThousandLines(t *testing.T) {
	const n = 20000
	a := make([]string, n)
	for i := range a {
		a[i] = fmt.Sprintf("line %d", i)
	}
	b := append([]string(nil), a...)
	for _, i := range []int{10, 5000, 10000, 15000, 19990} {
		b[i] = "changed " + b[i]
	}
	b = append(b[:12000], append([]string{"inserted"}, b[12000:]...)...)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	ops := diffOps(a, b)
	runtime.ReadMemStats(&after)
	checkScript(t, a, b, ops)
	if got := editCount(ops); got != 11 {
		t.Errorf("edits = %d, want 11 (5 changed lines = 10, 1 insert)", got)
	}
	// O(N+M): a few MiB, where the old DP table was n*n*8 bytes (~3 GiB)
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 32<<20 {
		t.Errorf("diffOps allocated %d MiB, want < 32 MiB", alloc>>20)
	}

	rev := make([]string, n)
	for i := range rev {
		rev[i] = a[n-1-i]
	}
	start := time.Now()
	ops = diffOps(a, rev)
	checkScript(t, a, rev, ops)
	t.Logf("reversed 20k lines: %d edits in %s", editCount(ops), time.Since(start))
}
