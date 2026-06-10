package engine

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/pgbranch/internal/registry"
	"github.com/abd-ulbasit/pgbranch/internal/runtime"
)

type fakeDriver struct {
	volumes    map[string]bool
	containers map[string]bool
	failStart  bool
	execErr    error
	helperErr  error // returned by RunHelper (fails seeding)
	starts     int   // StartBranch invocations
}

func newFake() *fakeDriver {
	return &fakeDriver{volumes: map[string]bool{}, containers: map[string]bool{}}
}
func (f *fakeDriver) EnsureImage(ctx context.Context, image string) error { return nil }
func (f *fakeDriver) CreateVolume(ctx context.Context, name string, l map[string]string) error {
	f.volumes[name] = true
	return nil
}
func (f *fakeDriver) RemoveVolume(ctx context.Context, name string) error {
	delete(f.volumes, name)
	return nil
}
func (f *fakeDriver) RunHelper(ctx context.Context, s runtime.HelperSpec) error { return f.helperErr }
func (f *fakeDriver) StartBranch(ctx context.Context, s runtime.BranchSpec) (string, error) {
	if f.failStart {
		return "", errors.New("boom")
	}
	f.starts++
	f.containers["cid-"+s.Name] = true
	return "cid-" + s.Name, nil
}
func (f *fakeDriver) Exec(ctx context.Context, id string, cmd []string) error { return f.execErr }
func (f *fakeDriver) Inspect(ctx context.Context, id string) (runtime.ContainerInfo, error) {
	return runtime.ContainerInfo{ID: id, Running: f.containers[id], Port: 54321}, nil
}
func (f *fakeDriver) StopRemove(ctx context.Context, id string) error {
	delete(f.containers, id)
	return nil
}
func (f *fakeDriver) ListManaged(ctx context.Context) ([]runtime.ContainerInfo, error) {
	var out []runtime.ContainerInfo
	for id := range f.containers {
		out = append(out, runtime.ContainerInfo{ID: id, Running: true})
	}
	return out, nil
}

func testEngine(t *testing.T, d runtime.Driver) (*Engine, *registry.Registry) {
	t.Helper()
	r, err := registry.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return New(r, d, "postgres:17"), r
}

func readySource(t *testing.T, r *registry.Registry) *registry.Source {
	t.Helper()
	s := &registry.Source{Name: "main", PGVersion: "17", Volume: "pgbranch-src-main"}
	if err := r.CreateSource(s); err != nil {
		t.Fatal(err)
	}
	if err := r.SetSourceState(s.ID, registry.SourceReady, "test"); err != nil {
		t.Fatal(err)
	}
	return s
}

func mustSource(t *testing.T, r *registry.Registry) *registry.Source {
	t.Helper()
	s, err := r.GetSourceByName("main")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestReconcileCleansOrphans(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	// registry says creating, but no container exists -> failed + cleaned
	b := &registry.Branch{Name: "stuck", SourceID: mustSource(t, r).ID, RWVolume: "pgbranch-br-stuck-rw"}
	if err := r.CreateBranch(b); err != nil {
		t.Fatal(err)
	}
	d.volumes["pgbranch-br-stuck-rw"] = true
	// container exists but registry has no row -> removed
	d.containers["cid-ghost"] = true

	if err := e.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := r.GetBranchByName("stuck")
	if got.State != registry.BranchFailed {
		t.Fatalf("state=%q", got.State)
	}
	if d.containers["cid-ghost"] {
		t.Fatal("ghost container not removed")
	}
}

func TestCreateBranchHappyPath(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)

	b, err := e.CreateBranch(context.Background(), "pr-1", "main", 0)
	if err != nil {
		t.Fatal(err)
	}
	if b.State != registry.BranchReady || b.Port != 54321 {
		t.Fatalf("branch %+v", b)
	}
	if !d.volumes["pgbranch-br-pr-1-rw"] {
		t.Fatal("rw volume not created")
	}
	if !d.containers["cid-pgbranch-br-pr-1"] {
		t.Fatal("container not started")
	}
}

