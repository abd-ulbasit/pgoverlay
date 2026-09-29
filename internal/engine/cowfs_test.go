package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/pgoverlay/internal/cow"
	"github.com/abd-ulbasit/pgoverlay/internal/registry"
	"github.com/abd-ulbasit/pgoverlay/internal/runtime"
)

// withDuTools makes the engine see exactly these pgoverlay-du binaries
// (machine -> bytes), whatever this build embeds.
func withDuTools(t *testing.T, tools map[string][]byte) {
	t.Helper()
	oldTool, oldTools := duTool, duTools
	t.Cleanup(func() { duTool, duTools = oldTool, oldTools })
	duTool = func(m string) []byte { return tools[cow.NormalizeMachine(m)] }
	duTools = func() map[string][]byte { return tools }
}

// scriptedDriver is the fake driver with RunHelper answered per call.
type scriptedDriver struct {
	*fakeDriver
	run func(spec runtime.HelperSpec) (string, error)
	// leftovers are appended to ListManagedVolumes (runtime.VolumeInfo.Leftover)
	leftovers []runtime.VolumeInfo
}

func (d *scriptedDriver) RunHelper(ctx context.Context, s runtime.HelperSpec) (string, error) {
	d.helpers = append(d.helpers, s)
	if d.run == nil {
		return d.helperOut, d.helperErr
	}
	return d.run(s)
}

func (d *scriptedDriver) ListManagedVolumes(ctx context.Context, instanceID string) ([]runtime.VolumeInfo, error) {
	vols, err := d.fakeDriver.ListManagedVolumes(ctx, instanceID)
	return append(vols, d.leftovers...), err
}

func isProbe(s runtime.HelperSpec) bool {
	return len(s.Cmd) > 3 && s.Cmd[3] == "pgoverlay-probe"
}

func isDuTool(s runtime.HelperSpec) bool {
	return len(s.Cmd) > 3 && s.Cmd[3] == "pgoverlay-du"
}

func isDuSb(s runtime.HelperSpec) bool {
	return len(s.Cmd) == 3 && s.Cmd[0] == "du" && s.Cmd[1] == "-sb"
}

const (
	cloneProbeOut = "pgoverlay-probe-machine=x86_64\npgoverlay-probe-fsmagic=58465342\npgoverlay-probe-bytes=67108864\npgoverlay-probe-used=0\npgoverlay-probe-open=100.00 100.01\npgoverlay-probe-fiemap=0\t67108864\t67108864\t/pgoverlay-probe/upper/u/f\n"
	copyProbeOut  = "pgoverlay-probe-machine=x86_64\npgoverlay-probe-fsmagic=ef53\npgoverlay-probe-bytes=67108864\npgoverlay-probe-used=67239936\npgoverlay-probe-open=100.00 101.42\n"
)

// probedEngine is an engine whose copy-up probe answered probeOut.
func probedEngine(t *testing.T, probeOut string) (*Engine, *registry.Registry, *scriptedDriver) {
	t.Helper()
	d := &scriptedDriver{fakeDriver: newFake()}
	e, r := testEngine(t, d)
	d.run = func(s runtime.HelperSpec) (string, error) {
		if isProbe(s) {
			return probeOut, nil
		}
		return "", nil
	}
	if _, err := e.DetectCopyUp(context.Background(), CopyUpOptions{}); err != nil {
		t.Fatal(err)
	}
	return e, r, d
}

