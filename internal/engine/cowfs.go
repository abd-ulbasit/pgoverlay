package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"

	"github.com/abd-ulbasit/pgoverlay/internal/cow"
	"github.com/abd-ulbasit/pgoverlay/internal/runtime"
)

// duTool returns the pgoverlay-du binary for a machine; tests replace it.
var duTool = cow.DuTool

// duTools returns every pgoverlay-du binary this build carries; tests
// replace it.
var duTools = cow.DuTools

// CopyUpOptions tunes DetectCopyUp.
type CopyUpOptions struct {
	// Root is the host directory the runtime creates volumes under: the
	// docker driver's volume root (branchd --volume-root) or the kube
	// hostPath data root. "" when the runtime manages volume storage itself
	// (docker named volumes). The XFS hint below is only ever set on Root.
	Root string
	// CowExtSize is the XFS copy-on-write extent size hint, in bytes, set on
	// Root when copy-up clones on XFS: new volumes inherit it, so the first
	// write to a cloned block copies this much rather than the filesystem
	// default (128 KiB with 4 KiB blocks). 0 leaves the default.
	CowExtSize int64
	// ProbeSize is the probe file's size (0 = cow.DefaultProbeSize).
	ProbeSize int64
}

// DefaultCowExtSize is branchd's default XFS copy-on-write extent size hint:
// two Postgres pages, so a random page write into a cloned segment copies
// 16 KiB instead of 128 KiB.
const DefaultCowExtSize int64 = 16 << 10

// DetectCopyUp probes what OverlayFS copy-up costs on the filesystem that
// holds this engine's volumes (cow.ProbeCopyUp), remembers the answer for
// BranchUsage and reports it through CopyUpMode. When copy-up clones on XFS
// and opts asks for it, it also sets the copy-on-write extent size hint on
// opts.Root. Overlay backend only. A failed probe leaves the mode unknown
// (usage keeps counting with du -sb) and returns the error.
func (e *Engine) DetectCopyUp(ctx context.Context, opts CopyUpOptions) (cow.ProbeResult, error) {
	if e.zfs() || e.csi() {
		return cow.ProbeResult{Mode: cow.CopyUpUnknown, SharedBytes: -1},
			fmt.Errorf("copy-up probe: the %s backend has no overlay copy-up", e.planner.Backend)
	}
	var id [4]byte
	if _, err := rand.Read(id[:]); err != nil {
		return cow.ProbeResult{Mode: cow.CopyUpUnknown, SharedBytes: -1}, err
	}
	prefix := "pgoverlay-probe-" + hex.EncodeToString(id[:])
	res, err := cow.ProbeCopyUp(ctx, e.drv, cow.ProbeSpec{
		LowerVolume: prefix + "-lower",
		UpperVolume: prefix + "-upper",
		// the instance label lets reconcile collect the probe volumes should
		// this process die before removing them
		Labels:  e.instanceLabels(map[string]string{"pgoverlay.managed": "true", "pgoverlay.role": "probe"}),
		Size:    opts.ProbeSize,
		DuTools: duTools(),
	})
	if err != nil && res.Mode == cow.CopyUpUnknown {
		return res, err
	}
	// a probe that measured but could not clean up still measured
	e.copyUp.Store(&res)
	if res.Mode == cow.CopyUpClone && duTool(res.Machine) == nil {
		slog.Warn("copy-up clones extents on this filesystem, but this build carries no pgoverlay-du for the host's architecture: branch usage falls back to du -sb, which counts cloned extents as the branch's own",
			"machine", res.Machine, "fs", res.FSType)
	}
	if hintErr := e.setCowExtSize(ctx, res, opts); hintErr != nil {
		err = errors.Join(err, hintErr)
	}
	return res, err
}

// setCowExtSize sets the XFS copy-on-write extent size hint on opts.Root
// when it applies: copy-up clones, the filesystem is XFS, and a hint and a
// root were given. Volumes created under the root afterwards inherit it.
func (e *Engine) setCowExtSize(ctx context.Context, res cow.ProbeResult, opts CopyUpOptions) error {
	if opts.Root == "" || opts.CowExtSize <= 0 || res.Mode != cow.CopyUpClone || res.FSType != "xfs" {
		return nil
	}
	bin := duTool(res.Machine)
	if bin == nil {
		slog.Info("not setting the XFS copy-on-write extent size hint: no pgoverlay-du for the host's architecture", "machine", res.Machine, "root", opts.Root)
		return nil
	}
	const target = "/pgoverlay-root"
	cmd, env := cow.DuCowExtSizeCommand(bin, target, opts.CowExtSize)
	out, err := e.drv.RunHelper(ctx, runtime.HelperSpec{
		Image:  runtime.UtilityImage,
		Cmd:    cmd,
		Env:    env,
		Mounts: []runtime.Mount{{Kind: runtime.MountHostPath, Volume: opts.Root, Target: target}},
	})
	if err != nil {
		return fmt.Errorf("set XFS cowextsize hint on %s: %w", opts.Root, err)
	}
	got, err := cow.ParseCowExtSize(out)
	if err != nil {
		return fmt.Errorf("set XFS cowextsize hint on %s: %w", opts.Root, err)
	}
	slog.Info("set the XFS copy-on-write extent size hint on the volume root; volumes created from now on inherit it",
		"root", opts.Root, "cowextsize", got)
	return nil
}

// CopyUpMode is the copy-up mode DetectCopyUp found: unknown before it ran,
// or when it failed.
func (e *Engine) CopyUpMode() cow.CopyUpMode {
	if r := e.copyUp.Load(); r != nil {
		return r.Mode
	}
	return cow.CopyUpUnknown
}

// usageHelpers returns the helpers that measure a branch's writable layer, in
// the order to try them. Overlay and csi measure the volume: with
// pgoverlay-du's exclusive bytes when copy-up clones (du counts a cloned file
// in full, although it shares all but its rewritten blocks with the source),
// otherwise with du -sb, which is exact when copy-up copies; du -sb also backs
// pgoverlay-du up. zfs asks for the clone's `used`.
func (e *Engine) usageHelpers(rwVolume string) []runtime.HelperSpec {
	if e.zfs() {
		return []runtime.HelperSpec{zfsHelperSpec(e.planner.ZFSUsed(rwVolume))}
	}
	mounts := []runtime.Mount{{Volume: rwVolume, Target: cow.RWPath, ReadOnly: true}}
	du := runtime.HelperSpec{Image: runtime.UtilityImage, Cmd: []string{"du", "-sb", cow.RWPath}, Mounts: mounts}
	if r := e.copyUp.Load(); r != nil && r.Mode == cow.CopyUpClone {
		if bin := duTool(r.Machine); bin != nil {
			cmd, env := cow.DuUsageCommand(bin, cow.RWPath)
			return []runtime.HelperSpec{{Image: runtime.UtilityImage, Cmd: cmd, Env: env, Mounts: mounts}, du}
		}
	}
	return []runtime.HelperSpec{du}
}
