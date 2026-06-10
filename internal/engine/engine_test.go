package engine

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/abd-ulbasit/pgbranch/internal/registry"
	"github.com/abd-ulbasit/pgbranch/internal/runtime"
)

type fakeDriver struct {
	volumes    map[string]bool
	containers map[string]bool
	failStart  bool
	execErr    error
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
func (f *fakeDriver) RunHelper(ctx context.Context, s runtime.HelperSpec) error { return nil }
func (f *fakeDriver) StartBranch(ctx context.Context, s runtime.BranchSpec) (string, error) {
	if f.failStart {
		return "", errors.New("boom")
	}
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

	b, err := e.CreateBranch(context.Background(), "pr-1", "main")
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

	if _, err := e.CreateBranch(context.Background(), "pr-1", "main"); err == nil {
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
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main"); err != nil {
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
