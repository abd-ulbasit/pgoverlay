package engine

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/abd-ulbasit/pgoverlay/internal/metrics"
	"github.com/abd-ulbasit/pgoverlay/internal/registry"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// LIFECYCLE-06: retrying a failed `source add` no longer piles up same-named
// failed rows, and `source rm` leaves nothing of the name behind.
func TestAddSourceRetriesDoNotPileUpFailedRows(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	d.helperErr = errors.New("pg_basebackup: no pg_hba.conf entry")
	for i := 0; i < 3; i++ {
		s := &registry.Source{Name: "again", PGVersion: "17", ConnHost: "h", ConnPort: 5432, ConnUser: "postgres"}
		if err := e.AddSource(context.Background(), s, "pw"); err == nil {
			t.Fatal("want seed failure")
		}
	}
	list, _ := r.ListSources()
	if len(list) != 1 || list[0].State != registry.SourceFailed {
		t.Fatalf("after 3 failed adds: %+v, want one failed row", list)
	}
	d.helperErr = nil
	if err := e.AddSource(context.Background(), &registry.Source{Name: "again", PGVersion: "17", ConnHost: "h", ConnPort: 5432, ConnUser: "postgres"}, "pw"); err != nil {
		t.Fatal(err)
	}
	if list, _ = r.ListSources(); len(list) != 1 || list[0].State != registry.SourceReady {
		t.Fatalf("after a successful retry: %+v, want only the ready row", list)
	}
	if err := e.RemoveSource(context.Background(), "again"); err != nil {
		t.Fatal(err)
	}
	if list, _ = r.ListSources(); len(list) != 0 {
		t.Fatalf("after source rm: %+v, want none", list)
	}
	if err := e.RemoveSource(context.Background(), "again"); !errors.Is(err, registry.ErrNotFound) || err.Error() != `source "again" not found` {
		t.Fatalf("second rm: %v", err)
	}
}

// LIFECYCLE-15: a refresh whose seed failed and whose new-generation volume
// could not be removed is logged and counted, like AddSource's compensation.
func TestRefreshSourceCountsFailedCompensation(t *testing.T) {
	d := &flakyDriver{fakeDriver: newFake()}
	m := metrics.New()
	r, err := registry.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	e := New(r, d, "postgres:17", WithMetrics(m))
	readySource(t, r)
	d.helperErr = errors.New("pg_basebackup: boom")
	d.rmVolErrs = []error{errors.New("volume is in use")}
	if err := e.RefreshSource(context.Background(), "main", "pw"); err == nil {
		t.Fatal("want refresh to fail")
	}
	want := `
# HELP pgoverlay_compensation_failures_total Best-effort saga compensation/failure-transition errors by kind (transition|undo|cleanup).
# TYPE pgoverlay_compensation_failures_total counter
pgoverlay_compensation_failures_total{kind="undo"} 1
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(want), "pgoverlay_compensation_failures_total"); err != nil {
		t.Fatal(err)
	}
}

// LIFECYCLE-16: failure reasons persisted in the registry drop the lines in
// which Postgres echoes row values, and are capped.
func TestFailureReasonDropsRowData(t *testing.T) {
	err := errors.New("helper exited 1: psql:<stdin>:12: ERROR:  duplicate key value violates unique constraint \"users_email_key\"\n" +
		"DETAIL:  Key (email)=(alice@example.com) already exists.\n" +
		"CONTEXT:  COPY users, line 4213, column email: \"alice@example.com\"")
	got := failureReason(err)
	if strings.Contains(got, "alice@example.com") {
		t.Fatalf("reason keeps row data: %q", got)
	}
	if !strings.Contains(got, "duplicate key value violates unique constraint") {
		t.Fatalf("reason lost the error itself: %q", got)
	}
	long := failureReason(errors.New(strings.Repeat("é", 2000)))
	if len(long) > maxFailureReason+len(" …(truncated)") || !strings.HasSuffix(long, "(truncated)") || !utf8.ValidString(long) {
		t.Fatalf("long reason not capped cleanly: len=%d valid=%v", len(long), utf8.ValidString(long))
	}
}

// The same applies to a failing masking script on a branch: its DETAIL line
// can carry data that was never masked.
func TestMaskingFailureReasonOmitsRowData(t *testing.T) {
	d := newFake()
	d.psqlErr = errors.New("exec psql exited 1: ERROR:  duplicate key value violates unique constraint \"u\"\nDETAIL:  Key (ssn)=(123-45-6789) already exists.")
	e, r := testEngine(t, d)
	src := readySource(t, r)
	if err := r.SetMaskScripts(src.ID, []registry.MaskScript{{Name: "m.sql", SQL: "UPDATE users SET ssn = '0'"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err == nil {
		t.Fatal("want masking failure")
	}
	h, err := r.BranchHistory("pr-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range h {
		if strings.Contains(tr.Reason, "123-45-6789") {
			t.Fatalf("journaled reason keeps unmasked row data: %q", tr.Reason)
		}
	}
	if last := h[len(h)-1]; last.ToState != "failed" || !strings.Contains(last.Reason, "m.sql") {
		t.Fatalf("last transition %+v", last)
	}
}

// LIFECYCLE-18: name errors say which kind of name was wrong, and unknown
// names are reported once, not doubled.
func TestNameErrorsNameTheKindAndSubject(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	err := e.AddSource(context.Background(), &registry.Source{Name: "Main", PGVersion: "17"}, "pw")
	if !errors.Is(err, ErrInvalidName) || !strings.HasPrefix(err.Error(), `invalid source name "Main"`) {
		t.Fatalf("AddSource(Main): %v", err)
	}
	_, err = e.CreateBranch(context.Background(), "PR-1", "main", 0)
	if !errors.Is(err, ErrInvalidName) || !strings.HasPrefix(err.Error(), `invalid branch name "PR-1"`) {
		t.Fatalf("CreateBranch(PR-1): %v", err)
	}
	if _, err := e.CreateBranch(context.Background(), "pr-1", "nope", 0); err == nil || err.Error() != `source "nope" not found` {
		t.Fatalf("unknown source: %v", err)
	}
	readySource(t, r)
	if _, err := e.CreateBranchFrom(context.Background(), "c", "ghost", 0); err == nil || err.Error() != `parent branch "ghost" not found` || !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("unknown parent: %v", err)
	}
	if err := e.DestroyBranch(context.Background(), "nope"); err == nil || err.Error() != `branch "nope" not found` {
		t.Fatalf("destroy unknown: %v", err)
	}
}
