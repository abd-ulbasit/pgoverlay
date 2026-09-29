package pgctl

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abd-ulbasit/pgoverlay/internal/runtime"
)

func TestDetectOwner(t *testing.T) {
	for _, tc := range []struct {
		out  string
		want string
	}{
		{"pgoverlay-owner=999:999\n", "999:999"},
		{"pgoverlay-owner=70:70\n", "70:70"},
		{"some image banner\npgoverlay-owner=26:26", "26:26"},
		// output that does not parse keeps the Debian identity
		{"", ""},
		{"pgoverlay-owner=postgres:postgres\n", ""},
	} {
		d := &recordingDriver{respond: func(int, runtime.HelperSpec) (string, error) { return tc.out, nil }}
		got, err := DetectOwner(context.Background(), d, "postgres:17-alpine")
		if err != nil || got != tc.want {
			t.Errorf("output %q: owner %q, %v; want %q", tc.out, got, err, tc.want)
		}
		h := d.helpers[0]
		// the image's own passwd, as its default user; no volume, no network
		if h.Image != "postgres:17-alpine" || h.User != "" || len(h.Mounts) != 0 || h.Network != "" || h.Privileged || h.SysAdmin {
			t.Errorf("owner helper = %+v", h)
		}
		if len(h.Cmd) != 3 || h.Cmd[0] != "sh" || h.Cmd[2] != ownerScript {
			t.Errorf("owner helper cmd = %q", h.Cmd)
		}
	}
}

// An image without a postgres user can be neither seeded nor branched: a
// seed failure (the API answers 422) naming the image.
func TestDetectOwnerNoPostgresUser(t *testing.T) {
	d := &recordingDriver{helperErr: errors.New("helper exited 1: id: 'postgres': no such user")}
	_, err := DetectOwner(context.Background(), d, "example/custom:1")
	if !errors.Is(err, ErrSeedFailed) || !strings.Contains(err.Error(), "example/custom:1") || !strings.Contains(err.Error(), "no such user") {
		t.Fatalf("err = %v", err)
	}
}

// The script itself: the ids of the postgres user, or a failure.
func TestOwnerScript(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	stubs := t.TempDir()
	id := `#!/bin/sh
[ "$2" = postgres ] || { echo "id: '$2': no such user" >&2; exit 1; }
[ -z "${STUB_NO_USER:-}" ] || { echo "id: 'postgres': no such user" >&2; exit 1; }
case $1 in -u) echo 70 ;; -g) echo 71 ;; esac`
	if err := os.WriteFile(filepath.Join(stubs, "id"), []byte(id), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(env ...string) (string, error) {
		cmd := exec.Command(sh, "-c", ownerScript)
		cmd.Env = append([]string{"PATH=" + stubs + string(os.PathListSeparator) + os.Getenv("PATH")}, env...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := run(); err != nil || strings.TrimSpace(out) != "pgoverlay-owner=70:71" {
		t.Fatalf("output %q, %v", out, err)
	}
	if out, err := run("STUB_NO_USER=1"); err == nil || strings.Contains(out, "pgoverlay-owner=") {
		t.Fatalf("no postgres user: output %q, %v; want a failure", out, err)
	}
}

// With a detected owner, the seed volume is chowned to it and every seed
// helper runs as it; without one, the Debian images' 999 and the user
// named postgres, as before.
func TestSeedHelpersRunAsOwner(t *testing.T) {
	for _, tc := range []struct {
		owner, chown, user string
	}{
		{"70:70", "chown 70:70 /seed", "70:70"},
		{"", "chown 999:999 /seed", "postgres"},
	} {
		spec := settleSpec(SettleFreeze)
		spec.Owner = tc.owner

		d := &recordingDriver{}
		if err := Seed(context.Background(), d, spec); err != nil {
			t.Fatal(err)
		}
		if len(d.helpers) != 3 {
			t.Fatalf("owner %q: %d helpers", tc.owner, len(d.helpers))
		}
		if prep := strings.Join(d.helpers[0].Cmd, " "); !strings.HasSuffix(prep, tc.chown) || d.helpers[0].User != "" {
			t.Errorf("owner %q: prepare = %q as %q, want %q as root", tc.owner, prep, d.helpers[0].User, tc.chown)
		}
		for _, h := range d.helpers[1:] {
			if h.User != tc.user {
				t.Errorf("owner %q: %s runs as %q, want %q", tc.owner, h.Cmd[0], h.User, tc.user)
			}
		}

		d = &recordingDriver{}
		if err := Settle(context.Background(), d, spec); err != nil {
			t.Fatal(err)
		}
		if d.helpers[0].User != tc.user {
			t.Errorf("owner %q: settle runs as %q, want %q", tc.owner, d.helpers[0].User, tc.user)
		}

		d = &recordingDriver{}
		if err := SeedDump(context.Background(), d, SeedDumpSpec{SeedSpec: spec}); err != nil {
			t.Fatal(err)
		}
		if prep := strings.Join(d.helpers[0].Cmd, " "); !strings.HasSuffix(prep, tc.chown) {
			t.Errorf("owner %q: dump prepare = %q, want %q", tc.owner, prep, tc.chown)
		}
		if d.helpers[1].User != tc.user {
			t.Errorf("owner %q: dump runs as %q, want %q", tc.owner, d.helpers[1].User, tc.user)
		}
	}
}

// The owner reaches a shell command line (the chown): only a numeric
// uid:gid is accepted.
func TestSeedSpecRejectsNonNumericOwner(t *testing.T) {
	for _, owner := range []string{"postgres", "70", "70:70; rm -rf /seed", "70:", ":70"} {
		spec := settleSpec(SettleFreeze)
		spec.Owner = owner
		d := &recordingDriver{}
		if err := Seed(context.Background(), d, spec); !errors.Is(err, ErrInvalidSpec) || len(d.helpers) != 0 {
			t.Errorf("owner %q: err = %v with %d helpers, want ErrInvalidSpec and none", owner, err, len(d.helpers))
		}
	}
}
