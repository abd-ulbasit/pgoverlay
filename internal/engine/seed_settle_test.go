package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/abd-ulbasit/pgoverlay/internal/pgctl"
	"github.com/abd-ulbasit/pgoverlay/internal/registry"
	"github.com/abd-ulbasit/pgoverlay/internal/runtime"
)

func settleEnv(h runtime.HelperSpec) string {
	for _, kv := range h.Env {
		if v, ok := strings.CutPrefix(kv, "PGB_SETTLE="); ok {
			return v
		}
	}
	return "<unset>"
}

// settleHelperIndex finds the settle helper (its script's $0 is
// pgoverlay-settle); -1 if none ran.
func settleHelperIndex(helpers []runtime.HelperSpec) int {
	for i, h := range helpers {
		if slices.Contains(h.Cmd, "pgoverlay-settle") {
			return i
		}
	}
	return -1
}

func basebackupSource(name string) *registry.Source {
	return &registry.Source{Name: name, PGVersion: "17", ConnHost: "db", ConnPort: 5432, ConnUser: "replicator"}
}

// A pg_basebackup seed is settled right after the copy, into the same layer,
// with the engine's --seed-settle mode; off skips the step.
func TestAddSourceSettlesBasebackupSeed(t *testing.T) {
	for _, tc := range []struct {
		opts []Option
		want string // PGB_SETTLE of the settle helper; "" = no settle helper
	}{
		{nil, "freeze"},
		{[]Option{WithSeedSettle(pgctl.SettleFreeze)}, "freeze"},
		{[]Option{WithSeedSettle(pgctl.SettleRecover)}, "recover"},
		{[]Option{WithSeedSettle(pgctl.SettleOff)}, ""},
	} {
		d := newFake()
		e, _ := testEngine(t, d, tc.opts...)
		s := basebackupSource("main")
		if err := e.AddSource(context.Background(), s, "pw"); err != nil {
			t.Fatal(err)
		}
		basebackup := helperIndex(d.helpers, "pg_basebackup")
		settle := settleHelperIndex(d.helpers)
		if tc.want == "" {
			if settle >= 0 {
				t.Errorf("settle off: a settle helper ran: %q", helperCmd(d.helpers[settle]))
			}
			continue
		}
		if basebackup < 0 || settle < basebackup {
			t.Fatalf("mode %s: settle helper %d, pg_basebackup helper %d: want settle after the copy", tc.want, settle, basebackup)
		}
		h := d.helpers[settle]
		if got := settleEnv(h); got != tc.want {
			t.Errorf("settle mode = %q, want %q", got, tc.want)
		}
		if h.Image != "postgres:17" || h.User != "postgres" {
			t.Errorf("settle helper image/user = %q/%q, want the source's image as postgres", h.Image, h.User)
		}
		if len(h.Mounts) != 1 || h.Mounts[0].Volume != s.Volume || h.Mounts[0].Target != "/seed" {
			t.Errorf("settle helper mounts = %+v, want the new layer %s at /seed", h.Mounts, s.Volume)
		}
	}
}

// A refresh settles the new generation, not the one live branches use.
func TestRefreshSourceSettlesNewGeneration(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	if err := e.RefreshSource(context.Background(), "main", "pw"); err != nil {
		t.Fatal(err)
	}
	src, err := r.GetSourceByName("main")
	if err != nil {
		t.Fatal(err)
	}
	i := settleHelperIndex(d.helpers)
	if i < 0 {
		t.Fatal("refresh ran no settle helper")
	}
	if m := d.helpers[i].Mounts; len(m) != 1 || m[0].Volume != src.Volume || src.Volume == "pgoverlay-src-main" {
		t.Fatalf("settle mounted %+v; the new generation is %s", m, src.Volume)
	}
}

// A dump seed settles inside its own helper (the mode travels as
// PGB_SETTLE); there is no separate settle step.
func TestAddSourceDumpSettlesInPlace(t *testing.T) {
	for _, mode := range []pgctl.SettleMode{pgctl.SettleFreeze, pgctl.SettleOff} {
		d := newFake()
		e, _ := testEngine(t, d, WithSeedSettle(mode))
		s := basebackupSource("dumped")
		s.SeedVia = registry.SeedViaDump
		if err := e.AddSource(context.Background(), s, "pw"); err != nil {
			t.Fatal(err)
		}
		if i := settleHelperIndex(d.helpers); i >= 0 {
			t.Errorf("%s: a separate settle helper ran for a dump seed", mode)
		}
		i := helperIndex(d.helpers, "pg_dump")
		if i < 0 {
			t.Fatal("no dump helper")
		}
		if got := settleEnv(d.helpers[i]); got != string(mode) {
			t.Errorf("dump helper PGB_SETTLE = %q, want %q", got, mode)
		}
	}
}

