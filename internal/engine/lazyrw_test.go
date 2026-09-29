package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/abd-ulbasit/pgoverlay/internal/cow"
	"github.com/abd-ulbasit/pgoverlay/internal/metrics"
	"github.com/abd-ulbasit/pgoverlay/internal/registry"
	"github.com/abd-ulbasit/pgoverlay/internal/runtime"
)

// lazyrwActiveOut is what the entrypoint leaves in cow-mode when the shim is
// active.
const lazyrwActiveOut = "lazyrw\n/pgoverlay/rw/lazyrw/liblazyrw-glibc-x86_64.so\n"

// lazyrwCowModeOut makes the fake's ExecOutput answer the cow-mode read with
// out (or err), and anything else with "".
func lazyrwCowModeOut(out string, err error) func(string, []string) (string, error) {
	return func(_ string, cmd []string) (string, error) {
		if slices.Equal(cmd, cowModeCmd) {
			return out, err
		}
		return "", nil
	}
}

// lazyrwCowModeReads lists the containers the engine read cow-mode in, in
// order.
func lazyrwCowModeReads(d *fakeDriver) []string {
	var ids []string
	for i, c := range d.execOuts {
		if slices.Equal(c, cowModeCmd) {
			ids = append(ids, d.execOutIDs[i])
		}
	}
	return ids
}

// lazyrwInstalls returns the install helpers run against rw volume vol.
func lazyrwInstalls(d *fakeDriver, vol string) []runtime.HelperSpec {
	cmd, _ := cow.OverlayInstall()
	var out []runtime.HelperSpec
	for _, h := range d.helpers {
		if !slices.Equal(h.Cmd, cmd) {
			continue
		}
		for _, m := range h.Mounts {
			if m.Volume == vol && m.Target == cow.RWPath && !m.ReadOnly {
				out = append(out, h)
			}
		}
	}
	return out
}

// lazyrwEnv returns the value of key in a branch spec's environment.
func lazyrwEnv(s runtime.BranchSpec, key string) (string, bool) {
	for _, e := range s.Env {
		if k, v, _ := strings.Cut(e, "="); k == key {
			return v, true
		}
	}
	return "", false
}

// assertLazyRWSpec checks that a branch container starts with the lazyrw
// setting the engine was built with.
func assertLazyRWSpec(t *testing.T, s runtime.BranchSpec, want string) {
	t.Helper()
	if got, ok := lazyrwEnv(s, "PGOVERLAY_LAZYRW"); !ok || got != want {
		t.Errorf("spec %q: PGOVERLAY_LAZYRW = %q (set %v), want %q", s.Name, got, ok, want)
	}
}

// wantCowModes compares CowModeCounts with want, every other mode 0.
func wantCowModes(t *testing.T, e *Engine, want map[string]int) {
	t.Helper()
	full := map[string]int{}
	for _, m := range CowModes {
		full[m] = want[m]
	}
	if got := e.CowModeCounts(); !maps.Equal(got, full) {
		t.Errorf("CowModeCounts = %v, want %v", got, full)
	}
}

// captureLogs sends slog's default logger to a buffer for the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// A created overlay branch's rw volume gets every lazyrw build through the
// install helper (base64 in the environment, one variable each), its
// container gets PGOVERLAY_LAZYRW=on, and once it is ready the engine reads
// the mode it started in.
func TestLazyRWCreateInstallsBuildsAndReadsTheMode(t *testing.T) {
	d := newFake()
	d.execOutFn = lazyrwCowModeOut(lazyrwActiveOut, nil)
	e, r := testEngine(t, d)
	readySource(t, r)
	b, err := e.CreateBranch(context.Background(), "pr-1", "main", 0)
	if err != nil {
		t.Fatal(err)
	}

	installs := lazyrwInstalls(d, b.RWVolume)
	if len(installs) != 1 {
		t.Fatalf("%d install helpers ran on %s, want 1", len(installs), b.RWVolume)
	}
	h := installs[0]
	if h.Image != runtime.UtilityImage || h.Privileged {
		t.Errorf("install helper image %q privileged %v, want the unprivileged utility image", h.Image, h.Privileged)
	}
	env := map[string]string{}
	for _, kv := range h.Env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	if env["PGOVERLAY_ENTRYPOINT"] != cow.EntrypointScript {
		t.Error("install helper does not carry the overlay entrypoint")
	}
	for v, want := range cow.Variants() {
		got, err := base64.StdEncoding.DecodeString(env[cow.LazyRWEnvName(v)])
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: install helper carries a different build (err %v)", cow.LazyRWEnvName(v), err)
		}
	}
	if len(env) != 1+len(cow.Variants()) {
		t.Errorf("install helper env has %d variables, want the entrypoint and %d builds", len(env), len(cow.Variants()))
	}

	if len(d.branches) != 1 {
		t.Fatalf("%d branch starts", len(d.branches))
	}
	assertLazyRWSpec(t, d.branches[0], "on")
	if _, ok := lazyrwEnv(d.branches[0], "PGOVERLAY_WAL_RECYCLE"); ok {
		t.Error("PGOVERLAY_WAL_RECYCLE set although wal recycling was not turned off")
	}

	if got := lazyrwCowModeReads(d); !slices.Equal(got, []string{b.ContainerID}) {
		t.Fatalf("cow-mode read in %v, want once in %s", got, b.ContainerID)
	}
	// after readiness, before the branch is marked ready
	ready := d.logIndex("exec:pg_isready")
	read := indexAfter(d.log, "execout:sh:"+b.ContainerID, ready)
	if ready < 0 || read < 0 {
		t.Errorf("cow-mode read (log %d) does not follow readiness (log %d): %v", read, ready, d.log)
	}
	wantCowModes(t, e, map[string]int{cow.CowModeLazyRW: 1})
}