func TestDetectCopyUpRecordsModeAndCleansUp(t *testing.T) {
	withDuTools(t, map[string][]byte{"x86_64": []byte("tool-x86")})
	d := &scriptedDriver{fakeDriver: newFake()}
	e, r := testEngine(t, d)
	if m := e.CopyUpMode(); m != cow.CopyUpUnknown {
		t.Fatalf("before the probe: mode %s, want unknown", m)
	}
	var probe runtime.HelperSpec
	d.run = func(s runtime.HelperSpec) (string, error) {
		probe = s
		return cloneProbeOut, nil
	}
	res, err := e.DetectCopyUp(context.Background(), CopyUpOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != cow.CopyUpClone || e.CopyUpMode() != cow.CopyUpClone || res.FSType != "xfs" || res.SharedBytes != 64<<20 {
		t.Fatalf("result %s, engine mode %s", res, e.CopyUpMode())
	}
	// two probe volumes, labelled for this instance (so reconcile can collect
	// them after a crash), mounted into a SYS_ADMIN helper and removed again
	if !probe.SysAdmin || probe.Privileged || len(probe.Mounts) != 2 {
		t.Fatalf("probe helper %+v", probe)
	}
	var created []string
	for _, entry := range d.log {
		if v, ok := strings.CutPrefix(entry, "volume:"); ok {
			created = append(created, v)
			if !strings.HasPrefix(v, "pgoverlay-probe-") {
				t.Fatalf("probe volume named %q", v)
			}
			if l := d.volumeLabels[v]; l[runtime.LabelInstance] != r.InstanceID() || l["pgoverlay.managed"] != "true" {
				t.Fatalf("probe volume %s labels %v", v, l)
			}
		}
	}
	if len(created) != 2 || probe.Mounts[0].Volume != created[0] || probe.Mounts[1].Volume != created[1] {
		t.Fatalf("created %v, mounted %+v", created, probe.Mounts)
	}
	if len(d.volumes) != 0 {
		t.Fatalf("probe volumes left: %v", d.volumes)
	}
	// the helper carries the binary for the helper to pick by its machine
	if !strings.Contains(strings.Join(probe.Env, "\n"), "PGOVERLAY_DU_X86_64_0=") {
		t.Fatalf("probe env %v lacks the x86_64 tool", probe.Env)
	}
}

func TestDetectCopyUpFailureLeavesModeUnknown(t *testing.T) {
	d := &scriptedDriver{fakeDriver: newFake()}
	e, _ := testEngine(t, d)
	d.run = func(runtime.HelperSpec) (string, error) {
		return "mount: permission denied", errors.New("helper exited 1")
	}
	res, err := e.DetectCopyUp(context.Background(), CopyUpOptions{})
	if err == nil || res.Mode != cow.CopyUpUnknown || e.CopyUpMode() != cow.CopyUpUnknown {
		t.Fatalf("failed probe: res %s err %v mode %s", res, err, e.CopyUpMode())
	}
	if len(d.volumes) != 0 {
		t.Fatalf("a failed probe left its volumes: %v", d.volumes)
	}
}

func TestDetectCopyUpRefusesNonOverlayBackends(t *testing.T) {
	for _, b := range []cow.Backend{cow.BackendZFS, cow.BackendCSI} {
		d := newFake()
		r, err := registry.Open(t.TempDir() + "/t.db")
		if err != nil {
			t.Fatal(err)
		}
		e := NewWithPlanner(r, d, "postgres:17", cow.Planner{Backend: b, Dataset: "tank/pg"})
		if _, err := e.DetectCopyUp(context.Background(), CopyUpOptions{}); err == nil {
			t.Errorf("%s: DetectCopyUp succeeded", b)
		}
		if len(d.helpers) != 0 || len(d.log) != 0 {
			t.Errorf("%s: the refused probe did work: %v", b, d.log)
		}
		r.Close()
	}
}

// The XFS extent size hint goes on the volume root when copy-up clones on XFS
// and there is a root and a hint; it is never set anywhere else.
func TestDetectCopyUpSetsXFSHintOnlyWhereItApplies(t *testing.T) {
	btrfs := strings.Replace(cloneProbeOut, "58465342", "9123683e", 1)
	cases := []struct {
		name     string
		probe    string
		opts     CopyUpOptions
		tools    map[string][]byte
		wantHint bool
	}{
		{"xfs clone with root", cloneProbeOut, CopyUpOptions{Root: "/data/pg", CowExtSize: 16384}, map[string][]byte{"x86_64": []byte("t")}, true},
		{"no root", cloneProbeOut, CopyUpOptions{CowExtSize: 16384}, map[string][]byte{"x86_64": []byte("t")}, false},
		{"hint off", cloneProbeOut, CopyUpOptions{Root: "/data/pg"}, map[string][]byte{"x86_64": []byte("t")}, false},
		{"copy mode", copyProbeOut, CopyUpOptions{Root: "/data/pg", CowExtSize: 16384}, map[string][]byte{"x86_64": []byte("t")}, false},
		{"btrfs has no hint", btrfs, CopyUpOptions{Root: "/data/pg", CowExtSize: 16384}, map[string][]byte{"x86_64": []byte("t")}, false},
		{"no tool for the machine", cloneProbeOut, CopyUpOptions{Root: "/data/pg", CowExtSize: 16384}, map[string][]byte{"aarch64": []byte("t")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withDuTools(t, tc.tools)
			d := &scriptedDriver{fakeDriver: newFake()}
			e, _ := testEngine(t, d)
			var hint *runtime.HelperSpec
			d.run = func(s runtime.HelperSpec) (string, error) {
				if isProbe(s) {
					return tc.probe, nil
				}
				hint = &s
				return "cowextsize=16384\t/pgoverlay-root\n", nil
			}
			if _, err := e.DetectCopyUp(context.Background(), tc.opts); err != nil {
				t.Fatal(err)
			}
			if (hint != nil) != tc.wantHint {
				t.Fatalf("hint helper ran: %v, want %v", hint != nil, tc.wantHint)
			}
			if hint == nil {
				return
			}
			if hint.SysAdmin || hint.Privileged {
				t.Fatalf("the hint helper needs no privileges: %+v", hint)
			}
			if len(hint.Mounts) != 1 || hint.Mounts[0].Kind != runtime.MountHostPath || hint.Mounts[0].Volume != "/data/pg" {
				t.Fatalf("hint helper mounts %+v, want the root as a host path", hint.Mounts)
			}
			if got := strings.Join(hint.Cmd, " "); !strings.Contains(got, "-c") || !strings.HasSuffix(got, "16384 /pgoverlay-root") {
				t.Fatalf("hint helper cmd %q", got)
			}
		})
	}
}

func TestBranchUsageCloneModeCountsExclusiveBytes(t *testing.T) {
	withDuTools(t, map[string][]byte{"x86_64": []byte("tool-x86")})
	e, r, d := probedEngine(t, cloneProbeOut)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}
	d.run = func(s runtime.HelperSpec) (string, error) {
		if isDuTool(s) {
			return "4096\t67108864\t67112960\t/pgoverlay/rw\n", nil
		}
		return "67112960\t/pgoverlay/rw\n", nil
	}
	n, err := e.BranchUsage(context.Background(), "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if n != 4096 {
		t.Fatalf("usage = %d, want pgoverlay-du's exclusive 4096", n)
	}
	last := d.helpers[len(d.helpers)-1]
	if !isDuTool(last) || len(last.Mounts) != 1 || last.Mounts[0].Volume != "pgoverlay-br-pr-1-rw" || !last.Mounts[0].ReadOnly {
		t.Fatalf("usage helper %+v", last)
	}
	if !strings.Contains(strings.Join(last.Env, "\n"), "PGOVERLAY_DU_0=") {
		t.Fatalf("usage helper env %v lacks the tool", last.Env)
	}
}

