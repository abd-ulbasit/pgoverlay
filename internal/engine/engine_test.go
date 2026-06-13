package engine

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/pgbranch/internal/metrics"
	"github.com/abd-ulbasit/pgbranch/internal/registry"
	"github.com/abd-ulbasit/pgbranch/internal/runtime"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

type fakeDriver struct {
	volumes       map[string]bool
	containers    map[string]bool
	failStart     bool
	failStartAt   map[int]bool // fail the Nth StartBranch attempt (1-based, counts failures too)
	startAttempts int
	execErr       error
	psqlErr       error                                         // returned by Exec for psql commands only (fails masking/checkpoint)
	helperErr     error                                         // returned by RunHelper (fails seeding)
	cloneErr      error                                         // returned by CloneVolume
	clones        [][2]string                                   // every CloneVolume call (src, dst), in order
	helperOut     string                                        // returned by RunHelper as captured output
	helpers       []runtime.HelperSpec                          // every RunHelper call, in order
	starts        int                                           // successful StartBranch invocations
	branches      []runtime.BranchSpec                          // every successful StartBranch call, in order
	execs         [][]string                                    // every Exec call, in order
	execOuts      [][]string                                    // every ExecOutput call, in order
	execOutIDs    []string                                      // container id of every ExecOutput call, in order
	execOutFn     func(id string, cmd []string) (string, error) // canned ExecOutput behavior (nil = "")
	log           []string                                      // coarse op log for ordering assertions

	// kubelet status-sync race simulation: the first N Inspect calls
	// report no Host (pod exec-ready before status.podIP is published).
	emptyHostInspects int
	inspects          int
}

func newFake() *fakeDriver {
	return &fakeDriver{volumes: map[string]bool{}, containers: map[string]bool{}}
}
func (f *fakeDriver) EnsureImage(ctx context.Context, image string) error { return nil }
func (f *fakeDriver) CreateVolume(ctx context.Context, name string, l map[string]string) error {
	f.volumes[name] = true
	f.log = append(f.log, "volume:"+name)
	return nil
}
func (f *fakeDriver) RemoveVolume(ctx context.Context, name string) error {
	delete(f.volumes, name)
	f.log = append(f.log, "rmvolume:"+name)
	return nil
}
func (f *fakeDriver) CloneVolume(ctx context.Context, src, dst string, l map[string]string) error {
	if f.cloneErr != nil {
		return f.cloneErr
	}
	f.volumes[dst] = true
	f.clones = append(f.clones, [2]string{src, dst})
	f.log = append(f.log, "clone:"+src+">"+dst)
	return nil
}
func (f *fakeDriver) RunHelper(ctx context.Context, s runtime.HelperSpec) (string, error) {
	f.helpers = append(f.helpers, s)
	return f.helperOut, f.helperErr
}
func (f *fakeDriver) StartBranch(ctx context.Context, s runtime.BranchSpec) (string, error) {
	f.startAttempts++
	if f.failStart || f.failStartAt[f.startAttempts] {
		return "", errors.New("boom")
	}
	f.starts++
	f.branches = append(f.branches, s)
	f.containers["cid-"+s.Name] = true
	f.log = append(f.log, "start:"+s.Name)
	return "cid-" + s.Name, nil
}
func (f *fakeDriver) Exec(ctx context.Context, id string, cmd []string) error {
	f.execs = append(f.execs, cmd)
	if len(cmd) > 0 {
		f.log = append(f.log, "exec:"+cmd[0]+":"+id)
	}
	if len(cmd) > 0 && cmd[0] == "psql" && f.psqlErr != nil {
		return f.psqlErr
	}
	return f.execErr
}
func (f *fakeDriver) ExecOutput(ctx context.Context, id string, cmd []string) (string, error) {
	f.execOuts = append(f.execOuts, cmd)
	f.execOutIDs = append(f.execOutIDs, id)
	if len(cmd) > 0 {
		f.log = append(f.log, "execout:"+cmd[0]+":"+id)
	}
	if f.execOutFn != nil {
		return f.execOutFn(id, cmd)
	}
	return "", nil
}

// logIndex returns the position of the first log entry containing substr.
func (f *fakeDriver) logIndex(substr string) int {
	for i, e := range f.log {
		if strings.Contains(e, substr) {
			return i
		}
	}
	return -1
}

