package ha

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// client-go starts OnStartedLeading with `go` and may cancel the term and call
// OnStoppedLeading before that goroutine runs: the late start must not reopen
// the gate, or a non-leader would accept writes.
func TestLateOnStartedLeadingDoesNotReopenGate(t *testing.T) {
	gate := &fakeGate{}
	var ran bool
	c := NewCallbacks(gate, func(ctx context.Context) { ran = true })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()             // the term already ended...
	c.OnStoppedLeading() // ...and the elector already reported it
	c.OnStartedLeading(ctx)
	if gate.IsLeader() {
		t.Fatal("a late OnStartedLeading reopened the gate after the term ended")
	}
	if ran {
		t.Fatal("reconcile ran for a term that had already ended")
	}
}

// recordingMarker records Mark/Unmark calls in order.
type recordingMarker struct {
	mu    sync.Mutex
	calls []string
	marks chan struct{}
}

func (m *recordingMarker) Mark(ctx context.Context) error {
	m.mu.Lock()
	m.calls = append(m.calls, "mark")
	m.mu.Unlock()
	select {
	case m.marks <- struct{}{}:
	default:
	}
	return nil
}

func (m *recordingMarker) Unmark(ctx context.Context) error {
	m.mu.Lock()
	m.calls = append(m.calls, "unmark")
	m.mu.Unlock()
	return nil
}

func (m *recordingMarker) snapshot() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls...)
}

// The leader marks itself as soon as it leads, re-marks during the term, and
// unmarks once the term ends, strictly after its last mark.
func TestMarkerFollowsLeadership(t *testing.T) {
	gate := &fakeGate{}
	m := &recordingMarker{marks: make(chan struct{}, 16)}
	c := NewCallbacks(gate, func(ctx context.Context) { <-ctx.Done() }, WithMarker(m))
	c.markEvery = 5 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.OnStartedLeading(ctx)
		close(done)
	}()
	for i := 0; i < 3; i++ { // first mark is immediate, then periodic
		select {
		case <-m.marks:
		case <-time.After(2 * time.Second):
			t.Fatalf("mark %d never happened", i+1)
		}
	}
	cancel()
	c.OnStoppedLeading()
	<-done

	calls := m.snapshot()
	if len(calls) < 4 || calls[len(calls)-1] != "unmark" {
		t.Fatalf("calls = %v, want marks then a final unmark", calls)
	}
	for _, call := range calls[:len(calls)-1] {
		if call != "mark" {
			t.Fatalf("calls = %v: unmark before the last mark", calls)
		}
	}
	if gate.IsLeader() {
		t.Fatal("gate still open after the term ended")
	}
}

func pod(name string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: labels}}
}

func podLabels(t *testing.T, p *PodLabeler, name string) map[string]string {
	t.Helper()
	got, err := p.Client.CoreV1().Pods("ns").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return got.Labels
}

// PodLabeler puts the leader label on its own pod and strips it from any
// other pod (a previous leader that could not clear its own), leaving other
// labels alone; Unmark removes it again.
func TestPodLabeler(t *testing.T) {
	app := map[string]string{"app.kubernetes.io/name": "pgoverlay"}
	stale := map[string]string{"app.kubernetes.io/name": "pgoverlay", LeaderLabel: "true"}
	cs := fake.NewClientset(pod("branchd-a", stale), pod("branchd-b", app), pod("unrelated", map[string]string{"x": "y"}))
	p := &PodLabeler{Client: cs, Namespace: "ns", Pod: "branchd-b"}
	ctx := context.Background()

	if err := p.Mark(ctx); err != nil {
		t.Fatal(err)
	}
	if got := podLabels(t, p, "branchd-b"); got[LeaderLabel] != "true" || got["app.kubernetes.io/name"] != "pgoverlay" {
		t.Fatalf("leader pod labels = %v", got)
	}
	if got := podLabels(t, p, "branchd-a"); got[LeaderLabel] != "" || got["app.kubernetes.io/name"] != "pgoverlay" {
		t.Fatalf("stale leader pod labels = %v, want the leader label removed and the rest kept", got)
	}
	if got := podLabels(t, p, "unrelated"); got["x"] != "y" || len(got) != 1 {
		t.Fatalf("unrelated pod labels = %v", got)
	}
	if err := p.Mark(ctx); err != nil { // idempotent
		t.Fatal(err)
	}

	if err := p.Unmark(ctx); err != nil {
		t.Fatal(err)
	}
	if got := podLabels(t, p, "branchd-b"); got[LeaderLabel] != "" {
		t.Fatalf("labels after Unmark = %v", got)
	}
	gone := &PodLabeler{Client: cs, Namespace: "ns", Pod: "deleted"}
	if err := gone.Unmark(ctx); err != nil {
		t.Fatalf("Unmark of a deleted pod = %v, want nil", err)
	}
	if err := gone.Mark(ctx); err == nil {
		t.Fatal("Mark of a missing pod should fail")
	}
}

// fakeLock is a resourcelock.Interface returning a canned record or error.
type fakeLock struct {
	rec *resourcelock.LeaderElectionRecord
	err error
}

func (l *fakeLock) Get(context.Context) (*resourcelock.LeaderElectionRecord, []byte, error) {
	return l.rec, nil, l.err
}
func (l *fakeLock) Create(context.Context, resourcelock.LeaderElectionRecord) error { return nil }
func (l *fakeLock) Update(context.Context, resourcelock.LeaderElectionRecord) error { return nil }
func (l *fakeLock) RecordEvent(string)                                              {}
func (l *fakeLock) Identity() string                                                { return "me" }
func (l *fakeLock) Describe() string                                                { return "fake" }

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A leaderless set (no live holder, or an unreadable Lease) is logged, since
// from the outside every replica looks healthy while refusing all writes; a
// live holder is quiet.
func TestWatchLeaderlessWarns(t *testing.T) {
	cases := []struct {
		name string
		lock *fakeLock
		want string // "" = no warning
	}{
		{"stale holder", &fakeLock{rec: &resourcelock.LeaderElectionRecord{
			HolderIdentity: "old", RenewTime: metav1.NewTime(time.Now().Add(-time.Hour)),
		}}, "no replica holds a live leader Lease"},
		{"released", &fakeLock{rec: &resourcelock.LeaderElectionRecord{
			RenewTime: metav1.NewTime(time.Now()),
		}}, "no replica holds a live leader Lease"},
		{"unreadable", &fakeLock{err: errors.New("forbidden")}, "cannot read the leader Lease"},
		{"live holder", &fakeLock{rec: &resourcelock.LeaderElectionRecord{
			HolderIdentity: "leader", RenewTime: metav1.NewTime(time.Now()),
		}}, ""},
	}
	for _, tc := range cases {
		var logs syncBuffer
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			watchLeaderless(ctx, tc.lock, 5*time.Millisecond, time.Minute, slog.New(slog.NewTextHandler(&logs, nil)))
		}()
		time.Sleep(50 * time.Millisecond) // several ticks
		cancel()
		<-done
		got := logs.String()
		if tc.want == "" && got != "" {
			t.Errorf("%s: unexpected warnings:\n%s", tc.name, got)
		}
		if tc.want != "" && !strings.Contains(got, tc.want) {
			t.Errorf("%s: no %q warning; logs:\n%s", tc.name, tc.want, got)
		}
	}
}