func TestBranchUsageCloneModeFallsBackToDu(t *testing.T) {
	withDuTools(t, map[string][]byte{"x86_64": []byte("tool-x86")})
	e, r, d := probedEngine(t, cloneProbeOut)
	readySource(t, r)
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}
	d.run = func(s runtime.HelperSpec) (string, error) {
		if isDuTool(s) {
			return "", errors.New("exec format error")
		}
		return "777\t/pgoverlay/rw\n", nil
	}
	n, err := e.BranchUsage(context.Background(), "pr-1")
	if err != nil || n != 777 {
		t.Fatalf("usage = %d, %v; want du -sb's 777 after pgoverlay-du failed", n, err)
	}
	if !isDuSb(d.helpers[len(d.helpers)-1]) {
		t.Fatalf("last helper %+v, want du -sb", d.helpers[len(d.helpers)-1])
	}
}

func TestBranchUsageUsesDuWithoutCloneOrTool(t *testing.T) {
	cases := []struct {
		name  string
		probe string
		tools map[string][]byte
	}{
		{"copy mode", copyProbeOut, map[string][]byte{"x86_64": []byte("t")}},
		{"clone without a tool for the machine", cloneProbeOut, map[string][]byte{"aarch64": []byte("t")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withDuTools(t, tc.tools)
			e, r, d := probedEngine(t, tc.probe)
			readySource(t, r)
			if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
				t.Fatal(err)
			}
			d.run = func(runtime.HelperSpec) (string, error) { return "123\t/pgoverlay/rw\n", nil }
			if n, err := e.BranchUsage(context.Background(), "pr-1"); err != nil || n != 123 {
				t.Fatalf("usage = %d, %v", n, err)
			}
			if last := d.helpers[len(d.helpers)-1]; !isDuSb(last) {
				t.Fatalf("usage helper %+v, want du -sb", last)
			}
		})
	}
}

// A volume the runtime reports only as a leftover directory (its volume is
// gone) is missing data: recover refuses rather than boot on an empty
// auto-created volume of that name.
func TestRecoverRefusesLeftoverVolume(t *testing.T) {
	d := &scriptedDriver{fakeDriver: newFake()}
	d.failStart = true
	e, r := testEngine(t, d)
	readySource(t, r)
	d.addOrphanVolume("pgoverlay-src-main", r.InstanceID())
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err == nil {
		t.Fatal("want create to fail")
	}
	d.failStart = false
	d.leftovers = []runtime.VolumeInfo{{Name: "pgoverlay-br-pr-1-rw", Leftover: true}}
	starts := d.startAttempts
	_, err := e.RecoverBranch(context.Background(), "pr-1")
	if !errors.Is(err, ErrNotRecoverable) || !strings.Contains(err.Error(), "pgoverlay-br-pr-1-rw") {
		t.Fatalf("recover on a leftover directory: err=%v, want ErrNotRecoverable naming the volume", err)
	}
	if d.startAttempts != starts {
		t.Fatal("recover started a container on a leftover directory")
	}
}

// Reconcile collects a leftover directory like any orphan volume.
func TestReconcileCollectsLeftoverDirectory(t *testing.T) {
	d := &scriptedDriver{fakeDriver: newFake()}
	e, _ := testEngine(t, d)
	d.leftovers = []runtime.VolumeInfo{{Name: "pgoverlay-br-gone-rw", Leftover: true}}
	plan, err := e.PlanReconcile(context.Background(), time.Now().Add(time.Hour), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range plan.Actions {
		if a.Kind == ActionGCVolume && a.Target == "pgoverlay-br-gone-rw" {
			found = true
		}
	}
	if !found {
		t.Fatalf("reconcile plan %+v does not collect the leftover directory", plan.Actions)
	}
}
