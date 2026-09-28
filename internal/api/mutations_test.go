package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/pgoverlay/internal/registry"
)

// On a follower, authentication and the role check still run first: a caller
// without a valid token gets 401 and an under-privileged one 403, so
// leadership is never revealed to them; only an authorized caller sees 503.
func TestMutationAuthCheckedBeforeLeaderGate(t *testing.T) {
	ts, srv := newTestServerWithLeader(t)
	addSource(t, ts)
	viewer := mintToken(t, ts, "ro", registry.RoleViewer)
	srv.LeaderGate().Set(false)

	req := CreateBranchRequest{Name: "x", Source: "main"}
	if code, body := do(t, ts, "", "POST", "/v1/branches", req); code != http.StatusUnauthorized {
		t.Errorf("no token on a follower = %d (%s), want 401", code, body)
	}
	if code, body := do(t, ts, "not-a-token", "POST", "/v1/branches", req); code != http.StatusUnauthorized {
		t.Errorf("bad token on a follower = %d (%s), want 401", code, body)
	}
	if code, body := do(t, ts, viewer, "POST", "/v1/branches", req); code != http.StatusForbidden {
		t.Errorf("viewer on a follower = %d (%s), want 403", code, body)
	}
	if code, body := do(t, ts, testToken, "POST", "/v1/branches", req); code != http.StatusServiceUnavailable {
		t.Errorf("admin on a follower = %d (%s), want 503", code, body)
	}
}

// asyncResult is the outcome of a request sent from a helper goroutine.
type asyncResult struct {
	code int
	body string
	err  error
}