// --lazyrw=off and --wal-recycle=off reach the entrypoint. The builds are
// installed anyway, so switching lazyrw back on needs only a restart.
func TestLazyRWOffAndWALRecycleOffReachTheEntrypoint(t *testing.T) {
	d := newFake()
	d.execOutFn = lazyrwCowModeOut("off\nPGOVERLAY_LAZYRW=off\n", nil)
	e, r := testEngine(t, d, WithLazyRW(false), WithWALRecycle(false))
	readySource(t, r)
	b, err := e.CreateBranch(context.Background(), "pr-1", "main", 0)
	if err != nil {
		t.Fatal(err)
	}
	assertLazyRWSpec(t, d.branches[0], "off")
	if v, ok := lazyrwEnv(d.branches[0], "PGOVERLAY_WAL_RECYCLE"); !ok || v != "off" {
		t.Errorf("PGOVERLAY_WAL_RECYCLE = %q (set %v), want off", v, ok)
	}
	if n := len(lazyrwInstalls(d, b.RWVolume)); n != 1 {
		t.Errorf("%d installs with lazyrw off, want the builds installed anyway", n)
	}
	wantCowModes(t, e, map[string]int{cow.CowModeOff: 1})
}

// An eager branch while lazyrw is on is a WARN naming the entrypoint's
// reason; a branch from before lazyrw counts as eager with the way out; a
// mode that cannot be read is logged, counted as unknown and never fails the
// branch.
func TestLazyRWReportsEagerAndUnreadableModes(t *testing.T) {
	cases := []struct {
		name     string
		out      string
		err      error
		mode     string
		wantLog  string
		wantWarn bool
	}{
		{"eager", "eager\nself-test failed: a file opened read-only before a copy-up did not read the data\n", nil,
			cow.CowModeEager, "self-test failed", true},
		{"legacy entrypoint", "legacy\n", nil, cow.CowModeEager, "reset the branch", true},
		{"read fails", "", errors.New("exec: container gone"), CowModeUnknown, "container gone", true},
		{"garbage", "hello\n", nil, CowModeUnknown, "unexpected", true},
		{"lazyrw", lazyrwActiveOut, nil, cow.CowModeLazyRW, "liblazyrw-glibc-x86_64.so", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			logs := captureLogs(t)
			d := newFake()
			d.execOutFn = lazyrwCowModeOut(c.out, c.err)
			e, r := testEngine(t, d)
			readySource(t, r)
			if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
				t.Fatalf("create failed over the cow-mode read: %v", err)
			}
			wantCowModes(t, e, map[string]int{c.mode: 1})
			out := logs.String()
			if !strings.Contains(out, c.wantLog) {
				t.Errorf("log lacks %q:\n%s", c.wantLog, out)
			}
			if got := strings.Contains(out, "level=WARN"); got != c.wantWarn {
				t.Errorf("WARN logged = %v, want %v:\n%s", got, c.wantWarn, out)
			}
		})
	}
}

// eager because lazyrw is switched off is not a warning: it is what was
// asked for.
func TestLazyRWEagerWhileOffIsNoWarning(t *testing.T) {
	logs := captureLogs(t)
	d := newFake()
	d.execOutFn = lazyrwCowModeOut("legacy\n", nil)
	e, r := testEngine(t, d, WithLazyRW(false))
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("WARN with lazyrw off:\n%s", logs.String())
	}
}

