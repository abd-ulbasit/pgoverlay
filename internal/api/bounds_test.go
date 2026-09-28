package api

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/abd-ulbasit/pgoverlay/internal/engine"
)

// ttl_seconds above what a time.Duration can hold used to wrap negative and be
// read as "never expires", bypassing --max-ttl. It is now a 400; the largest
// representable value is still accepted and capped by --max-ttl.
func TestCreateBranchTTLOverflowRejected(t *testing.T) {
	ts, _ := newTestServer(t, engine.WithTTLPolicy(0, 168*time.Hour))
	addSource(t, ts)
	for _, ttl := range []int{int(maxTTLSeconds) + 1, 10_000_000_000, 20_000_000_000} {
		body := fmt.Sprintf(`{"name":"pr-1","source":"main","ttl_seconds":%d}`, ttl)
		if code, resp := doRaw(t, ts, "POST", "/v1/branches", body); code != http.StatusBadRequest {
			t.Errorf("ttl_seconds=%d = %d %s, want 400", ttl, code, resp)
		}
	}
	code, resp := doRaw(t, ts, "POST", "/v1/branches", fmt.Sprintf(`{"name":"pr-1","source":"main","ttl_seconds":%d}`, maxTTLSeconds))
	if code != http.StatusCreated {
		t.Fatalf("ttl_seconds=max = %d %s, want 201", code, resp)
	}
	b := mustUnmarshal[Branch](t, []byte(resp))
	exp, err := time.Parse(time.RFC3339, b.ExpiresAt)
	if err != nil {
		t.Fatalf("expires_at %q: %v", b.ExpiresAt, err)
	}
	if limit := time.Now().Add(168*time.Hour + time.Minute); exp.After(limit) {
		t.Fatalf("expires_at %s is past the --max-ttl cap", b.ExpiresAt)
	}
}

// ?data=N is bounded: a viewer-sized request cannot make branchd buffer whole
// tables as JSON.
func TestDiffDataSampleBounded(t *testing.T) {
	ts, _ := newTestServer(t)
	addSource(t, ts)
	if code, body := do(t, ts, testToken, "POST", "/v1/branches", CreateBranchRequest{Name: "pr-1", Source: "main"}); code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	for _, n := range []int{engine.MaxSampleRows + 1, 2_000_000_000} {
		if code, body := do(t, ts, testToken, "GET", fmt.Sprintf("/v1/branches/pr-1/diff?data=%d", n), nil); code != http.StatusBadRequest {
			t.Errorf("diff?data=%d = %d %s, want 400", n, code, body)
		}
	}
	if code, body := do(t, ts, testToken, "GET", fmt.Sprintf("/v1/branches/pr-1/diff?data=%d", engine.MaxSampleRows), nil); code != http.StatusOK {
		t.Errorf("diff?data=%d = %d %s, want 200", engine.MaxSampleRows, code, body)
	}
}