func TestCreateBranchUnwindsOnStartFailure(t *testing.T) {
	d := newFake()
	d.failStart = true
	e, r := testEngine(t, d)
	readySource(t, r)

	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err == nil {
		t.Fatal("want error")
	}
	if len(d.volumes) != 0 {
		t.Fatalf("rw volume leaked: %v", d.volumes)
	}
	b, err := r.GetBranchByName("pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if b.State != registry.BranchFailed {
		t.Fatalf("state=%q want failed", b.State)
	}
}

func TestDestroyBranch(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}
	if err := e.DestroyBranch(context.Background(), "pr-1"); err != nil {
		t.Fatal(err)
	}
	if len(d.containers) != 0 || len(d.volumes) != 0 {
		t.Fatalf("leaked: c=%v v=%v", d.containers, d.volumes)
	}
	if _, err := r.GetBranchByName("pr-1"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("want gone, got %v", err)
	}
}

func TestCreateBranchRecordsTTLAndSourceVolume(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)

	b, err := e.CreateBranch(context.Background(), "pr-1", "main", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if b.SourceVolume != "pgbranch-src-main" {
		t.Fatalf("SourceVolume=%q", b.SourceVolume)
	}
	want := time.Now().Add(24 * time.Hour).UTC()
	got, err := time.Parse(time.RFC3339, b.ExpiresAt)
	if err != nil {
		t.Fatalf("ExpiresAt=%q: %v", b.ExpiresAt, err)
	}
	if diff := got.Sub(want); diff < -time.Minute || diff > time.Minute {
		t.Fatalf("ExpiresAt=%s want ~%s", got, want)
	}
	// ttl 0 = never expires
	b2, err := e.CreateBranch(context.Background(), "pr-2", "main", 0)
	if err != nil {
		t.Fatal(err)
	}
	if b2.ExpiresAt != "" {
		t.Fatalf("ttl 0: ExpiresAt=%q want empty", b2.ExpiresAt)
	}
}

func TestResetBranchHappyPath(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}

	b, err := e.ResetBranch(context.Background(), "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if b.State != registry.BranchReady {
		t.Fatalf("state=%q", b.State)
	}
	if d.starts != 2 {
		t.Fatalf("starts=%d want 2 (container recreated)", d.starts)
	}
	if !d.volumes["pgbranch-br-pr-1-rw"] {
		t.Fatal("rw volume missing after reset")
	}
	if !d.containers["cid-pgbranch-br-pr-1"] {
		t.Fatal("container missing after reset")
	}
	// journal: ready -> resetting -> ready, same row
	if _, err := r.GetBranchByName("pr-1"); err != nil {
		t.Fatal(err)
	}
}

func TestResetBranchFailsToFailedAndUnwinds(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}
	d.failStart = true
	if _, err := e.ResetBranch(context.Background(), "pr-1"); err == nil {
		t.Fatal("want error")
	}
	b, err := r.GetBranchByName("pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if b.State != registry.BranchFailed {
		t.Fatalf("state=%q want failed", b.State)
	}
	if len(d.volumes) != 0 || len(d.containers) != 0 {
		t.Fatalf("leaked: c=%v v=%v", d.containers, d.volumes)
	}
}

func TestResetBranchRequiresReady(t *testing.T) {
	d := newFake()
	d.failStart = true
	e, r := testEngine(t, d)
	readySource(t, r)
	e.CreateBranch(context.Background(), "pr-1", "main", 0) // fails -> failed state
	if _, err := e.ResetBranch(context.Background(), "pr-1"); err == nil {
		t.Fatal("want error resetting a failed branch")
	}
	b, _ := r.GetBranchByName("pr-1")
	if b.State != registry.BranchFailed {
		t.Fatalf("state=%q", b.State)
	}
}

func TestRefreshSourceBumpsGenerationAndGCsOldVolume(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	d.volumes["pgbranch-src-main"] = true // gen-1 volume exists

	// no live branches -> old volume GC'd immediately
	if err := e.RefreshSource(context.Background(), "main", "secret"); err != nil {
		t.Fatal(err)
	}
	s := mustSource(t, r)
	if s.Generation != 2 || s.Volume != "pgbranch-src-main-g2" {
		t.Fatalf("source after refresh: %+v", s)
	}
	if !d.volumes["pgbranch-src-main-g2"] {
		t.Fatal("new generation volume not created")
	}
	if d.volumes["pgbranch-src-main"] {
		t.Fatal("unreferenced old volume not GC'd")
	}
}

