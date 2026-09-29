package cow

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/pgoverlay/internal/runtime"
)

func TestParseProbeClassifies(t *testing.T) {
	const base = "pgoverlay-probe-machine=x86_64\npgoverlay-probe-bytes=67108864\npgoverlay-probe-open=1234.50 1234.51\n"
	cases := []struct {
		name   string
		out    string
		mode   CopyUpMode
		fs     string
		shared int64
	}{
		{"xfs clone, FIEMAP", base + "pgoverlay-probe-fsmagic=58465342\npgoverlay-probe-used=0\npgoverlay-probe-fiemap=0\t67108864\t67108864\t/pgoverlay-probe/upper/u/f\n", CopyUpClone, "xfs", 64 << 20},
		// FIEMAP wins over a noisy free-space reading either way
		{"FIEMAP clone despite noise", base + "pgoverlay-probe-fsmagic=9123683e\npgoverlay-probe-used=60000000\npgoverlay-probe-fiemap=4096\t67104768\t67108864\t/f\n", CopyUpClone, "btrfs", 67104768},
		{"FIEMAP copy despite noise", base + "pgoverlay-probe-fsmagic=ef53\npgoverlay-probe-used=-5000000\npgoverlay-probe-fiemap=67108864\t0\t67108864\t/f\n", CopyUpCopy, "ext4", 0},
		{"ext4 copy, free space", base + "pgoverlay-probe-fsmagic=ef53\npgoverlay-probe-used=70291456\n", CopyUpCopy, "ext4", -1},
		{"btrfs clone, free space", base + "pgoverlay-probe-fsmagic=9123683e\npgoverlay-probe-used=16384\n", CopyUpClone, "btrfs", -1},
		{"freed space reads as clone", base + "pgoverlay-probe-fsmagic=58465342\npgoverlay-probe-used=-4096\n", CopyUpClone, "xfs", -1},
		{"unknown magic", base + "pgoverlay-probe-fsmagic=abcdef01\npgoverlay-probe-used=67108864\n", CopyUpCopy, "0xabcdef01", -1},
		{"unparseable FIEMAP line falls back to free space", base + "pgoverlay-probe-fsmagic=ef53\npgoverlay-probe-used=67108864\npgoverlay-probe-fiemap=\n", CopyUpCopy, "ext4", -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := ParseProbe(tc.out)
			if err != nil {
				t.Fatal(err)
			}
			if r.Mode != tc.mode || r.FSType != tc.fs || r.SharedBytes != tc.shared || r.Machine != "x86_64" || r.ProbeBytes != 64<<20 {
				t.Fatalf("got %s", r)
			}
			if r.CopyUpTime != 10*time.Millisecond {
				t.Fatalf("copy-up time %s, want 10ms", r.CopyUpTime)
			}
		})
	}
}

func TestParseProbeRejectsIncompleteOutput(t *testing.T) {
	for _, out := range []string{
		"",
		"mount: permission denied",
		"pgoverlay-probe-bytes=67108864\n", // no free-space reading
		"pgoverlay-probe-used=0\n",         // no size
		"pgoverlay-probe-bytes=0\npgoverlay-probe-used=0\n",
	} {
		r, err := ParseProbe(out)
		if err == nil || r.Mode != CopyUpUnknown {
			t.Errorf("ParseProbe(%q) = %s, %v; want an error and mode unknown", out, r, err)
		}
	}
}

