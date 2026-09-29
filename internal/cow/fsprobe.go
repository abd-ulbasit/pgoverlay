package cow

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/abd-ulbasit/pgoverlay/internal/runtime"
)

// CopyUpMode is what an OverlayFS copy-up costs on the filesystem that holds
// the branch volumes. A branch's first write to a source file copies that file
// into its writable layer; whether that is a copy or an extent clone is up to
// the filesystem, and it decides how BranchUsage has to count.
type CopyUpMode string

const (
	// CopyUpUnknown: not probed (yet), or the probe failed.
	CopyUpUnknown CopyUpMode = "unknown"
	// CopyUpClone: copy-up clones extents (XFS with reflink=1, btrfs). It
	// takes milliseconds and no space; the clone and the source then share
	// blocks until one is rewritten, which is block-level copy-on-write.
	// `du` counts shared blocks as the branch's own, so usage is measured
	// with pgoverlay-du instead.
	CopyUpClone CopyUpMode = "clone"
	// CopyUpCopy: copy-up copies the file's data (ext4, XFS without
	// reflink, most others). `du -sb` is accurate.
	CopyUpCopy CopyUpMode = "copy"
)

// CopyUpModes lists every mode (the label values of
// pgoverlay_cow_copyup_mode).
var CopyUpModes = []CopyUpMode{CopyUpClone, CopyUpCopy, CopyUpUnknown}

// DefaultProbeSize is how much data the copy-up probe writes and copies up:
// large enough that a real copy stands out from other writers on a shared
// filesystem, small enough to take well under a second where it is copied.
const DefaultProbeSize int64 = 64 << 20

// ProbeSpec describes one copy-up probe.
type ProbeSpec struct {
	// LowerVolume and UpperVolume name two empty volumes ProbeCopyUp creates
	// (with Labels) and removes again. They land wherever the runtime puts
	// every other volume, which is the filesystem being probed.
	LowerVolume, UpperVolume string
	Labels                   map[string]string
	// Size is the probe file's size in bytes, rounded up to whole MiB
	// (0 = DefaultProbeSize).
	Size int64
	// DuTools are pgoverlay-du binaries by machine name (DuTools()); the
	// helper runs the one matching its own architecture, if any, to read the
	// copied-up file's shared extents directly. nil probes by free space
	// alone.
	DuTools map[string][]byte
}

// ProbeResult is what one copy-up probe measured.
type ProbeResult struct {
	Mode CopyUpMode
	// FSType names the filesystem holding the volumes ("xfs", "btrfs",
	// "ext4", ...; the statfs magic in hex when it is not a known one).
	FSType string
	// Machine is the helper's `uname -m`: the architecture of the host (or
	// node) that runs branches, which picks the pgoverlay-du binary.
	Machine string
	// ProbeBytes is the size of the file copied up.
	ProbeBytes int64
	// UsedBytes is how much free space the filesystem lost across the
	// copy-up (other writers on the same filesystem add noise).
	UsedBytes int64
	// SharedBytes is how much of the copied-up file shares extents with
	// the original according to FIEMAP; -1 when not measured (no
	// pgoverlay-du binary for Machine).
	SharedBytes int64
	// CopyUpTime is how long the O_RDWR open that triggered the copy-up
	// took (10 ms resolution).
	CopyUpTime time.Duration
}

// String renders the result for logs.
func (r ProbeResult) String() string {
	shared := "n/a"
	if r.SharedBytes >= 0 {
		shared = strconv.FormatInt(r.SharedBytes, 10)
	}
	return fmt.Sprintf("mode=%s fs=%s machine=%s probe_bytes=%d used_bytes=%d shared_bytes=%s copyup_time=%s",
		r.Mode, r.FSType, r.Machine, r.ProbeBytes, r.UsedBytes, shared, r.CopyUpTime)
}

