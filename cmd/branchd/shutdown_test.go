package main

import (
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/abd-ulbasit/pgoverlay/internal/api"
)

// serveBlocking starts newAPIHTTPServer with a handler that parks every
// request until release is closed, and returns the server and its address.
func serveBlocking(t *testing.T, entered chan<- struct{}, release <-chan struct{}) (*http.Server, string) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := newAPIHTTPServer(lis.Addr().String(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	go srv.Serve(lis)
	return srv, "http://" + lis.Addr().String()
}

// A request that finishes inside the budget is waited for and completes.
func TestDrainAPIWaitsForInflightRequests(t *testing.T) {
	entered, release := make(chan struct{}, 1), make(chan struct{})
	srv, url := serveBlocking(t, entered, release)
	apiSrv := api.New(nil, nil, "t", nil, nil, 0)

	codes := make(chan int, 1)
	go func() {
		resp, err := http.Post(url+"/x", "application/json", nil)
		if err != nil {
			codes <- 0
			return
		}
		resp.Body.Close()
		codes <- resp.StatusCode
	}()
	<-entered
	done := make(chan struct{})
	go func() {
		drainAPI(srv, apiSrv, 5*time.Second)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("drainAPI returned while a request was still in flight")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("drainAPI did not return after the request finished")
	}
	if code := <-codes; code != http.StatusNoContent {
		t.Fatalf("in-flight request finished with %d, want 204", code)
	}
}

// Past the budget, drainAPI stops waiting (it cancels the mutations and
// closes the remaining connections) instead of hanging shutdown forever.
func TestDrainAPIGivesUpAfterBudget(t *testing.T) {
	entered, release := make(chan struct{}, 1), make(chan struct{})
	defer close(release)
	srv, url := serveBlocking(t, entered, release)
	apiSrv := api.New(nil, nil, "t", nil, nil, 0)

	go func() {
		if resp, err := http.Post(url+"/x", "application/json", nil); err == nil {
			resp.Body.Close()
		}
	}()
	<-entered
	start := time.Now()
	drainAPI(srv, apiSrv, 100*time.Millisecond)
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("drainAPI took %s with a 100ms budget and no mutations in flight", took)
	}
	if _, err := http.Get(url + "/x"); err == nil {
		t.Fatal("server still accepts connections after drainAPI")
	}
}

func TestNewAPIHTTPServerHasConnectionLimits(t *testing.T) {
	srv := newAPIHTTPServer(":0", http.NotFoundHandler())
	if srv.ReadHeaderTimeout <= 0 || srv.ReadTimeout <= 0 || srv.IdleTimeout <= 0 {
		t.Fatalf("missing timeouts: header=%s read=%s idle=%s", srv.ReadHeaderTimeout, srv.ReadTimeout, srv.IdleTimeout)
	}
	if srv.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout = %s; long diff/seed responses need none", srv.WriteTimeout)
	}
}

func TestStorageRoot(t *testing.T) {
	cases := []struct {
		name                                       string
		runtime, storage, dataRoot, home, override string
		want                                       string
	}{
		{"docker uses home", "docker", "hostpath", "/var/lib/pgoverlay", "/home/u/.pgoverlay", "", "/home/u/.pgoverlay"},
		{"chart layout measures the mounted state dir", "kube", "hostpath", "/var/lib/pgoverlay", "/var/lib/pgoverlay/state", "", "/var/lib/pgoverlay/state"},
		{"trailing slashes", "kube", "hostpath", "/var/lib/pgoverlay/", "/var/lib/pgoverlay/state/", "", "/var/lib/pgoverlay/state/"},
		{"on the storage node itself", "kube", "hostpath", "/var/lib/pgoverlay", "/root/.pgoverlay", "", "/var/lib/pgoverlay"},
		{"sibling prefix is not inside", "kube", "hostpath", "/var/lib/pgoverlay", "/var/lib/pgoverlay-state", "", "/var/lib/pgoverlay"},
		{"csi has no shared root", "kube", "csi", "/var/lib/pgoverlay", "/var/lib/pgoverlay/state", "", ""},
		{"override wins", "kube", "csi", "/var/lib/pgoverlay", "/var/lib/pgoverlay/state", "/data", "/data"},
	}
	for _, tc := range cases {
		if got := storageRoot(tc.runtime, tc.storage, tc.dataRoot, tc.home, tc.override); got != tc.want {
			t.Errorf("%s: storageRoot = %q, want %q", tc.name, got, tc.want)
		}
	}
}
