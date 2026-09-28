package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/docker/docker/client"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// dockerAnswering is a DockerDriver whose daemon answers every request with
// the given status and message, the way dockerd reports a refused operation.
func dockerAnswering(t *testing.T, status int, message string) *DockerDriver {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, status, map[string]string{"message": message})
	}))
	t.Cleanup(srv.Close)
	return dockerAt(t, srv.URL)
}

func dockerAt(t *testing.T, url string) *DockerDriver {
	t.Helper()
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+strings.TrimPrefix(url, "http://")), client.WithVersion("1.47"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Close() })
	return &DockerDriver{cli: cli}
}

// IsInUse and IsUnavailable classify the errors the drivers really return, so
// a failed destroy can tell the caller whether to free a resource, wait for
// the runtime, or look in the log (issue #10).
func TestRuntimeErrorClassification(t *testing.T) {
	ctx := context.Background()
	inUse := dockerAnswering(t, http.StatusConflict, "remove pgoverlay-br-d1-rw: volume is in use - [766ac31a4b2c]")
	dockerInUse := inUse.RemoveVolume(ctx, "pgoverlay-br-d1-rw")
	daemonUnavailable := dockerAnswering(t, http.StatusServiceUnavailable, "daemon is shutting down").RemoveVolume(ctx, "v")
	dockerInternal := dockerAnswering(t, http.StatusInternalServerError, "driver failed").RemoveVolume(ctx, "v")

	// a daemon nobody listens for any more
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	dockerDown := dockerAt(t, "http://"+addr).RemoveVolume(ctx, "v")

	pods := schema.GroupResource{Resource: "pods"}
	for _, tc := range []struct {
		name               string
		err                error
		inUse, unavailable bool
	}{
		{"docker volume in use (409)", fmt.Errorf("remove branch layer: %w", dockerInUse), true, false},
		{"zfs dataset busy", errors.New("destroy zfs clone: helper exited 1: cannot destroy 'tank/pgoverlay/br-d1': dataset is busy"), true, false},
		{"docker daemon unreachable", fmt.Errorf("remove container: %w", dockerDown), false, true},
		{"docker daemon 503", daemonUnavailable, false, true},
		{"kube API server unavailable", apierrors.NewServiceUnavailable("apiserver is restarting"), false, true},
		{"kube API server timeout", apierrors.NewServerTimeout(pods, "delete", 5), false, true},
		{"teardown ran out of time", fmt.Errorf("waiting for volume x to be deleted: %w", context.DeadlineExceeded), false, true},
		{"docker internal error", dockerInternal, false, false},
		{"kube forbidden", apierrors.NewForbidden(pods, "br-d1", errors.New("rbac")), false, false},
		{"plain error", errors.New("boom"), false, false},
		{"nil", nil, false, false},
	} {
		if got := IsInUse(tc.err); got != tc.inUse {
			t.Errorf("%s: IsInUse(%v) = %v, want %v", tc.name, tc.err, got, tc.inUse)
		}
		if got := IsUnavailable(tc.err); got != tc.unavailable {
			t.Errorf("%s: IsUnavailable(%v) = %v, want %v", tc.name, tc.err, got, tc.unavailable)
		}
	}
	if dockerInUse == nil || !strings.Contains(dockerInUse.Error(), "volume is in use") {
		t.Fatalf("fake daemon's 409 = %v, want the daemon's message", dockerInUse)
	}
}
