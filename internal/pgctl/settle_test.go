package pgctl

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/abd-ulbasit/pgoverlay/internal/runtime"
)

func TestParseSettleMode(t *testing.T) {
	for in, want := range map[string]SettleMode{
		"":         SettleFreeze,
		"freeze":   SettleFreeze,
		" Freeze ": SettleFreeze,
		"RECOVER":  SettleRecover,
		"off":      SettleOff,
	} {
		got, err := ParseSettleMode(in)
		if err != nil || got != want {
			t.Errorf("ParseSettleMode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"on", "true", "vacuum", "frozen"} {
		if _, err := ParseSettleMode(in); err == nil || !strings.Contains(err.Error(), "want freeze, recover or off") {
			t.Errorf("ParseSettleMode(%q) err = %v, want the valid modes named", in, err)
		}
	}
	if DefaultSettleMode != SettleFreeze {
		t.Fatalf("default settle mode %q, want freeze", DefaultSettleMode)
	}
}

func settleSpec(mode SettleMode) SeedSpec {
	return SeedSpec{
		Image: "postgres:17", Volume: "pgoverlay-src-main-g2", Network: "net1",
		Host: "db.internal", Port: 5432, User: "replicator", Password: "s3cret",
		Settle: mode,
	}
}

func TestSettleHelperSpec(t *testing.T) {
	for _, mode := range []SettleMode{"", SettleFreeze, SettleRecover} {
		d := &recordingDriver{}
		if err := Settle(context.Background(), d, settleSpec(mode)); err != nil {
			t.Fatal(err)
		}
		if len(d.helpers) != 1 {
			t.Fatalf("mode %q: helpers=%d want 1", mode, len(d.helpers))
		}
		h := d.helpers[0]
		// the branch image, as the in-image postgres user, so the binaries
		// match the cluster and file ownership stays uid 999
		if h.Image != "postgres:17" || h.User != "postgres" || h.Privileged {
			t.Fatalf("mode %q: helper image/user/privileged = %q/%q/%v", mode, h.Image, h.User, h.Privileged)
		}
		if len(h.Mounts) != 1 || h.Mounts[0].Volume != "pgoverlay-src-main-g2" || h.Mounts[0].Target != "/seed" || h.Mounts[0].ReadOnly {
			t.Fatalf("mode %q: mounts = %+v, want the seed volume read-write at /seed", mode, h.Mounts)
		}
		// no network: the helper talks to nobody but its own server
		if h.Network != "" {
			t.Errorf("mode %q: network = %q, want none", mode, h.Network)
		}
		if len(h.Cmd) != 5 || h.Cmd[0] != "sh" || h.Cmd[1] != "-c" || h.Cmd[2] != settleScript || h.Cmd[4] != "/seed/data" {
			t.Fatalf("mode %q: cmd = %q", mode, h.Cmd)
		}
		want := mode
		if want == "" {
			want = SettleFreeze
		}
		env := strings.Join(h.Env, "\n")
		for _, kv := range []string{"PGB_SETTLE=" + string(want), "PGB_USER=replicator", "PGCTLTIMEOUT=86400"} {
			if !strings.Contains(env, kv) {
				t.Errorf("mode %q: env missing %q: %v", mode, kv, h.Env)
			}
		}
		// the source password is never needed: the helper's own server
		// trusts its private socket
		if strings.Contains(env+strings.Join(h.Cmd, " "), "s3cret") {
			t.Errorf("mode %q: the source password reached the settle helper", mode)
		}
	}
}

// zfs seeds are dataset mountpoints; the settle helper mounts them the same
// way the seed helpers do.
func TestSettleKeepsHostPathMount(t *testing.T) {
	d := &recordingDriver{}
	s := settleSpec(SettleFreeze)
	s.Volume, s.MountKind = "/tank/pgoverlay/src-main-g1", runtime.MountHostPath
	if err := Settle(context.Background(), d, s); err != nil {
		t.Fatal(err)
	}
	if m := d.helpers[0].Mounts[0]; m.Kind != runtime.MountHostPath || m.Volume != "/tank/pgoverlay/src-main-g1" {
		t.Fatalf("mount = %+v", m)
	}
}

func TestSettleOffRunsNothing(t *testing.T) {
	d := &recordingDriver{}
	if err := Settle(context.Background(), d, settleSpec(SettleOff)); err != nil {
		t.Fatal(err)
	}
	if len(d.helpers) != 0 {
		t.Fatalf("settle off ran %d helpers", len(d.helpers))
	}
}

func TestSettleRejectsUnknownMode(t *testing.T) {
	d := &recordingDriver{}
	err := Settle(context.Background(), d, settleSpec("sometimes"))
	if !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("err = %v, want ErrInvalidSpec", err)
	}
	if len(d.helpers) != 0 {
		t.Fatal("an unknown mode ran a helper")
	}
	// Seed and SeedDump refuse it up front, before any helper runs
	if err := settleSpec("sometimes").Validate(); !errors.Is(err, ErrInvalidSpec) || !strings.Contains(err.Error(), `"sometimes"`) {
		t.Fatalf("Validate = %v, want ErrInvalidSpec naming the mode", err)
	}
	if err := Seed(context.Background(), d, settleSpec("sometimes")); !errors.Is(err, ErrInvalidSpec) || len(d.helpers) != 0 {
		t.Fatalf("Seed = %v with %d helpers, want ErrInvalidSpec and none", err, len(d.helpers))
	}
}

// A settle that fails leaves a seed no branch should use: the error carries
// the helper's output (the server log tail) and is a seed failure, which the
// engine answers by removing the layer and the API with 422.
func TestSettleFailureIsASeedFailure(t *testing.T) {
	d := &recordingDriver{helperErr: errors.New(`helper exited 1: postgres did not start on the seeded data directory; its log ends:
FATAL:  configuration file "/seed/data/postgresql.conf" contains errors`)}
	err := Settle(context.Background(), d, settleSpec(SettleFreeze))
	if !errors.Is(err, ErrSeedFailed) {
		t.Fatalf("err = %v, want ErrSeedFailed", err)
	}
	for _, want := range []string{"db.internal:5432", "--seed-settle=freeze", "off skips this step", "contains errors"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

func TestParseSettleReport(t *testing.T) {
	r := parseSettleReport(`some server chatter
pgoverlay-settle-vacuum-error=vacuumdb: error: processing of database "app" failed: ERROR:  invalid input syntax for type integer: "[redacted]"
pgoverlay-settle-superuser=postgres
pgoverlay-settle-locked=2
pgoverlay-settle-vacuum=failed
pgoverlay-settle-state=shut down
`)
	if r.vacuum != "failed" || r.superuser != "postgres" || r.locked != 2 || r.state != "shut down" {
		t.Fatalf("report = %+v", r)
	}
	if len(r.errors) != 1 || !strings.Contains(r.errors[0], `database "app" failed`) {
		t.Fatalf("errors = %q", r.errors)
	}
	if r := parseSettleReport(""); r.vacuum != "" || r.locked != 0 {
		t.Fatalf("empty output parsed as %+v", r)
	}
}
