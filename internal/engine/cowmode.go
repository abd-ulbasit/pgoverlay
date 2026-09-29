package engine

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/abd-ulbasit/pgoverlay/internal/cow"
	"github.com/abd-ulbasit/pgoverlay/internal/registry"
)

// The lazyrw shim (internal/cow/lazyrw) makes an overlay branch copy a
// relation file up on its first write instead of on its first read. branchd
// turns it on or off for every overlay branch (--lazyrw); the branch
// entrypoint decides at each start whether it can be used and records the
// outcome in cow.CowModePath, which the engine reads once the branch is
// ready, logs, and counts in pgoverlay_branch_cow_mode.

// WithLazyRW switches the lazyrw shim on (the default) or off for overlay
// branches: the entrypoint gets PGOVERLAY_LAZYRW=on|off. Branches pick a
// change up on their next start. branchd --lazyrw, pgb $PGOVERLAY_LAZYRW.
// zfs and csi branches never use it: their clones copy blocks, not files.
func WithLazyRW(on bool) Option {
	return func(e *Engine) { e.lazyrwOff = !on }
}

// WithWALRecycle(false) starts overlay branches with -c wal_recycle=off, so a
// WAL segment that came from the seed is removed instead of being renamed
// (which copies it up) when a checkpoint recycles it. Experimental; on (the
// Postgres default) unless set. branchd --wal-recycle, pgb
// $PGOVERLAY_WAL_RECYCLE.
func WithWALRecycle(on bool) Option {
	return func(e *Engine) { e.walRecycleOff = !on }
}

// CowModeUnknown is what pgoverlay_branch_cow_mode counts a ready overlay
// branch under while its mode has not been read (branchd started after it)
// or could not be read.
const CowModeUnknown = "unknown"

// CowModes lists the modes pgoverlay_branch_cow_mode reports, in order.
var CowModes = []string{cow.CowModeLazyRW, cow.CowModeEager, cow.CowModeOff, CowModeUnknown}

// cowModeLegacy is what cowModeCmd prints when the branch's rw volume has no
// cow-mode file: its entrypoint was installed by a pgoverlay without lazyrw,
// so it copies eagerly until it is reset (or recovered), which installs the
// current entrypoint.
const cowModeLegacy = "legacy"

// cowModeCmd reads the mode inside a running overlay branch container.
var cowModeCmd = []string{"sh", "-c", "cat " + cow.CowModePath + " 2>/dev/null || echo " + cowModeLegacy}

// cowModeReadTimeout bounds one read: it is a best-effort observation and must
// not hold a saga up.
const cowModeReadTimeout = 15 * time.Second

// cowModeState is the mode each overlay branch reported at its last start,
// by branch id.
type cowModeState struct {
	mu    sync.Mutex
	modes map[string]string
}

func (s *cowModeState) set(branchID, mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.modes == nil {
		s.modes = map[string]string{}
	}
	s.modes[branchID] = mode
}

func (s *cowModeState) get(branchID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.modes[branchID]
	return m, ok
}

func (s *cowModeState) forget(branchID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.modes, branchID)
}

// overlayBranchEnv is the environment an overlay branch's entrypoint reads
// besides PGDATA and PGOVERLAY_LOWERS.
func (e *Engine) overlayBranchEnv() []string {
	env := []string{"PGOVERLAY_LAZYRW=on"}
	if e.lazyrwOff {
		env[0] = "PGOVERLAY_LAZYRW=off"
	}
	if e.walRecycleOff {
		env = append(env, "PGOVERLAY_WAL_RECYCLE=off")
	}
	return env
}

// readCowMode reads the copy-on-write mode a running overlay branch started
// in, and the entrypoint's detail line (the preloaded build, or why the shim
// is not active).
func (e *Engine) readCowMode(ctx context.Context, cid string) (mode, detail string, err error) {
	ctx, cancel := context.WithTimeout(ctx, cowModeReadTimeout)
	defer cancel()
	out, err := e.drv.ExecOutput(ctx, cid, cowModeCmd)
	if err != nil {
		return CowModeUnknown, "", err
	}
	first, rest, _ := strings.Cut(strings.TrimSpace(out), "\n")
	switch first = strings.TrimSpace(first); first {
	case cow.CowModeLazyRW, cow.CowModeEager, cow.CowModeOff:
		return first, strings.TrimSpace(rest), nil
	case cowModeLegacy:
		return cow.CowModeEager, "the branch's entrypoint predates lazyrw; reset the branch to install the current one", nil
	}
	return CowModeUnknown, "", fmt.Errorf("unexpected %s content %q", cow.CowModePath, out)
}

// observeCowMode reads which copy-on-write mode a freshly started overlay
// branch runs in, logs it (a WARN when lazyrw is on but the branch copies
// eagerly) and records it for pgoverlay_branch_cow_mode. Best-effort: a
// failed read is logged and counted as unknown, never an error. No-op for
// zfs and csi branches.
func (e *Engine) observeCowMode(ctx context.Context, b *registry.Branch, cid string) {
	if e.zfs() || e.csi() {
		return
	}
	mode, detail, err := e.readCowMode(ctx, cid)
	e.cowModes.set(b.ID, mode)
	switch {
	case err != nil:
		slog.Warn("could not read the branch's copy-on-write mode", "branch", b.Name, "container", cid, "err", err)
	case mode == cow.CowModeLazyRW:
		slog.Info("branch copy-on-write mode: lazyrw (reads copy nothing; a file is copied into the branch on its first write)",
			"branch", b.Name, "shim", detail)
	case mode == cow.CowModeEager && !e.lazyrwOff:
		slog.Warn("branch copy-on-write mode: eager although lazyrw is on; every relation file Postgres opens is copied into the branch",
			"branch", b.Name, "reason", detail)
	default:
		slog.Info("branch copy-on-write mode: "+mode, "branch", b.Name, "detail", detail)
	}
}

// CowModeCounts reports how many ready overlay branches run in each
// copy-on-write mode (CowModes; a branch whose mode is not known is counted
// as unknown), for pgoverlay_branch_cow_mode. nil when the registry cannot be
// read, or on the zfs and csi backends, which have no such mode.
func (e *Engine) CowModeCounts() map[string]int {
	if e.zfs() || e.csi() {
		return nil
	}
	branches, err := e.reg.ListLiveBranches()
	if err != nil {
		return nil
	}
	counts := make(map[string]int, len(CowModes))
	for _, m := range CowModes {
		counts[m] = 0
	}
	for _, b := range branches {
		if b.State != registry.BranchReady {
			continue
		}
		mode, ok := e.cowModes.get(b.ID)
		if !ok {
			mode = CowModeUnknown
		}
		counts[mode]++
	}
	return counts
}

// RefreshCowModes reads the copy-on-write mode of every ready overlay branch
// whose mode this process has not seen yet: the branches that were already
// running when branchd started. branchd runs it once at startup, in the
// background.
func (e *Engine) RefreshCowModes(ctx context.Context) {
	if e.zfs() || e.csi() {
		return
	}
	branches, err := e.reg.ListLiveBranches()
	if err != nil {
		slog.Warn("could not list branches to read their copy-on-write modes", "err", err)
		return
	}
	for _, b := range branches {
		if ctx.Err() != nil {
			return
		}
		if b.State != registry.BranchReady || b.ContainerID == "" {
			continue
		}
		if _, ok := e.cowModes.get(b.ID); ok {
			continue
		}
		e.observeCowMode(ctx, b, b.ContainerID)
	}
}