// pgoverlay_branch_cow_mode counts ready overlay branches by mode, for every
// mode the engine knows; a destroyed branch leaves the count.
func TestLazyRWCowModeMetric(t *testing.T) {
	d := newFake()
	mode := lazyrwActiveOut
	d.execOutFn = func(id string, cmd []string) (string, error) {
		if slices.Equal(cmd, cowModeCmd) {
			return mode, nil
		}
		return "", nil
	}
	m := metrics.New()
	e, r := testEngine(t, d, WithMetrics(m))
	m.SetCowModes(e.CowModeCounts)
	readySource(t, r)
	for _, n := range []string{"a", "b"} {
		if _, err := e.CreateBranch(context.Background(), n, "main", 0); err != nil {
			t.Fatal(err)
		}
	}
	mode = "eager\nno lazyrw build for glibc-riscv64\n"
	if _, err := e.CreateBranch(context.Background(), "c", "main", 0); err != nil {
		t.Fatal(err)
	}
	if err := e.DestroyBranch(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	mfs, err := m.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, mf := range mfs {
		if mf.GetName() != "pgoverlay_branch_cow_mode" {
			continue
		}
		for _, s := range mf.GetMetric() {
			got[s.GetLabel()[0].GetValue()] = s.GetGauge().GetValue()
		}
	}
	// one series per engine mode, so the metric and CowModes cannot drift
	want := map[string]float64{}
	for _, mode := range CowModes {
		want[mode] = 0
	}
	want[cow.CowModeLazyRW], want[cow.CowModeEager] = 1, 1
	if !maps.Equal(got, want) {
		t.Fatalf("pgoverlay_branch_cow_mode = %v, want %v", got, want)
	}
	if n := testutil.CollectAndCount(m.Registry(), "pgoverlay_branch_cow_mode"); n != len(CowModes) {
		t.Fatalf("%d series, want %d", n, len(CowModes))
	}
}

// Branch-from-branch: the parent's fresh rw volume and the child's both get
// the builds, both containers start with the shim on, and both report their
// mode once ready.
func TestLazyRWFreezeParentAndChild(t *testing.T) {
	d := newFake()
	d.execOutFn = lazyrwCowModeOut(lazyrwActiveOut, nil)
	e, r := testEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "p", "main", 0); err != nil {
		t.Fatal(err)
	}
	c, err := e.CreateBranchFrom(context.Background(), "c", "p", 0)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := r.GetBranchByName("p")
	for _, vol := range []string{p.RWVolume, c.RWVolume} {
		if n := len(lazyrwInstalls(d, vol)); n != 1 {
			t.Errorf("%d installs into %s, want 1", n, vol)
		}
	}
	if len(d.branches) != 3 {
		t.Fatalf("%d starts, want create p, restart p, start c", len(d.branches))
	}
	for _, s := range d.branches {
		assertLazyRWSpec(t, s, "on")
	}
	reads := lazyrwCowModeReads(d)
	if len(reads) != 3 || reads[1] != p.ContainerID || reads[2] != c.ContainerID {
		t.Errorf("cow-mode reads %v, want p, restarted p (%s), c (%s)", reads, p.ContainerID, c.ContainerID)
	}
	wantCowModes(t, e, map[string]int{cow.CowModeLazyRW: 2})
}