// psqlExecs returns the recorded Exec calls that ran psql (masking).
func (f *fakeDriver) psqlExecs() [][]string {
	var out [][]string
	for _, c := range f.execs {
		if len(c) > 0 && c[0] == "psql" {
			out = append(out, c)
		}
	}
	return out
}
func (f *fakeDriver) Inspect(ctx context.Context, id string) (runtime.ContainerInfo, error) {
	f.inspects++
	if f.inspects <= f.emptyHostInspects {
		return runtime.ContainerInfo{ID: id, Running: f.containers[id], Port: 54321}, nil
	}
	return runtime.ContainerInfo{ID: id, Running: f.containers[id], Host: "127.0.0.1", Port: 54321}, nil
}
func (f *fakeDriver) StopRemove(ctx context.Context, id string) error {
	delete(f.containers, id)
	f.log = append(f.log, "stop:"+id)
	return nil
}
func (f *fakeDriver) ListManaged(ctx context.Context) ([]runtime.ContainerInfo, error) {
	var out []runtime.ContainerInfo
	for id := range f.containers {
		out = append(out, runtime.ContainerInfo{ID: id, Running: true})
	}
	return out, nil
}
func (f *fakeDriver) ListManagedVolumes(ctx context.Context) ([]string, error) {
	var out []string
	for name := range f.volumes {
		out = append(out, name)
	}
	return out, nil
}

