package runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/client"
)

// fakeDaemon answers the Docker API calls StopRemove makes. inspectGone
// switches GET /containers/{id}/json between "still there" and 404.
func fakeDaemon(t *testing.T, removeStatus int, inspectGone *atomic.Bool) *DockerDriver {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Api-Version", "1.47")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.Write([]byte("OK"))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/stop"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete:
			w.WriteHeader(removeStatus)
			if removeStatus >= 400 {
				w.Write([]byte(`{"message":"removal of container c1 is already in progress"}`))
			}
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/json"):
			if inspectGone.Load() {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"message":"No such container: c1"}`))
				return
			}
			w.Write([]byte(`{"Id":"c1","State":{"Status":"dead","Dead":true},"Config":{},"NetworkSettings":{}}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	t.Cleanup(ts.Close)
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+strings.TrimPrefix(ts.URL, "http://")), client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	return &DockerDriver{cli: cli}
}

func shortRemoveWait(t *testing.T, d time.Duration) {
	t.Helper()
	old := removeWait
	removeWait = d
	t.Cleanup(func() { removeWait = old })
}

// CLI-13: the wait for the container to disappear had no bound of its own, so
// with the WithoutCancel contexts saga compensations use, a container that
// never went away (here: another caller's removal "already in progress" that
// then failed) hung the caller forever.
func TestStopRemoveWaitIsBounded(t *testing.T) {
	shortRemoveWait(t, 300*time.Millisecond)
	var gone atomic.Bool
	d := fakeDaemon(t, http.StatusConflict, &gone)

	start := time.Now()
	err := d.StopRemove(context.WithoutCancel(context.Background()), "c1")
	if err == nil || !strings.Contains(err.Error(), "still exists") {
		t.Fatalf("err = %v, want a bounded-wait error", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("StopRemove took %s with a %s bound", took, removeWait)
	}
}

func TestStopRemoveReturnsOnceGone(t *testing.T) {
	shortRemoveWait(t, 10*time.Second)
	var gone atomic.Bool
	gone.Store(true)
	d := fakeDaemon(t, http.StatusNoContent, &gone)
	if err := d.StopRemove(context.Background(), "c1"); err != nil {
		t.Fatalf("StopRemove: %v", err)
	}

	// still present at first, gone a moment later: waits, then succeeds
	gone.Store(false)
	go func() { time.Sleep(250 * time.Millisecond); gone.Store(true) }()
	if err := d.StopRemove(context.Background(), "c1"); err != nil {
		t.Fatalf("StopRemove after a delayed removal: %v", err)
	}
}