// A failed freeze restores the parent on its original rw volume, which kept
// the builds installed when the parent was created: the restored container
// starts with the shim on and reports its mode.
func TestLazyRWRestoreParentKeepsTheShim(t *testing.T) {
	d := newFake()
	d.execOutFn = lazyrwCowModeOut(lazyrwActiveOut, nil)
	// attempts: 1 = create p, 2 = restart p (ok), 3 = start child (fails),
	// 4 = restore p (ok)
	d.failStartAt = map[int]bool{3: true}
	e, r := testEngine(t, d)
	readySource(t, r)
	p, err := e.CreateBranch(context.Background(), "p", "main", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateBranchFrom(context.Background(), "c", "p", 0); err == nil {
		t.Fatal("want the child to fail")
	}
	restored := d.branches[len(d.branches)-1]
	if got := mountAt(t, restored, cow.RWPath); got.Volume != p.RWVolume {
		t.Fatalf("restored parent mounts %q, want its original rw volume %q", got.Volume, p.RWVolume)
	}
	assertLazyRWSpec(t, restored, "on")
	if n := len(lazyrwInstalls(d, p.RWVolume)); n != 1 {
		t.Errorf("%d installs into the parent's original volume, want the one from its create", n)
	}
	got, _ := r.GetBranchByName("p")
	if got.State != registry.BranchReady {
		t.Fatalf("parent %s after restore", got.State)
	}
	reads := lazyrwCowModeReads(d)
	if len(reads) == 0 || reads[len(reads)-1] != got.ContainerID {
		t.Errorf("cow-mode reads %v, want the restored parent (%s) last", reads, got.ContainerID)
	}
	wantCowModes(t, e, map[string]int{cow.CowModeLazyRW: 1})
}

// Reset re-provisions and recover rewrites the entrypoint: both put the
// current builds on the volume and start with the shim on.
func TestLazyRWResetAndRecoverInstallTheCurrentBuilds(t *testing.T) {
	t.Run("reset", func(t *testing.T) {
		d := newFake()
		d.execOutFn = lazyrwCowModeOut(lazyrwActiveOut, nil)
		e, r := testEngine(t, d)
		readySource(t, r)
		b, err := e.CreateBranch(context.Background(), "pr-1", "main", 0)
		if err != nil {
			t.Fatal(err)
		}
		if b, err = e.ResetBranch(context.Background(), "pr-1"); err != nil {
			t.Fatal(err)
		}
		if n := len(lazyrwInstalls(d, b.RWVolume)); n < 1 {
			t.Fatalf("reset installed nothing into %s", b.RWVolume)
		}
		assertLazyRWSpec(t, d.branches[len(d.branches)-1], "on")
		if reads := lazyrwCowModeReads(d); len(reads) != 2 {
			t.Errorf("cow-mode reads %v, want one per start", reads)
		}
		wantCowModes(t, e, map[string]int{cow.CowModeLazyRW: 1})
	})
	t.Run("recover", func(t *testing.T) {
		d := newFake()
		d.execOutFn = lazyrwCowModeOut(lazyrwActiveOut, nil)
		e, r := testEngine(t, d)
		crashedFreeze(t, e, r, d)
		p, err := e.RecoverBranch(context.Background(), "p")
		if err != nil {
			t.Fatal(err)
		}
		if n := len(lazyrwInstalls(d, p.RWVolume)); n != 1 {
			t.Fatalf("%d installs into %s on recover, want 1", n, p.RWVolume)
		}
		assertLazyRWSpec(t, d.branches[len(d.branches)-1], "on")
		if reads := lazyrwCowModeReads(d); !slices.Equal(reads, []string{p.ContainerID}) {
			t.Errorf("cow-mode reads %v, want the recovered container", reads)
		}
	})
}

// Reconcile's restart of a ready branch whose container was lost starts it
// with the shim on and reads its mode again.
func TestLazyRWReconcileRestartReadsTheMode(t *testing.T) {
	d := newFake()
	d.execOutFn = lazyrwCowModeOut(lazyrwActiveOut, nil)
	e, r := testEngine(t, d)
	readySource(t, r)
	b := readyBranch(t, e, "pr-1")
	delete(d.containers, b.ContainerID)
	if _, err := e.ApplyReconcile(context.Background(), time.Now(), 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	assertLazyRWSpec(t, d.branches[len(d.branches)-1], "on")
	if reads := lazyrwCowModeReads(d); len(reads) != 2 {
		t.Errorf("cow-mode reads %v, want one for the create and one for the restart", reads)
	}
}

// A branchd that starts while branches already run learns their modes with
// RefreshCowModes, once per branch.
func TestRefreshCowModesAfterARestart(t *testing.T) {
	d := newFake()
	d.execOutFn = lazyrwCowModeOut(lazyrwActiveOut, nil)
	e, r := testEngine(t, d)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}
	restarted := New(r, d, "postgres:17") // a new process on the same registry
	wantCowModes(t, restarted, map[string]int{CowModeUnknown: 1})
	reads := len(lazyrwCowModeReads(d))
	restarted.RefreshCowModes(context.Background())
	wantCowModes(t, restarted, map[string]int{cow.CowModeLazyRW: 1})
	restarted.RefreshCowModes(context.Background())
	if n := len(lazyrwCowModeReads(d)) - reads; n != 1 {
		t.Errorf("two refreshes read cow-mode %d times, want once", n)
	}
}

// zfs and csi branches run on block-level clones: no cow-mode, no metric.
func TestCowModeOnlyForOverlay(t *testing.T) {
	for name, mk := range map[string]func(*testing.T, runtime.Driver) (*Engine, *registry.Registry){
		"zfs": zfsEngine,
		"csi": func(t *testing.T, d runtime.Driver) (*Engine, *registry.Registry) { return csiEngine(t, d) },
	} {
		t.Run(name, func(t *testing.T) {
			d := newFake()
			e, _ := mk(t, d)
			if got := e.CowModeCounts(); got != nil {
				t.Errorf("CowModeCounts = %v, want nil", got)
			}
			e.observeCowMode(context.Background(), &registry.Branch{ID: "x", Name: "x"}, "cid-x")
			e.RefreshCowModes(context.Background())
			if len(d.execOuts) != 0 {
				t.Errorf("%s engine exec'd %v", name, d.execOuts)
			}
		})
	}
}