// doAsync sends an authenticated JSON request from a goroutine (no t.Fatal
// off the test goroutine) and delivers the outcome on the returned channel.
func doAsync(ctx context.Context, ts *httptest.Server, method, path string, body any) <-chan asyncResult {
	out := make(chan asyncResult, 1)
	go func() {
		b, _ := json.Marshal(body)
		req, err := http.NewRequestWithContext(ctx, method, ts.URL+path, bytes.NewReader(b))
		if err != nil {
			out <- asyncResult{err: err}
			return
		}
		req.Header.Set("Authorization", "Bearer "+testToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			out <- asyncResult{err: err}
			return
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		out <- asyncResult{code: resp.StatusCode, body: string(data)}
	}()
	return out
}

// parkStarts makes every StartBranch on d block until the returned channel is
// closed or the saga's context ends.
func parkStarts(d *fakeDriver) (release chan struct{}) {
	d.startBlock = make(chan struct{})
	d.startEntered = make(chan struct{}, 4)
	d.startErr = make(chan error, 4)
	return d.startBlock
}

func recvWithin[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

// A client that disconnects mid-create must not abort the saga: the mutation
// runs on a context detached from the connection and finishes on its own.
func TestMutationSurvivesClientDisconnect(t *testing.T) {
	ts, _, d := newTestServerParts(t)
	addSource(t, ts)
	release := parkStarts(d)

	ctx, cancel := context.WithCancel(context.Background())
	res := doAsync(ctx, ts, "POST", "/v1/branches", CreateBranchRequest{Name: "pr-1", Source: "main"})
	recvWithin(t, d.startEntered, "the create saga to reach StartBranch")
	cancel() // the client gives up (timeout, dropped port-forward, LB idle timeout)
	if r := recvWithin(t, res, "the client call to return"); r.err == nil {
		t.Fatalf("client call should have failed after its context was cancelled, got %d", r.code)
	}
	// Give the server time to notice the closed connection; a request-scoped
	// saga would see its context cancelled here.
	select {
	case err := <-d.startErr:
		t.Fatalf("saga context was cancelled by the client disconnect: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)

	deadline := time.Now().Add(5 * time.Second)
	for {
		code, body := do(t, ts, testToken, "GET", "/v1/branches/pr-1", nil)
		if code == http.StatusOK && mustUnmarshal[Branch](t, body).State == "ready" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("branch never became ready after the client left: %d %s", code, body)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Losing leadership cancels the sagas the deposed leader still has in flight
// (they compensate and the client gets a retryable 503), instead of letting
// them keep writing next to the new leader.
func TestDemotionCancelsInflightMutation(t *testing.T) {
	ts, srv, d := newTestServerParts(t)
	addSource(t, ts)
	parkStarts(d)

	res := doAsync(context.Background(), ts, "POST", "/v1/branches", CreateBranchRequest{Name: "pr-1", Source: "main"})
	recvWithin(t, d.startEntered, "the create saga to reach StartBranch")
	srv.LeaderGate().Set(false)

	if err := recvWithin(t, d.startErr, "the saga to observe cancellation"); !errors.Is(err, context.Canceled) {
		t.Fatalf("saga context error = %v, want context.Canceled", err)
	}
	r := recvWithin(t, res, "the create call to return")
	if r.err != nil || r.code != http.StatusServiceUnavailable || !strings.Contains(r.body, "leadership") {
		t.Fatalf("create after demotion = %d %q (err %v), want 503 naming the lost leadership", r.code, r.body, r.err)
	}
	// the saga compensated: the row is failed, not left creating
	code, body := do(t, ts, testToken, "GET", "/v1/branches/pr-1", nil)
	if code != http.StatusOK || mustUnmarshal[Branch](t, body).State != "failed" {
		t.Fatalf("branch after cancelled create = %d %s, want state failed", code, body)
	}
}

// Shutdown drain: StopAdmitting refuses new mutations (reads keep working),
// WaitMutations blocks until in-flight sagas finish.
func TestStopAdmittingDrainsInflight(t *testing.T) {
	ts, srv, d := newTestServerParts(t)
	addSource(t, ts)
	release := parkStarts(d)

	res := doAsync(context.Background(), ts, "POST", "/v1/branches", CreateBranchRequest{Name: "pr-1", Source: "main"})
	recvWithin(t, d.startEntered, "the create saga to reach StartBranch")
	srv.StopAdmitting()

	code, body := do(t, ts, testToken, "POST", "/v1/branches", CreateBranchRequest{Name: "pr-2", Source: "main"})
	if code != http.StatusServiceUnavailable || !strings.Contains(string(body), "shutting down") {
		t.Errorf("new mutation while draining = %d %s, want 503 shutting down", code, body)
	}
	if code, _ := do(t, ts, testToken, "GET", "/v1/branches", nil); code != http.StatusOK {
		t.Errorf("read while draining = %d, want 200", code)
	}

	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := srv.WaitMutations(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitMutations with a parked saga = %v, want deadline exceeded", err)
	}
	close(release)
	long, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if err := srv.WaitMutations(long); err != nil {
		t.Fatalf("WaitMutations after release = %v", err)
	}
	if r := recvWithin(t, res, "the create call"); r.code != http.StatusCreated {
		t.Fatalf("in-flight create during drain = %d %s (err %v), want 201", r.code, r.body, r.err)
	}
}

// CancelMutations (drain budget exhausted) cancels in-flight sagas so they
// compensate and return; the client gets a retryable 503.
func TestCancelMutationsEndsInflight(t *testing.T) {
	ts, srv, d := newTestServerParts(t)
	addSource(t, ts)
	parkStarts(d)

	res := doAsync(context.Background(), ts, "POST", "/v1/branches", CreateBranchRequest{Name: "pr-1", Source: "main"})
	recvWithin(t, d.startEntered, "the create saga to reach StartBranch")
	srv.StopAdmitting()
	srv.CancelMutations()

	if err := recvWithin(t, d.startErr, "the saga to observe cancellation"); !errors.Is(err, context.Canceled) {
		t.Fatalf("saga context error = %v, want context.Canceled", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.WaitMutations(ctx); err != nil {
		t.Fatalf("WaitMutations after cancel = %v", err)
	}
	if r := recvWithin(t, res, "the create call"); r.code != http.StatusServiceUnavailable || !strings.Contains(r.body, "shutting down") {
		t.Fatalf("cancelled create = %d %q (err %v), want 503 shutting down", r.code, r.body, r.err)
	}
}

// Branch sagas are bounded by the stuck timeout: past it the saga is cancelled
// (and compensates) instead of racing the reconcile loop for the row.
func TestBranchSagaBoundedByStuckTimeout(t *testing.T) {
	ts, _, d := newTestServerCfg(t, 100*time.Millisecond)
	addSource(t, ts)
	parkStarts(d)

	res := doAsync(context.Background(), ts, "POST", "/v1/branches", CreateBranchRequest{Name: "pr-1", Source: "main"})
	recvWithin(t, d.startEntered, "the create saga to reach StartBranch")
	if err := recvWithin(t, d.startErr, "the saga to hit its deadline"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("saga context error = %v, want deadline exceeded", err)
	}
	if r := recvWithin(t, res, "the create call"); r.code != http.StatusGatewayTimeout {
		t.Fatalf("timed-out create = %d %q (err %v), want 504", r.code, r.body, r.err)
	}
}

// The gate reports every leadership change to its observers (the leader
// gauge), plus the current value on registration.
func TestLeaderGateObserve(t *testing.T) {
	g := newLeaderGate()
	var seen []bool
	g.Observe(func(v bool) { seen = append(seen, v) })
	g.Set(false)
	g.Set(false) // unchanged: no notification
	g.Set(true)
	want := []bool{true, false, true}
	if len(seen) != len(want) {
		t.Fatalf("observed %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("observed %v, want %v", seen, want)
		}
	}
}
