package pgctl

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/abd-ulbasit/pgoverlay/internal/runtime"
)

// recordingDriver records RunHelper calls; only the methods SeedDump touches
// do anything (the rest satisfy runtime.Driver).
type recordingDriver struct {
	helpers   []runtime.HelperSpec
	helperErr error
	failFrom  int // helperErr applies from this RunHelper call index on (0 = every call)
	// respond, when set, supplies each helper's output and error instead.
	respond func(i int, s runtime.HelperSpec) (string, error)
}

func (f *recordingDriver) EnsureImage(ctx context.Context, image string) error { return nil }
func (f *recordingDriver) CreateVolume(ctx context.Context, name string, l map[string]string) error {
	return nil
}
func (f *recordingDriver) RemoveVolume(ctx context.Context, name string) error { return nil }
func (f *recordingDriver) CloneVolume(ctx context.Context, src, dst string, l map[string]string) error {
	return nil
}
func (f *recordingDriver) RunHelper(ctx context.Context, s runtime.HelperSpec) (string, error) {
	i := len(f.helpers)
	f.helpers = append(f.helpers, s)
	if f.respond != nil {
		return f.respond(i, s)
	}
	if i < f.failFrom {
		return "", nil
	}
	return "", f.helperErr
}
func (f *recordingDriver) StartBranch(ctx context.Context, s runtime.BranchSpec) (string, error) {
	return "", nil
}
func (f *recordingDriver) Exec(ctx context.Context, id string, cmd []string) error { return nil }
func (f *recordingDriver) ExecOutput(ctx context.Context, id string, cmd []string) (string, error) {
	return "", nil
}
func (f *recordingDriver) Inspect(ctx context.Context, id string) (runtime.ContainerInfo, error) {
	return runtime.ContainerInfo{}, nil
}
func (f *recordingDriver) StopRemove(ctx context.Context, id string) error { return nil }
func (f *recordingDriver) ListManaged(ctx context.Context) ([]runtime.ContainerInfo, error) {
	return nil, nil
}
func (f *recordingDriver) ListHelpers(ctx context.Context) ([]runtime.ContainerInfo, error) {
	return nil, nil
}
func (f *recordingDriver) ListManagedVolumes(ctx context.Context, instanceID string) ([]runtime.VolumeInfo, error) {
	return nil, nil
}