func testEngine(t *testing.T, d runtime.Driver, opts ...Option) (*Engine, *registry.Registry) {
	t.Helper()
	r, err := registry.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return New(r, d, "postgres:17", opts...), r
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
	// registry says creating, stuck past the timeout -> failed + cleaned
	b := &registry.Branch{Name: "stuck", SourceID: mustSource(t, r).ID, RWVolume: "pgbranch-br-stuck-rw"}
	if err := r.CreateBranch(b); err != nil {
		t.Fatal(err)
	}
	d.volumes["pgbranch-br-stuck-rw"] = true
	// container exists but registry has no row -> removed
	d.containers["cid-ghost"] = true

	// now far in the future so the just-created row is past the stuck timeout.
	_, err := e.ApplyReconcile(context.Background(), time.Now().Add(time.Hour), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := r.GetBranchByName("stuck")
	if got.State != registry.BranchFailed {
		t.Fatalf("state=%q", got.State)
	}
	if d.containers["cid-ghost"] {
		t.Fatal("ghost container not removed")
	}
	if d.volumes["pgbranch-br-stuck-rw"] {
		t.Fatal("stuck branch rw volume not removed")
	}
}

// Kubernetes pods answer exec probes before kubelet publishes status.podIP;
// the engine must keep inspecting until an address appears instead of
// recording host "" (which the proxy would dial as ":5432").
func TestCreateBranchWaitsForAddress(t *testing.T) {
	d := newFake()
	d.emptyHostInspects = 2
	e, r := testEngine(t, d)
	readySource(t, r)

	b, err := e.CreateBranch(context.Background(), "pr-1", "main", 0)
	if err != nil {
		t.Fatal(err)
	}
	if b.Host != "127.0.0.1" {
		t.Fatalf("Host=%q want 127.0.0.1 (engine must retry empty-host inspects)", b.Host)
	}
	if d.inspects < 3 {
		t.Fatalf("inspects=%d, want >=3 (two empty-host reads then the real one)", d.inspects)
	}
}

func TestCreateBranchObservesOpMetric(t *testing.T) {
	d := newFake()
	m := metrics.New()
	r, err := registry.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	e := New(r, d, "postgres:17", WithMetrics(m))
	readySource(t, r)

	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}
	// the create histogram recorded exactly one observation
	want := `
# HELP pgbranch_branch_op_duration_seconds Duration of branch operations by op (create|reset|destroy|from_branch|diff).
# TYPE pgbranch_branch_op_duration_seconds histogram
pgbranch_branch_op_duration_seconds_count{op="create"} 1
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(want),
		"pgbranch_branch_op_duration_seconds_count"); err != nil {
		t.Fatal(err)
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
	if b.Host != "127.0.0.1" {
		t.Fatalf("Host=%q want 127.0.0.1 (from driver Inspect)", b.Host)
	}
	if !d.volumes["pgbranch-br-pr-1-rw"] {
		t.Fatal("rw volume not created")
	}
	if !d.containers["cid-pgbranch-br-pr-1"] {
		t.Fatal("container not started")
	}
}

func TestCreateBranchRejectsInvalidNames(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)

	for _, name := range []string{
		"",                      // empty
		"PR-1",                  // uppercase
		"pr_1",                  // underscore
		"-pr-1",                 // leading dash
		strings.Repeat("a", 42), // longer than 41 chars
	} {
		_, err := e.CreateBranch(context.Background(), name, "main", 0)
		if !errors.Is(err, ErrInvalidName) {
			t.Errorf("CreateBranch(%q) err = %v, want ErrInvalidName", name, err)
		}
		if err != nil && !strings.Contains(err.Error(), "[a-z0-9][a-z0-9-]{0,40}") {
			t.Errorf("CreateBranch(%q) error %q does not state the rule", name, err)
		}
	}
	// no registry rows or driver resources created for rejected names
	if live, _ := r.ListLiveBranches(); len(live) != 0 {
		t.Fatalf("rejected names left rows: %v", live)
	}
	if len(d.volumes) != 0 || len(d.containers) != 0 {
		t.Fatalf("rejected names leaked resources: v=%v c=%v", d.volumes, d.containers)
	}
	// boundary: max-length valid name (41 chars) is accepted
	long := strings.Repeat("a", 41)
	if _, err := e.CreateBranch(context.Background(), long, "main", 0); err != nil {
		t.Fatalf("CreateBranch(41 chars) = %v, want ok", err)
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

func TestCreateBranchAppliesMaskScriptsInOrder(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	src := readySource(t, r)
	scripts := []registry.MaskScript{
		{Name: "emails.sql", SQL: "UPDATE users SET email = 'x@invalid'"},
		{Name: "names.sql", SQL: "UPDATE users SET name = 'redacted'"},
	}
	if err := r.SetMaskScripts(src.ID, scripts); err != nil {
		t.Fatal(err)
	}

	b, err := e.CreateBranch(context.Background(), "pr-1", "main", 0)
	if err != nil {
		t.Fatal(err)
	}
	if b.State != registry.BranchReady {
		t.Fatalf("state=%q", b.State)
	}
	got := d.psqlExecs()
	if len(got) != 2 {
		t.Fatalf("psql execs=%d want 2: %v", len(got), got)
	}
	for i, sc := range scripts {
		want := []string{"psql", "-v", "ON_ERROR_STOP=1", "-U", "postgres", "-d", "postgres", "-c", sc.SQL}
		if len(got[i]) != len(want) {
			t.Fatalf("exec[%d]=%v want %v", i, got[i], want)
		}
		for j := range want {
			if got[i][j] != want[j] {
				t.Fatalf("exec[%d]=%v want %v", i, got[i], want)
			}
		}
	}
	// masking runs after readiness: pg_isready precedes the first psql call
	firstPsql, lastReady := -1, -1
	for i, c := range d.execs {
		switch c[0] {
		case "psql":
			if firstPsql == -1 {
				firstPsql = i
			}
		case "pg_isready":
			lastReady = i
		}
	}
	if lastReady == -1 || firstPsql < lastReady {
		t.Fatalf("masking did not run after readiness: execs=%v", d.execs)
	}
}

func TestCreateBranchMaskUsesSourceUserAndDB(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	s := &registry.Source{Name: "main", PGVersion: "17", Volume: "pgbranch-src-main", ConnUser: "app", ConnDB: "appdb"}
	if err := r.CreateSource(s); err != nil {
		t.Fatal(err)
	}
	if err := r.SetSourceState(s.ID, registry.SourceReady, "test"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetMaskScripts(s.ID, []registry.MaskScript{{Name: "m.sql", SQL: "SELECT 1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}
	got := d.psqlExecs()
	if len(got) != 1 {
		t.Fatalf("psql execs=%v", got)
	}
	if got[0][4] != "app" || got[0][6] != "appdb" {
		t.Fatalf("psql user/db args wrong: %v", got[0])
	}
}

func TestCreateBranchMaskFailureFailsAndUnwinds(t *testing.T) {
	d := newFake()
	d.psqlErr = errors.New("ERROR: relation \"users\" does not exist")
	e, r := testEngine(t, d)
	src := readySource(t, r)
	if err := r.SetMaskScripts(src.ID, []registry.MaskScript{{Name: "bad.sql", SQL: "UPDATE users SET x=1"}}); err != nil {
		t.Fatal(err)
	}

	_, err := e.CreateBranch(context.Background(), "pr-1", "main", 0)
	if err == nil || !strings.Contains(err.Error(), "bad.sql") {
		t.Fatalf("want masking error naming the script, got %v", err)
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

func TestResetBranchReappliesMasking(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	src := readySource(t, r)
	if err := r.SetMaskScripts(src.ID, []registry.MaskScript{{Name: "m.sql", SQL: "UPDATE t SET x=1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}
	if n := len(d.psqlExecs()); n != 1 {
		t.Fatalf("psql execs after create=%d want 1", n)
	}
	if _, err := e.ResetBranch(context.Background(), "pr-1"); err != nil {
		t.Fatal(err)
	}
	if n := len(d.psqlExecs()); n != 2 {
		t.Fatalf("psql execs after reset=%d want 2 (reset re-clones, masking must re-run)", n)
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

// dumpHelpers returns the recorded RunHelper specs whose script runs pg_dump.
func (f *fakeDriver) dumpHelpers() []runtime.HelperSpec {
	var out []runtime.HelperSpec
	for _, h := range f.helpers {
		if len(h.Cmd) == 3 && h.Cmd[0] == "bash" && strings.Contains(h.Cmd[2], "pg_dump") {
			out = append(out, h)
		}
	}
	return out
}

func TestAddSourceViaDumpUsesSeedDump(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	s := &registry.Source{Name: "main", PGVersion: "17", ConnHost: "db.supabase.co", ConnPort: 5432,
		ConnUser: "postgres", ConnDB: "appdb", SeedVia: registry.SeedViaDump, DumpSchemas: []string{"public"}}
	if err := e.AddSource(context.Background(), s, "secret"); err != nil {
		t.Fatal(err)
	}
	if got := mustSource(t, r); got.State != registry.SourceReady || got.SeedVia != registry.SeedViaDump {
		t.Fatalf("source after dump seed: %+v", got)
	}
	dumps := d.dumpHelpers()
	if len(dumps) != 1 {
		t.Fatalf("dump helpers=%d want 1 (helpers: %v)", len(dumps), d.helpers)
	}
	h := dumps[0]
	if h.Image != "postgres:17" || h.User != "postgres" {
		t.Fatalf("dump helper image/user: %+v", h)
	}
	if !strings.Contains(strings.Join(h.Env, "\n"), "PGB_DB=appdb") {
		t.Fatalf("dump helper env missing database: %v", h.Env)
	}
	if !strings.Contains(h.Cmd[2], "-n 'public'") {
		t.Fatalf("dump helper script missing schema scope:\n%s", h.Cmd[2])
	}
	if len(h.Mounts) != 1 || h.Mounts[0].Volume != "pgbranch-src-main" {
		t.Fatalf("dump helper mounts: %+v", h.Mounts)
	}
}

func TestRefreshSourceKeepsDumpMethod(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	s := &registry.Source{Name: "main", PGVersion: "17", ConnHost: "h", ConnPort: 5432,
		ConnUser: "postgres", SeedVia: registry.SeedViaDump}
	if err := e.AddSource(context.Background(), s, "secret"); err != nil {
		t.Fatal(err)
	}
	if err := e.RefreshSource(context.Background(), "main", "secret"); err != nil {
		t.Fatal(err)
	}
	if got := mustSource(t, r); got.Generation != 2 {
		t.Fatalf("generation=%d want 2", got.Generation)
	}
	if n := len(d.dumpHelpers()); n != 2 {
		t.Fatalf("dump helpers=%d want 2 (add + refresh both via pg_dump)", n)
	}
}

func TestAddSourceBasebackupUnaffected(t *testing.T) {
	d := newFake()
	e, _ := testEngine(t, d)
	s := &registry.Source{Name: "main", PGVersion: "17", ConnHost: "h", ConnPort: 5432, ConnUser: "postgres"}
	if err := e.AddSource(context.Background(), s, "secret"); err != nil {
		t.Fatal(err)
	}
	if n := len(d.dumpHelpers()); n != 0 {
		t.Fatalf("basebackup source ran %d dump helpers", n)
	}
	var basebackup bool
	for _, h := range d.helpers {
		if len(h.Cmd) > 0 && h.Cmd[0] == "pg_basebackup" {
			basebackup = true
		}
	}
	if !basebackup {
		t.Fatalf("pg_basebackup helper not recorded: %v", d.helpers)
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
	go e.RunReconcile(ctx, 50*time.Millisecond, 10*time.Minute, nil)
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

func TestBranchUsage(t *testing.T) {
	d := newFake()
	d.helperOut = "123456\t/pgbranch/rw\n"
	e, r := testEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}

	n, err := e.BranchUsage(context.Background(), "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if n != 123456 {
		t.Fatalf("usage = %d want 123456", n)
	}
	// the measuring helper runs du -sb against the branch's rw volume,
	// mounted read-only
	last := d.helpers[len(d.helpers)-1]
	wantCmd := []string{"du", "-sb", "/pgbranch/rw"}
	if len(last.Cmd) != 3 || last.Cmd[0] != wantCmd[0] || last.Cmd[1] != wantCmd[1] || last.Cmd[2] != wantCmd[2] {
		t.Fatalf("helper cmd = %v want %v", last.Cmd, wantCmd)
	}
	if len(last.Mounts) != 1 || last.Mounts[0].Volume != "pgbranch-br-pr-1-rw" || !last.Mounts[0].ReadOnly {
		t.Fatalf("helper mounts = %+v want ro pgbranch-br-pr-1-rw", last.Mounts)
	}
}

func TestBranchUsageUnknownBranch(t *testing.T) {
	d := newFake()
	e, _ := testEngine(t, d)
	if _, err := e.BranchUsage(context.Background(), "nope"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("err = %v want ErrNotFound", err)
	}
}

func TestBranchUsageBadHelperOutput(t *testing.T) {
	d := newFake()
	d.helperOut = "du: cannot access '/pgbranch/rw'"
	e, r := testEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.BranchUsage(context.Background(), "pr-1"); err == nil {
		t.Fatal("want error for unparseable du output")
	}
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
