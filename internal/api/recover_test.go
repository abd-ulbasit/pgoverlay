package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/abd-ulbasit/pgoverlay/internal/registry"
)

// POST /v1/branches/{name}/recover restarts a failed branch on its existing
// data; a branch that is not failed is a 409, like other state conflicts.
func TestRecoverBranchEndpoint(t *testing.T) {
	ts, srv := newTestServerWithLeader(t)
	addSource(t, ts)
	if code, body := do(t, ts, testToken, "POST", "/v1/branches", CreateBranchRequest{Name: "pr-1", Source: "main"}); code != http.StatusCreated {
		t.Fatalf("create: code=%d body=%s", code, body)
	}

	code, body := do(t, ts, testToken, "POST", "/v1/branches/pr-1/recover", nil)
	if code != http.StatusConflict || !strings.Contains(string(body), "only a failed branch") {
		t.Fatalf("recover ready branch: code=%d body=%s, want 409", code, body)
	}

	// fail it the way crash recovery does (its volume stays)
	b, err := srv.reg.GetBranchByName("pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.reg.TransitionBranch(b.ID, registry.BranchResetting, "freeze for child c"); err != nil {
		t.Fatal(err)
	}
	if err := srv.reg.TransitionBranch(b.ID, registry.BranchFailed, "reconcile: stuck resetting"); err != nil {
		t.Fatal(err)
	}
	code, body = do(t, ts, testToken, "POST", "/v1/branches/pr-1/recover", nil)
	if code != http.StatusOK {
		t.Fatalf("recover failed branch: code=%d body=%s", code, body)
	}
	got := mustUnmarshal[Branch](t, body)
	if got.State != "ready" || got.Port == 0 {
		t.Fatalf("recovered branch %+v", got)
	}

	if code, _ := do(t, ts, testToken, "POST", "/v1/branches/nope/recover", nil); code != http.StatusNotFound {
		t.Fatalf("recover unknown branch: code=%d want 404", code)
	}
}