// settleFailDriver fails only the settle helper.
type settleFailDriver struct {
	*fakeDriver
	err error
}

func (f *settleFailDriver) RunHelper(ctx context.Context, s runtime.HelperSpec) (string, error) {
	out, err := f.fakeDriver.RunHelper(ctx, s)
	if slices.Contains(s.Cmd, "pgoverlay-settle") {
		return "postgres did not start on the seeded data directory", f.err
	}
	return out, err
}

// A seed that cannot be settled is not used: the source is failed with the
// reason, the half-settled layer removed, and the error is a seed failure
// (the API answers 422: the source's configuration is the usual cause).
func TestAddSourceSettleFailureFailsTheSeed(t *testing.T) {
	d := &settleFailDriver{fakeDriver: newFake(),
		err: errors.New(`helper exited 1: FATAL:  configuration file "/seed/data/postgresql.conf" contains errors`)}
	e, r := testEngine(t, d)
	s := basebackupSource("main")
	err := e.AddSource(context.Background(), s, "pw")
	if !errors.Is(err, ErrSeedFailed) || !strings.Contains(err.Error(), "--seed-settle=freeze") {
		t.Fatalf("err = %v, want a seed failure naming --seed-settle", err)
	}
	src, gerr := r.GetSourceByName("main")
	if gerr != nil {
		t.Fatal(gerr)
	}
	if src.State != registry.SourceFailed {
		t.Errorf("source state = %s, want failed", src.State)
	}
	if d.volumes[s.Volume] {
		t.Errorf("layer %s kept after the settle failed", s.Volume)
	}
}

// ownerDriver answers the image's postgres user lookup like an Alpine image.
type ownerDriver struct {
	*fakeDriver
	owner string
}

func (f *ownerDriver) RunHelper(ctx context.Context, s runtime.HelperSpec) (string, error) {
	out, err := f.fakeDriver.RunHelper(ctx, s)
	if len(s.Cmd) == 3 && strings.Contains(s.Cmd[2], "id -u postgres") {
		return "pgoverlay-owner=" + f.owner + "\n", nil
	}
	return out, err
}

// The seed belongs to the image's own postgres user: the engine looks it up
// in the source's image first, then chowns the layer to it and runs every
// seed helper (copy, fixup, settle; the dump) as it.
func TestSeedRunsAsTheImagesPostgresUser(t *testing.T) {
	for _, via := range []string{registry.SeedViaBasebackup, registry.SeedViaDump} {
		d := &ownerDriver{fakeDriver: newFake(), owner: "70:70"}
		e, _ := testEngine(t, d)
		s := basebackupSource("alpine")
		s.Image, s.SeedVia = "postgres:17-alpine", via
		if err := e.AddSource(context.Background(), s, "pw"); err != nil {
			t.Fatal(err)
		}
		if len(d.helpers) < 3 || !strings.Contains(helperCmd(d.helpers[0]), "id -u postgres") || d.helpers[0].Image != "postgres:17-alpine" {
			t.Fatalf("%s: the first helper is not the owner lookup in the source's image: %+v", via, d.helpers)
		}
		var ran []string
		for _, h := range d.helpers[1:] {
			cmd := helperCmd(h)
			switch {
			case strings.Contains(cmd, "chown"):
				if !strings.HasSuffix(cmd, "chown 70:70 /seed") || h.User != "" {
					t.Errorf("%s: prepare = %q as %q", via, cmd, h.User)
				}
			case h.Image == "postgres:17-alpine":
				ran = append(ran, h.Cmd[0])
				if h.User != "70:70" {
					t.Errorf("%s: %q runs as %q, want 70:70", via, cmd, h.User)
				}
			}
		}
		want := map[string]int{registry.SeedViaBasebackup: 3, registry.SeedViaDump: 1}[via]
		if len(ran) != want {
			t.Errorf("%s: seed helpers in the image = %v, want %d", via, ran, want)
		}
	}
}