// probeScript runs as root with CAP_SYS_ADMIN in the utility image, the lower
// probe volume at /pgoverlay-probe/lower and the upper one at
// /pgoverlay-probe/upper. It writes $1 MiB of random data into the lower
// volume, mounts an overlay the way a branch container does (lower volume as
// lowerdir, upper volume holding upperdir and workdir), opens the file O_RDWR
// through the overlay (which copies it up) and reports the filesystem's free
// space before and after, the time the open took, and, when a pgoverlay-du
// for this machine came along, the copied-up file's shared extents. The
// %s placeholder is replaced with the shell that selects that binary.
const probeScript = `set -eu
lo=/pgoverlay-probe/lower
up=/pgoverlay-probe/upper
m=/pgoverlay-probe-merged
mb=$1
machine=$(uname -m)
echo "pgoverlay-probe-machine=$machine"
echo "pgoverlay-probe-fsmagic=$(stat -f -c %%t "$up")"
mkdir -p "$lo/d" "$up/u" "$up/w" "$m"
dd if=/dev/urandom of="$lo/d/f" bs=1048576 count="$mb" 2>/dev/null
sync -f "$lo/d/f" 2>/dev/null || sync
mount -t overlay overlay -o "lowerdir=$lo/d,upperdir=$up/u,workdir=$up/w" "$m"
trap 'umount "$m" 2>/dev/null || true' EXIT
bsize=$(stat -f -c %%S "$up")
free0=$(stat -f -c %%f "$up")
t0=$(cut -d' ' -f1 /proc/uptime)
exec 3<>"$m/f"
exec 3>&-
t1=$(cut -d' ' -f1 /proc/uptime)
sync -f "$up/u/f" 2>/dev/null || sync
free1=$(stat -f -c %%f "$up")
echo "pgoverlay-probe-bytes=$((mb * 1048576))"
echo "pgoverlay-probe-used=$(((free0 - free1) * bsize))"
echo "pgoverlay-probe-open=$t0 $t1"
%s
if du_b64 > /tmp/pgoverlay-du.b64 2>/dev/null && [ -s /tmp/pgoverlay-du.b64 ]; then
  base64 -d < /tmp/pgoverlay-du.b64 > /tmp/pgoverlay-du
  chmod 0755 /tmp/pgoverlay-du
  echo "pgoverlay-probe-fiemap=$(/tmp/pgoverlay-du -- "$up/u/f")"
fi
`

// probeDuSelect renders the shell function du_b64, which prints the base64 of
// the pgoverlay-du binary for the helper's machine (or fails when there is
// none), and the environment carrying those binaries.
func probeDuSelect(tools map[string][]byte) (shell string, env []string) {
	machines := make([]string, 0, len(tools))
	for m := range tools {
		machines = append(machines, m)
	}
	sort.Strings(machines)
	var b strings.Builder
	b.WriteString("case \"$machine\" in\n")
	for _, m := range machines {
		norm := NormalizeMachine(m)
		if norm == "" || len(tools[m]) == 0 {
			continue
		}
		e, words := duToolEnv("PGOVERLAY_DU_"+strings.ToUpper(norm)+"_", tools[m])
		env = append(env, e...)
		pattern := norm
		if norm == "aarch64" {
			pattern = "aarch64|arm64|armv8l"
		}
		fmt.Fprintf(&b, "%s) du_b64() { printf '%%s' %s; } ;;\n", pattern, words)
	}
	b.WriteString("*) du_b64() { return 1; } ;;\nesac")
	return b.String(), env
}

// ProbeHelper returns the helper that runs the copy-up probe on spec's
// volumes.
func ProbeHelper(spec ProbeSpec) runtime.HelperSpec {
	size := spec.Size
	if size <= 0 {
		size = DefaultProbeSize
	}
	mib := (size + (1<<20 - 1)) >> 20
	sel, env := probeDuSelect(spec.DuTools)
	return runtime.HelperSpec{
		Image:    runtime.UtilityImage,
		Cmd:      []string{"sh", "-c", fmt.Sprintf(probeScript, sel), "pgoverlay-probe", strconv.FormatInt(mib, 10)},
		Env:      env,
		SysAdmin: true,
		Mounts: []runtime.Mount{
			{Volume: spec.LowerVolume, Target: "/pgoverlay-probe/lower"},
			{Volume: spec.UpperVolume, Target: "/pgoverlay-probe/upper"},
		},
	}
}

