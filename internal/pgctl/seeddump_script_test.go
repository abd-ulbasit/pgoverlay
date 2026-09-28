package pgctl

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Stubs for the Postgres client tools the dump script calls. They record
// their argv, relevant environment and stdin under $REC and replay canned
// catalog output and a canned dump, so the script's control flow, filtering
// and log hygiene run for real (bash, sed) without a server.
var dumpScriptStubs = map[string]string{
	"initdb": `d=$2
mkdir -p "$d"
: > "$d/pg_hba.conf"
: > "$d/postgresql.conf"
for a in "$@"; do case "$a" in --pwfile=*) cat "${a#--pwfile=}" > "$REC/pwfile";; esac; done
echo "initdb chatter that would crowd the error tail"`,
	"pg_ctl":   `echo "$@" >> "$REC/pg_ctl"`,
	"createdb": `echo "$@" > "$REC/createdb"`,
	"pg_dump": `printf 'PGPASSWORD=%s PGSSLMODE=%s PGCONNECT_TIMEOUT=%s\n' "${PGPASSWORD:-}" "${PGSSLMODE:-}" "${PGCONNECT_TIMEOUT:-}" > "$REC/pg_dump.env"
echo "$@" > "$REC/pg_dump.args"
cat "$STUB_DUMP"`,
	"psql": `case " $* " in
*" -c "*)
  printf 'PGPASSWORD=%s PGSSLMODE=%s\n' "${PGPASSWORD:-}" "${PGSSLMODE:-}" >> "$REC/psql-remote.env"
  case "$*" in
  *pg_roles*) echo 'CREATE ROLE authenticated NOLOGIN;' ;;
  *pg_extension*) echo 'CREATE SCHEMA IF NOT EXISTS extensions; CREATE EXTENSION IF NOT EXISTS "uuid-ossp" WITH SCHEMA extensions CASCADE;' ;;
  esac
  ;;
*)
  n=$(cat "$REC/n" 2>/dev/null || echo 0)
  echo $((n + 1)) > "$REC/n"
  cat > "$REC/psql-local-$n.sql"
  printf 'PGPASSWORD=%s\n' "${PGPASSWORD:-}" > "$REC/psql-local-$n.env"
  echo " set_config"
  if [ -n "${STUB_RESTORE_FAIL:-}" ] && grep -q '^COPY ' "$REC/psql-local-$n.sql"; then
    printf '%s\n' "$STUB_RESTORE_FAIL" >&2
    exit 3
  fi
  ;;
esac`,
}

const cannedDump = `--
-- PostgreSQL database dump
--
SET client_min_messages = warning;
CREATE SCHEMA public;
CREATE SCHEMA audit;
CREATE SCHEMA "My Schema";
CREATE TABLE public.lines (line text);
COPY public.lines (line) FROM stdin;
CREATE SCHEMA evil;
\.
CREATE SCHEMA late;
`

type dumpRun struct {
	rec    string
	out    string
	failed bool
}

func (r dumpRun) file(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.rec, name))
	if err != nil {
		t.Fatalf("%s: %v (script output:\n%s)", name, err, r.out)
	}
	return string(b)
}