func TestRefreshSourceKeepsOldVolumeWhileReferenced(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	d.volumes["pgbranch-src-main"] = true
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}

	if err := e.RefreshSource(context.Background(), "main", "secret"); err != nil {
		t.Fatal(err)
	}
	if !d.volumes["pgbranch-src-main"] {
		t.Fatal("old volume removed while pr-1 still references it")
	}
	// new branches use the new generation volume
	b2, err := e.CreateBranch(context.Background(), "pr-2", "main", 0)
	if err != nil {
		t.Fatal(err)
	}
	if b2.SourceVolume != "pgbranch-src-main-g2" {
		t.Fatalf("pr-2 SourceVolume=%q", b2.SourceVolume)
	}
	// destroying the last referencing branch GCs the orphaned old volume
	if err := e.DestroyBranch(context.Background(), "pr-1"); err != nil {
		t.Fatal(err)
	}
	if d.volumes["pgbranch-src-main"] {
		t.Fatal("orphaned old-generation volume not GC'd on branch destroy")
	}
	if !d.volumes["pgbranch-src-main-g2"] {
		t.Fatal("current generation volume must survive")
	}
}

func TestRefreshSourceSeedFailureKeepsCurrentGeneration(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	d.volumes["pgbranch-src-main"] = true
	d.helperErr = errors.New("pg_basebackup: boom")

	if err := e.RefreshSource(context.Background(), "main", "secret"); err == nil {
		t.Fatal("want error")
	}
	s := mustSource(t, r)
	if s.Generation != 1 || s.Volume != "pgbranch-src-main" || s.State != registry.SourceReady {
		t.Fatalf("source mutated by failed refresh: %+v", s)
	}
	if d.volumes["pgbranch-src-main-g2"] {
		t.Fatal("failed refresh leaked the new volume")
	}
	if !d.volumes["pgbranch-src-main"] {
		t.Fatal("current volume must survive a failed refresh")
	}
}

func TestRemoveSource(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	d.volumes["pgbranch-src-main"] = true
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}
	// refused while a live branch exists
	err := e.RemoveSource(context.Background(), "main")
	if err == nil || !strings.Contains(err.Error(), "live branch") {
		t.Fatalf("want live-branch refusal, got %v", err)
	}
	if err := e.DestroyBranch(context.Background(), "pr-1"); err != nil {
		t.Fatal(err)
	}
	if err := e.RemoveSource(context.Background(), "main"); err != nil {
		t.Fatal(err)
	}
	if d.volumes["pgbranch-src-main"] {
		t.Fatal("source volume not removed")
	}
	if _, err := r.GetSourceByName("main"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("want source gone, got %v", err)
	}
}

func TestRunReaperDestroysExpired(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "short", "main", time.Millisecond); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.RunReaper(ctx, 50*time.Millisecond, nil)
	// expires_at has second resolution; the reaper should catch it within ~2s
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := r.GetBranchByName("short"); errors.Is(err, registry.ErrNotFound) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("reaper never destroyed the expired branch")
}

func TestReapExpired(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "short", "main", time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateBranch(context.Background(), "forever", "main", 0); err != nil {
		t.Fatal(err)
	}

	// fake clock: before expiry nothing happens
	destroyed, err := e.ReapExpired(context.Background(), time.Now())
	if err != nil || len(destroyed) != 0 {
		t.Fatalf("destroyed=%v err=%v", destroyed, err)
	}
	// well past expiry: only the TTL'd branch goes
	destroyed, err = e.ReapExpired(context.Background(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(destroyed) != 1 || destroyed[0] != "short" {
		t.Fatalf("destroyed=%v want [short]", destroyed)
	}
	if _, err := r.GetBranchByName("short"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("short not destroyed: %v", err)
	}
	if _, err := r.GetBranchByName("forever"); err != nil {
		t.Fatalf("forever was reaped: %v", err)
	}
}