// ProbeCopyUp measures what OverlayFS copy-up costs where the runtime keeps
// its volumes: it creates spec's two volumes, copies a file up through an
// overlay across them in a helper with the privileges a branch container has
// (CAP_SYS_ADMIN, to mount), classifies the result and removes the volumes
// again. It is a startup probe: a few seconds, 64 MiB of temporary data.
func ProbeCopyUp(ctx context.Context, drv runtime.Driver, spec ProbeSpec) (res ProbeResult, err error) {
	if spec.LowerVolume == "" || spec.UpperVolume == "" || spec.LowerVolume == spec.UpperVolume {
		return ProbeResult{Mode: CopyUpUnknown, SharedBytes: -1}, errors.New("copy-up probe: need two distinct volume names")
	}
	bg := context.WithoutCancel(ctx)
	var created []string
	defer func() {
		for _, v := range created {
			if rmErr := drv.RemoveVolume(bg, v); rmErr != nil && err == nil {
				err = fmt.Errorf("copy-up probe: remove probe volume %s: %w", v, rmErr)
			}
		}
	}()
	for _, v := range []string{spec.LowerVolume, spec.UpperVolume} {
		if err := drv.CreateVolume(ctx, v, spec.Labels); err != nil {
			return ProbeResult{Mode: CopyUpUnknown, SharedBytes: -1}, fmt.Errorf("copy-up probe: create volume %s: %w", v, err)
		}
		created = append(created, v)
	}
	out, err := drv.RunHelper(ctx, ProbeHelper(spec))
	if err != nil {
		return ProbeResult{Mode: CopyUpUnknown, SharedBytes: -1}, fmt.Errorf("copy-up probe: %w", err)
	}
	res, err = ParseProbe(out)
	if err != nil {
		return res, fmt.Errorf("copy-up probe: %w", err)
	}
	return res, nil
}

// ParseProbe reads probeScript's report and classifies it.
func ParseProbe(out string) (ProbeResult, error) {
	r := ProbeResult{Mode: CopyUpUnknown, SharedBytes: -1}
	var haveBytes, haveUsed bool
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "pgoverlay-probe-machine":
			r.Machine = strings.TrimSpace(v)
		case "pgoverlay-probe-fsmagic":
			r.FSType = fsTypeName(v)
		case "pgoverlay-probe-bytes":
			n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			haveBytes = err == nil
			r.ProbeBytes = n
		case "pgoverlay-probe-used":
			n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			haveUsed = err == nil
			r.UsedBytes = n
		case "pgoverlay-probe-open":
			if f := strings.Fields(v); len(f) == 2 {
				t0, err0 := strconv.ParseFloat(f[0], 64)
				t1, err1 := strconv.ParseFloat(f[1], 64)
				if err0 == nil && err1 == nil && t1 >= t0 {
					r.CopyUpTime = time.Duration((t1 - t0) * float64(time.Second)).Round(10 * time.Millisecond)
				}
			}
		case "pgoverlay-probe-fiemap":
			if t, err := ParseDuTotals(v); err == nil {
				r.SharedBytes = t.Shared
			}
		}
	}
	if !haveBytes || !haveUsed || r.ProbeBytes <= 0 {
		return r, fmt.Errorf("unrecognized probe output %q", out)
	}
	r.Mode = classify(r)
	return r, nil
}

// classify decides the copy-up mode. FIEMAP is exact: a clone shares (nearly)
// all of the copied-up file's extents with the original, a copy none. Without
// it the free-space drop decides: a copy costs the file's size, a clone about
// nothing, and the threshold at half the file leaves room for other writers
// on a shared filesystem. A misjudgement costs accuracy, not correctness:
// usage in clone mode is counted with pgoverlay-du, which is exact on either
// kind of filesystem, and in copy mode with du -sb, which over-counts clones.
func classify(r ProbeResult) CopyUpMode {
	if r.SharedBytes >= 0 {
		if r.SharedBytes >= r.ProbeBytes/2 {
			return CopyUpClone
		}
		return CopyUpCopy
	}
	if r.UsedBytes < r.ProbeBytes/2 {
		return CopyUpClone
	}
	return CopyUpCopy
}

// fsMagics names the statfs f_type values pgoverlay meets (linux/magic.h).
var fsMagics = map[uint64]string{
	0x58465342: "xfs",
	0x9123683e: "btrfs",
	0xef53:     "ext4", // ext2, ext3 and ext4 share the magic
	0x2fc12fc1: "zfs",
	0x01021994: "tmpfs",
	0x794c7630: "overlay",
	0x6969:     "nfs",
	0xf2f52010: "f2fs",
	0x65735546: "fuse",
	0xca451a4e: "bcachefs",
}

func fsTypeName(hexMagic string) string {
	s := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(hexMagic)), "0x")
	n, err := strconv.ParseUint(s, 16, 64)
	if err != nil {
		return strings.TrimSpace(hexMagic)
	}
	if name, ok := fsMagics[n]; ok {
		return name
	}
	return "0x" + s
}