func TestProbeHelperSpec(t *testing.T) {
	spec := ProbeHelper(ProbeSpec{
		LowerVolume: "lo", UpperVolume: "up", Size: 3<<20 + 1,
		DuTools: map[string][]byte{"x86_64": []byte("x86 tool"), "aarch64": []byte("arm tool")},
	})
	if !spec.SysAdmin || spec.Privileged || spec.Image != runtime.UtilityImage {
		t.Fatalf("probe helper privileges/image: %+v", spec)
	}
	if len(spec.Mounts) != 2 || spec.Mounts[0].Volume != "lo" || spec.Mounts[1].Volume != "up" ||
		spec.Mounts[0].ReadOnly || spec.Mounts[1].ReadOnly {
		t.Fatalf("probe mounts %+v", spec.Mounts)
	}
	if len(spec.Cmd) != 5 || spec.Cmd[3] != "pgoverlay-probe" || spec.Cmd[4] != "4" {
		t.Fatalf("probe cmd %q: want the size rounded up to 4 MiB", spec.Cmd)
	}
	env := strings.Join(spec.Env, "\n")
	for _, want := range []string{"PGOVERLAY_DU_X86_64_0=", "PGOVERLAY_DU_AARCH64_0="} {
		if !strings.Contains(env, want) {
			t.Fatalf("probe env lacks %s: %v", want, spec.Env)
		}
	}
	script := spec.Cmd[2]
	for _, want := range []string{`x86_64) du_b64()`, `aarch64|arm64|armv8l) du_b64()`, "stat -f -c %t", "stat -f -c %f", "exec 3<>"} {
		if !strings.Contains(script, want) {
			t.Fatalf("probe script lacks %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "%!") || strings.Contains(script, "%%") {
		t.Fatalf("probe script has formatting residue:\n%s", script)
	}
	if err := exec.Command("sh", "-n", "-c", script).Run(); err != nil {
		t.Fatalf("probe script does not parse: %v\n%s", err, script)
	}
	// without binaries the script still parses and simply skips FIEMAP
	bare := ProbeHelper(ProbeSpec{LowerVolume: "lo", UpperVolume: "up"})
	if len(bare.Env) != 0 || bare.Cmd[4] != "64" {
		t.Fatalf("bare probe helper %+v", bare)
	}
	if err := exec.Command("sh", "-n", "-c", bare.Cmd[2]).Run(); err != nil {
		t.Fatalf("bare probe script does not parse: %v", err)
	}
}

// probeDriver records volume and helper calls for ProbeCopyUp.
type probeDriver struct {
	runtime.Driver   // unused methods panic
	created, removed []string
	labels           map[string]map[string]string
	createErr        error
	removeErr        error
	helperOut        string
	helperErr        error
	helpers          []runtime.HelperSpec
}

func (d *probeDriver) CreateVolume(_ context.Context, name string, l map[string]string) error {
	if d.createErr != nil && len(d.created) == 1 {
		return d.createErr
	}
	d.created = append(d.created, name)
	if d.labels == nil {
		d.labels = map[string]map[string]string{}
	}
	d.labels[name] = l
	return nil
}

func (d *probeDriver) RemoveVolume(_ context.Context, name string) error {
	d.removed = append(d.removed, name)
	return d.removeErr
}

func (d *probeDriver) RunHelper(_ context.Context, s runtime.HelperSpec) (string, error) {
	d.helpers = append(d.helpers, s)
	return d.helperOut, d.helperErr
}

const cloneOut = "pgoverlay-probe-machine=aarch64\npgoverlay-probe-fsmagic=58465342\npgoverlay-probe-bytes=67108864\npgoverlay-probe-used=8192\n"

func TestProbeCopyUpCreatesProbesAndRemoves(t *testing.T) {
	d := &probeDriver{helperOut: cloneOut}
	labels := map[string]string{"pgoverlay.managed": "true"}
	r, err := ProbeCopyUp(context.Background(), d, ProbeSpec{LowerVolume: "p-lower", UpperVolume: "p-upper", Labels: labels})
	if err != nil {
		t.Fatal(err)
	}
	if r.Mode != CopyUpClone || r.Machine != "aarch64" {
		t.Fatalf("result %s", r)
	}
	if strings.Join(d.created, ",") != "p-lower,p-upper" || strings.Join(d.removed, ",") != "p-lower,p-upper" {
		t.Fatalf("created %v removed %v", d.created, d.removed)
	}
	if d.labels["p-lower"]["pgoverlay.managed"] != "true" {
		t.Fatalf("labels %v", d.labels)
	}
	if len(d.helpers) != 1 || !d.helpers[0].SysAdmin {
		t.Fatalf("helpers %+v", d.helpers)
	}
}

func TestProbeCopyUpCleansUpOnFailure(t *testing.T) {
	// the helper fails: both volumes still go
	d := &probeDriver{helperErr: errors.New("helper exited 32: mount: permission denied")}
	r, err := ProbeCopyUp(context.Background(), d, ProbeSpec{LowerVolume: "a", UpperVolume: "b"})
	if err == nil || r.Mode != CopyUpUnknown {
		t.Fatalf("got %s, %v", r, err)
	}
	if strings.Join(d.removed, ",") != "a,b" {
		t.Fatalf("removed %v", d.removed)
	}

	// the second create fails: the first volume goes
	d = &probeDriver{createErr: errors.New("disk full")}
	if _, err := ProbeCopyUp(context.Background(), d, ProbeSpec{LowerVolume: "a", UpperVolume: "b"}); err == nil {
		t.Fatal("want the create error")
	}
	if strings.Join(d.removed, ",") != "a" || len(d.helpers) != 0 {
		t.Fatalf("removed %v, helpers %d", d.removed, len(d.helpers))
	}

	// a measurement whose cleanup failed keeps its result and reports it
	d = &probeDriver{helperOut: cloneOut, removeErr: errors.New("volume is in use")}
	r, err = ProbeCopyUp(context.Background(), d, ProbeSpec{LowerVolume: "a", UpperVolume: "b"})
	if err == nil || r.Mode != CopyUpClone {
		t.Fatalf("got %s, %v; want the result and the cleanup error", r, err)
	}

	// the same name twice is refused before anything is created
	d = &probeDriver{}
	if _, err := ProbeCopyUp(context.Background(), d, ProbeSpec{LowerVolume: "a", UpperVolume: "a"}); err == nil || len(d.created) != 0 {
		t.Fatalf("err %v created %v", err, d.created)
	}
}

func TestFSTypeName(t *testing.T) {
	for in, want := range map[string]string{
		"58465342": "xfs", "0x9123683E": "btrfs", "ef53": "ext4", "2fc12fc1": "zfs",
		"794c7630": "overlay", "1234": "0x1234", "zz": "zz",
	} {
		if got := fsTypeName(in); got != want {
			t.Errorf("fsTypeName(%q) = %q, want %q", in, got, want)
		}
	}
}