// runDumpScript renders the helper script the way SeedDump does and runs it
// with bash against the stubs, using the helper's env with PGB_DATA moved
// into a temp dir.
func runDumpScript(t *testing.T, spec SeedDumpSpec, extraEnv ...string) dumpRun {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	d := &recordingDriver{}
	if err := SeedDump(t.Context(), d, spec); err != nil {
		t.Fatal(err)
	}
	h := d.helpers[1]

	stubs, rec := t.TempDir(), t.TempDir()
	for name, body := range dumpScriptStubs {
		if err := os.WriteFile(filepath.Join(stubs, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dump := filepath.Join(t.TempDir(), "dump.sql")
	if err := os.WriteFile(dump, []byte(cannedDump), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{
		"PATH=" + stubs + string(os.PathListSeparator) + os.Getenv("PATH"),
		"REC=" + rec, "STUB_DUMP=" + dump,
	}
	for _, kv := range h.Env {
		if strings.HasPrefix(kv, "PGB_DATA=") {
			kv = "PGB_DATA=" + filepath.Join(t.TempDir(), "data")
		}
		env = append(env, kv)
	}
	cmd := exec.Command(bash, h.Cmd[1:]...)
	cmd.Env = append(env, extraEnv...)
	out, err := cmd.CombinedOutput()
	return dumpRun{rec: rec, out: string(out), failed: err != nil}
}

func scopedSpec() SeedDumpSpec {
	return SeedDumpSpec{
		SeedSpec: SeedSpec{Image: "postgres:17", Volume: "v", Host: "db.example.com", Port: 5432,
			User: "app", Password: "pw", SSLMode: "require"},
		Database: "appdb",
		// PUBLIC selects public too: pg_dump case-folds unquoted patterns
		Schemas: []string{"PUBLIC", "aud*"},
	}
}

func TestSeedDumpScriptScoped(t *testing.T) {
	r := runDumpScript(t, scopedSpec())
	if r.failed {
		t.Fatalf("script failed:\n%s", r.out)
	}

	// roles and extensions first, errors tolerated (no ON_ERROR_STOP)
	pre := r.file(t, "psql-local-0.sql")
	for _, want := range []string{
		"SET client_min_messages = warning;",
		"CREATE ROLE authenticated NOLOGIN;",
		`CREATE EXTENSION IF NOT EXISTS "uuid-ossp" WITH SCHEMA extensions CASCADE;`,
	} {
		if !strings.Contains(pre, want) {
			t.Errorf("pre-restore SQL missing %q:\n%s", want, pre)
		}
	}

	// every pre-data CREATE SCHEMA is idempotent, whatever the pattern that
	// selected it; row data after the first COPY is untouched
	restore := r.file(t, "psql-local-1.sql")
	for _, want := range []string{
		"\nCREATE SCHEMA IF NOT EXISTS public;\n",
		"\nCREATE SCHEMA IF NOT EXISTS audit;\n",
		"\nCREATE SCHEMA IF NOT EXISTS \"My Schema\";\n",
		"\nCREATE SCHEMA evil;\n",
		"\nCREATE SCHEMA late;\n",
	} {
		if !strings.Contains(restore, want) {
			t.Errorf("restore input missing %q:\n%s", want, restore)
		}
	}

	// only the remote leg carries the password and TLS settings
	if got := r.file(t, "pg_dump.env"); got != "PGPASSWORD=pw PGSSLMODE=require PGCONNECT_TIMEOUT=10\n" {
		t.Errorf("pg_dump env = %q", got)
	}
	if got := r.file(t, "pg_dump.args"); !strings.HasPrefix(got, "--no-owner --no-acl -n PUBLIC -n aud* -h db.example.com -p 5432 -U app -d appdb") {
		t.Errorf("pg_dump args = %q", got)
	}
	for _, line := range strings.Split(strings.TrimSpace(r.file(t, "psql-remote.env")), "\n") {
		if line != "PGPASSWORD=pw PGSSLMODE=require" {
			t.Errorf("catalog query env = %q", line)
		}
	}
	for _, f := range []string{"psql-local-0.env", "psql-local-1.env"} {
		if got := r.file(t, f); got != "PGPASSWORD=\n" {
			t.Errorf("%s = %q: the temp server leg must not see the source password", f, got)
		}
	}
	if got := r.file(t, "pwfile"); got != "pw\n" {
		t.Errorf("initdb pwfile = %q", got)
	}
	if got := r.file(t, "createdb"); !strings.HasSuffix(strings.TrimSpace(got), "appdb") {
		t.Errorf("createdb args = %q", got)
	}
	if !strings.Contains(r.file(t, "pg_ctl"), "-l /tmp/pgoverlay-seed.log") {
		t.Errorf("temp server log not redirected to a file: %s", r.file(t, "pg_ctl"))
	}
	// stdout chatter (initdb, psql results) stays out of the helper output
	if strings.Contains(r.out, "chatter") || strings.Contains(r.out, "set_config") {
		t.Errorf("helper output carries stdout noise:\n%s", r.out)
	}
}

func TestSeedDumpScriptWholeDatabase(t *testing.T) {
	spec := scopedSpec()
	spec.Schemas = nil
	r := runDumpScript(t, spec)
	if r.failed {
		t.Fatalf("script failed:\n%s", r.out)
	}
	if pre := r.file(t, "psql-local-0.sql"); !strings.Contains(pre, "CREATE ROLE authenticated NOLOGIN;") || strings.Contains(pre, "EXTENSION") {
		t.Errorf("whole-database pre-restore SQL = %q, want roles only (the dump has its extensions)", pre)
	}
	if got := r.file(t, "psql-local-1.sql"); got != cannedDump {
		t.Errorf("whole-database dump was rewritten:\n%s", got)
	}
}

// A data error must not copy row values into the helper output, which ends
// up in the error message, the logs and the registry's source state.
func TestSeedDumpScriptRedactsRowData(t *testing.T) {
	r := runDumpScript(t, scopedSpec(),
		"STUB_RESTORE_FAIL=psql:<stdin>:9: ERROR:  invalid input syntax for type integer: \"4111 1111 1111 1111\"\n"+
			"CONTEXT:  COPY lines, line 1: \"s3cr3t-row\"")
	if !r.failed {
		t.Fatalf("script succeeded despite a failing restore:\n%s", r.out)
	}
	if strings.Contains(r.out, "4111") || strings.Contains(r.out, "s3cr3t") {
		t.Errorf("row data in the helper output:\n%s", r.out)
	}
	if !strings.Contains(r.out, `ERROR:  invalid input syntax for type integer: "[redacted]"`) {
		t.Errorf("error lost its non-data part:\n%s", r.out)
	}
}
