package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/pgoverlay/internal/runtime"
)

// the kube driver provides the optional diagnosis capability waitReady uses
var _ containerDiagnoser = (*runtime.KubeDriver)(nil)

// diagDriver reports a canned diagnosis for every container.
type diagDriver struct {
	*fakeDriver
	reason string
	fatal  bool
	calls  int
}

func (d *diagDriver) DiagnoseContainer(ctx context.Context, id string) (string, bool) {
	d.calls++
	return d.reason, d.fatal
}

// LIFECYCLE-14: a pod that can never start (image pull back-off) fails the
// create at once with the cause in the error — instead of 90s of opaque exec
// errors — and the cause is captured before the compensation deletes the pod.
func TestWaitReadyFailsFastWithDriverDiagnosis(t *testing.T) {
	d := &diagDriver{fakeDriver: newFake(), reason: `pod pgoverlay-br-pr-1 container postgres waiting: ImagePullBackOff: Back-off pulling image "ghcr.io/acme/postgres:17"`, fatal: true}
	d.execErr = errors.New("container not found")
	e, r := testEngine(t, d)
	readySource(t, r)
	start := time.Now()
	_, err := e.CreateBranch(context.Background(), "pr-1", "main", 0)
	if err == nil || !strings.Contains(err.Error(), "ImagePullBackOff") {
		t.Fatalf("create err=%v, want the pull back-off diagnosis", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("took %s: a fatal diagnosis must short-circuit the 90s readiness wait", time.Since(start))
	}
	if len(d.containers) != 0 {
		t.Fatalf("pod not cleaned up after the failure: %v", d.containers)
	}
	h, _ := r.BranchHistory("pr-1")
	if last := h[len(h)-1]; !strings.Contains(last.Reason, "ImagePullBackOff") {
		t.Fatalf("failure reason %q lacks the diagnosis", last.Reason)
	}
}

// A non-fatal diagnosis (still pulling, unscheduled) keeps waiting, and the
// timeout error carries it.
func TestWaitReadyTimeoutCarriesDiagnosis(t *testing.T) {
	d := &diagDriver{fakeDriver: newFake(), reason: "pod p not scheduled: Unschedulable: 0/3 nodes are available"}
	d.execErr = errors.New("container not found")
	e, _ := testEngine(t, d)
	err := e.waitReady(context.Background(), "cid", 1500*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "0/3 nodes are available") || !strings.Contains(err.Error(), "container not found") {
		t.Fatalf("err=%v", err)
	}
	if d.calls < 2 {
		t.Fatalf("diagnosed %d times; a non-fatal state must keep waiting", d.calls)
	}
}
