package api

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abd-ulbasit/pgoverlay/internal/cow"
	"github.com/abd-ulbasit/pgoverlay/internal/engine"
	"github.com/abd-ulbasit/pgoverlay/internal/registry"
)

// newCSITestServer builds the API over a csi-backend engine with a ready
// source "main" and a branch pr-2 created from branch pr-1.
func newCSITestServer(t *testing.T) (*httptest.Server, *fakeDriver) {
	t.Helper()
	d := newFake()
	reg, err := registry.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	src := &registry.Source{Name: "main", PGVersion: "17", Volume: "pgoverlay-src-main"}
	if err := reg.CreateSource(src); err != nil {
		t.Fatal(err)
	}
	if err := reg.SetSourceState(src.ID, registry.SourceReady, "test"); err != nil {
		t.Fatal(err)
	}
	eng := engine.NewWithPlanner(reg, d, "postgres:17", cow.Planner{Backend: cow.BackendCSI})
	ts := httptest.NewServer(New(eng, reg, testToken, nil, nil, 0).Handler())
	t.Cleanup(ts.Close)
	for _, req := range []CreateBranchRequest{{Name: "pr-1", Source: "main"}, {Name: "pr-2", Parent: "pr-1"}} {
		if code, body := do(t, ts, testToken, "POST", "/v1/branches", req); code != http.StatusCreated {
			t.Fatalf("create %s: code=%d body=%s", req.Name, code, body)
		}
	}
	return ts, d
}

// checkpoints counts the CHECKPOINTs run so far (the first step of stopping
// a csi parent for a clone).
func checkpoints(d *fakeDriver) int {
	n := 0
	for _, c := range d.execs {
		if strings.Contains(strings.Join(c, " "), "CHECKPOINT") {
			n++
		}
	}
	return n
}

// Issue #35: on the csi backend, diffing a branch created from another branch
// stops that parent around the base clone, like a reset. A viewer must not be
// able to cause that: 403 before anything is touched. An operator (the role a
// reset needs) may, and the parent comes back ready.
func TestBranchDiffCSIChildNeedsOperator(t *testing.T) {
	ts, d := newCSITestServer(t)
	viewer := mintToken(t, ts, "ro", registry.RoleViewer)
	operator := mintToken(t, ts, "ci", registry.RoleOperator)
	parent := func() Branch {
		t.Helper()
		code, body := do(t, ts, testToken, "GET", "/v1/branches/pr-1", nil)
		if code != http.StatusOK {
			t.Fatalf("get pr-1: code=%d body=%s", code, body)
		}
		return mustUnmarshal[Branch](t, body)
	}
	ckpts, starts := checkpoints(d), d.starts

	code, body := do(t, ts, viewer, "GET", "/v1/branches/pr-2/diff", nil)
	if code != http.StatusForbidden || !strings.Contains(string(body), "lacks the required operator privilege") {
		t.Fatalf("viewer diff of a csi child: code=%d body=%s, want 403 naming the operator role", code, body)
	}
	if checkpoints(d) != ckpts || d.starts != starts {
		t.Fatalf("a refused diff touched the runtime: checkpoints %d -> %d, starts %d -> %d", ckpts, checkpoints(d), starts, d.starts)
	}
	if p := parent(); p.State != string(registry.BranchReady) {
		t.Fatalf("parent after a refused diff: %+v", p)
	}

	if code, body := do(t, ts, operator, "GET", "/v1/branches/pr-2/diff", nil); code != http.StatusOK {
		t.Fatalf("operator diff of a csi child: code=%d body=%s, want 200", code, body)
	}
	if checkpoints(d) != ckpts+1 {
		t.Errorf("operator diff ran %d CHECKPOINTs, want 1 (the parent is quiesced for the clone)", checkpoints(d)-ckpts)
	}
	if p := parent(); p.State != string(registry.BranchReady) {
		t.Fatalf("parent after the operator's diff: %+v", p)
	}
}