func TestSeedDumpHelperSpec(t *testing.T) {
	t.Setenv(SSLModeEnv, "")
	d := &recordingDriver{}
	err := SeedDump(context.Background(), d, SeedDumpSpec{
		SeedSpec: SeedSpec{
			Image: "postgres:17", Volume: "pgoverlay-src-main", Network: "net1",
			Host: "db.proj.supabase.co", Port: 6543, User: "appuser", Password: "s3cret",
		},
		Database: "appdb",
		Schemas:  []string{"public", "audit"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.helpers) != 2 {
		t.Fatalf("helpers=%d want 2 (chown prep + dump)", len(d.helpers))
	}
	// prep helper chowns the seed volume to the in-image postgres uid
	prep := d.helpers[0]
	if prep.Image != runtime.UtilityImage || !strings.Contains(strings.Join(prep.Cmd, " "), "chown 999:999 /seed") {
		t.Fatalf("prep helper: %+v", prep)
	}
	if len(prep.Mounts) != 1 || prep.Mounts[0].Volume != "pgoverlay-src-main" || prep.Mounts[0].Target != "/seed" {
		t.Fatalf("prep mounts: %+v", prep.Mounts)
	}

	dump := d.helpers[1]
	if dump.Image != "postgres:17" {
		t.Fatalf("image=%q want the source's pg_version image", dump.Image)
	}
	if dump.User != "postgres" {
		t.Fatalf("user=%q want postgres", dump.User)
	}
	if dump.Network != "net1" {
		t.Fatalf("network=%q want net1", dump.Network)
	}
	if len(dump.Mounts) != 1 || dump.Mounts[0].Volume != "pgoverlay-src-main" || dump.Mounts[0].Target != "/seed" {
		t.Fatalf("dump mounts: %+v", dump.Mounts)
	}
	// remote credentials travel via env, never argv
	env := strings.Join(dump.Env, "\n")
	for _, want := range []string{
		"PGB_USER=appuser", "PGB_PASSWORD=s3cret", "PGB_DB=appdb",
		"PGB_REMOTE_HOST=db.proj.supabase.co", "PGB_REMOTE_PORT=6543",
		// remote leg: libpq's default sslmode unless configured, and a
		// bounded connect time
		"PGB_SSLMODE=prefer", "PGB_CONNECT_TIMEOUT=10",
		// a scoped dump pre-creates extensions and makes CREATE SCHEMA
		// idempotent
		"PGB_SCOPED=1", "PGB_DATA=/seed/data",
		// the default settle mode freezes the restored cluster before its
		// clean stop
		"PGB_SETTLE=freeze",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("env missing %q: %v", want, dump.Env)
		}
	}
	if len(dump.Cmd) != 3 || dump.Cmd[0] != "bash" || dump.Cmd[1] != "-c" {
		t.Fatalf("cmd=%v want bash -c <script>", dump.Cmd)
	}
	script := dump.Cmd[2]
	if strings.Contains(script, "s3cret") {
		t.Fatal("password leaked into the script (argv)")
	}
	for _, want := range []string{
		"set -euo pipefail",
		`initdb -D "$PGB_DATA"`,
		"--auth-local=trust --auth-host=scram-sha-256",
		"host all all all scram-sha-256",
		"listen_addresses = '*'",
		`pg_ctl -D "$PGB_DATA" -l /tmp/pgoverlay-seed.log`,
		"unix_socket_directories=/tmp",
		"createdb",
		"CREATE ROLE ' || quote_ident(rolname) || ' NOLOGIN",
		"CREATE EXTENSION IF NOT EXISTS ' || quote_ident(e.extname)",
		"CREATE SCHEMA IF NOT EXISTS \\1;",
		"pg_dump --no-owner --no-acl -n 'public' -n 'audit'",
		`PGSSLMODE="$PGB_SSLMODE" PGCONNECT_TIMEOUT="$PGB_CONNECT_TIMEOUT"`,
		"ON_ERROR_STOP=1",
		"VERBOSITY=terse",
		"SHOW_CONTEXT=never",
		`vacuumdb -h /tmp -U "$PGB_USER" --all --freeze --analyze`,
		`pg_ctl -D "$PGB_DATA" -w stop -m fast`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
	// public is never dropped: the pattern-independent CREATE SCHEMA
	// rewrite replaced the exact-match "public" special case
	if strings.Contains(script, "DROP SCHEMA") {
		t.Errorf("script drops a schema:\n%s", script)
	}
}

func TestSeedDumpNoSchemasDumpsWholeDatabase(t *testing.T) {
	d := &recordingDriver{}
	err := SeedDump(context.Background(), d, SeedDumpSpec{
		SeedSpec: SeedSpec{Image: "postgres:16", Volume: "v", Host: "h", Port: 5432, User: "postgres", Password: "pw"},
	})
	if err != nil {
		t.Fatal(err)
	}
	script := d.helpers[1].Cmd[2]
	if !strings.Contains(script, "pg_dump --no-owner --no-acl \\\n") {
		t.Fatalf("schema flags present for whole-database dump:\n%s", script)
	}
	// a whole-database dump never emits CREATE SCHEMA public; the
	// initdb-created schema must survive
	if strings.Contains(script, "DROP SCHEMA public") {
		t.Fatalf("whole-database dump must not drop public:\n%s", script)
	}
	// empty Database defaults to postgres (no createdb needed, but the guard
	// must still compare against it)
	env := strings.Join(d.helpers[1].Env, "\n")
	if !strings.Contains(env, "PGB_DB=postgres") {
		t.Fatalf("env missing PGB_DB=postgres default: %v", d.helpers[1].Env)
	}
	// whole-database dumps carry their own extensions and schemas
	if v, ok := envOf(d.helpers[1].Env, "PGB_SCOPED"); !ok || v != "" {
		t.Fatalf("PGB_SCOPED = %q (set %v), want empty for a whole-database dump", v, ok)
	}
}

func TestSeedDumpHelperFailure(t *testing.T) {
	d := &recordingDriver{helperErr: errors.New("exit 1: pg_dump: connection refused")}
	err := SeedDump(context.Background(), d, SeedDumpSpec{
		SeedSpec: SeedSpec{Image: "postgres:17", Volume: "v", Host: "h", Port: 5432, User: "u", Password: "p"},
	})
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("err=%v want wrapped helper failure", err)
	}
}
