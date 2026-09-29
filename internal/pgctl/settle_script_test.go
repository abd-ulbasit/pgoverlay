package pgctl

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Stubs for the server-side tools settleScript calls. They record argv, the
// libpq environment and the order of the steps under $REC and fail on
// demand, so the script's control flow, its overrides and its report run for
// real (sh, sed, grep) without a server.
var settleScriptStubs = map[string]string{
	"pg_ctl": `echo "$*" >> "$REC/pg_ctl"
echo "PGCTLTIMEOUT=${PGCTLTIMEOUT:-}" > "$REC/pg_ctl.env"
log= d= action=
while [ $# -gt 0 ]; do
  case "$1" in
  -l) log=$2; shift ;;
  -D) d=$2; shift ;;
  -o|-m|-t) shift ;;
  start|stop) action=$1 ;;
  esac
  shift
done
echo "$action" >> "$REC/order"
case "$action" in
start)
  echo "LOG:  database system was interrupted" >> "$log"
  if [ -n "${STUB_START_FAIL:-}" ]; then printf '%s\n' "$STUB_START_FAIL" >> "$log"; exit 1; fi
  echo "$log" > "$REC/logpath"
  echo "postgres -D $d" > "$d/postmaster.opts" ;;
stop)
  if [ -n "${STUB_STOP_FAIL:-}" ]; then printf '%s\n' "$STUB_STOP_FAIL" >> "$(cat "$REC/logpath")"; exit 1; fi ;;
esac`,
	"psql": `u= db= sql=
while [ $# -gt 0 ]; do
  case "$1" in
  -U) u=$2; shift ;;
  -d) db=$2; shift ;;
  -c) sql=$2; shift ;;
  esac
  shift
done
echo "$u@$db: $sql" >> "$REC/psql"
printf 'PGHOST=%s PGPORT=%s PGAPPNAME=%s\nPGOPTIONS=%s\n' "$PGHOST" "$PGPORT" "$PGAPPNAME" "$PGOPTIONS" > "$REC/psql.env"
case "$sql" in
CHECKPOINT) echo checkpoint >> "$REC/order" ;;
*pg_roles*)
  case " ${STUB_LOGIN_OK-postgres} " in
  *" $u@$db "*|*" $u "*) ;;
  *) echo "psql: error: FATAL:  role \"$u\" does not exist" >&2; exit 2 ;;
  esac
  printf '%s\n' "${STUB_SUPERUSER-postgres}" ;;
esac`,
	"vacuumdb": `echo "$*" >> "$REC/vacuumdb"
printf 'PGHOST=%s PGOPTIONS=%s\n' "$PGHOST" "$PGOPTIONS" > "$REC/vacuumdb.env"
echo vacuum >> "$REC/order"
i=0
while [ "$i" -lt "${STUB_LOCKED:-0}" ]; do
  echo 'WARNING:  skipping vacuum of "t" --- lock not available' >&2
  i=$((i + 1))
done
if [ -n "${STUB_VACUUM_FAIL:-}" ]; then printf '%s\n' "$STUB_VACUUM_FAIL" >&2; exit 1; fi
echo 'vacuumdb: vacuuming database "postgres"'`,
	"pg_controldata": `echo "pg_control version number:            1700"
echo "Database cluster state:               ${STUB_STATE-shut down}"`,
	"sync": `echo "$*" >> "$REC/sync"
echo sync >> "$REC/order"`,
}

type settleRun struct {
	rec, data string
	out       string
	failed    bool
}

func (r settleRun) file(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.rec, name))
	if err != nil {
		t.Fatalf("%s: %v (script output:\n%s)", name, err, r.out)
	}
	return string(b)
}

func (r settleRun) has(name string) bool {
	_, err := os.Stat(filepath.Join(r.rec, name))
	return err == nil
}

// runSettleScript runs the script Settle hands the helper, the way the
// helper runs it (sh -c, the data dir as $1, the helper's env), against the
// stubs. major is the cluster's PG_VERSION; conf says whether the data dir
// has a postgresql.conf.
func runSettleScript(t *testing.T, mode SettleMode, major string, conf bool, extraEnv ...string) settleRun {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	d := &recordingDriver{}
	if err := Settle(t.Context(), d, SeedSpec{Image: "postgres:" + major, Volume: "v", Host: "db", Port: 5432, User: "replicator", Settle: mode}); err != nil {
		t.Fatal(err)
	}
	h := d.helpers[0]

	stubs, rec, data := t.TempDir(), t.TempDir(), t.TempDir()
	for name, body := range settleScriptStubs {
		if err := os.WriteFile(filepath.Join(stubs, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(data, "PG_VERSION"), []byte(major+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if conf {
		if err := os.WriteFile(filepath.Join(data, "postgresql.conf"), []byte("shared_preload_libraries = 'timescaledb'\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(sh, append(h.Cmd[1:len(h.Cmd)-1], data)...)
	cmd.Env = append([]string{
		"PATH=" + stubs + string(os.PathListSeparator) + os.Getenv("PATH"),
		"REC=" + rec, "TMPDIR=" + t.TempDir(),
	}, append(h.Env, extraEnv...)...)
	out, err := cmd.CombinedOutput()
	return settleRun{rec: rec, data: data, out: string(out), failed: err != nil}
}

func TestSettleScriptFreeze(t *testing.T) {
	r := runSettleScript(t, SettleFreeze, "17", true)
	if r.failed {
		t.Fatalf("script failed:\n%s", r.out)
	}
	// recover (start), freeze, checkpoint, clean stop, then make it durable
	if got := strings.Fields(r.file(t, "order")); strings.Join(got, " ") != "start vacuum checkpoint stop sync" {
		t.Fatalf("steps = %v", got)
	}
	ctl := r.file(t, "pg_ctl")
	start, stop, _ := strings.Cut(ctl, "\n")
	for _, want := range []string{
		"-w start",
		// nothing reaches the server but this container, and the
		// source's auth, TLS, logging and archiving do not apply
		"-c listen_addresses= ", "-c port=5432", "-c unix_socket_permissions=0700",
		"/pg_hba.conf", "-c ident_file=", "-c ssl=off",
		"-c logging_collector=off", "-c log_destination=stderr", "-c archive_mode=off",
		"-c synchronous_standby_names= ",
		// production memory and preload settings cannot stop the start
		"-c shared_preload_libraries= ", "-c session_preload_libraries= ", "-c local_preload_libraries=",
		"-c shared_buffers=128MB", "-c huge_pages=off", "-c min_dynamic_shared_memory=0",
		"-c autovacuum=off",
		// throwaway durability, made up for by the final sync
		"-c fsync=off", "-c full_page_writes=off", "-c synchronous_commit=off",
		// no recycled WAL left behind in the seed
		"-c wal_recycle=off", "-c wal_keep_size=0",
		"-c recovery_init_sync_method=syncfs",
		"-c data_directory=" + r.data,
		"-c event_triggers=off", // 17+
	} {
		if !strings.Contains(start, want) {
			t.Errorf("pg_ctl start lacks %q:\n%s", want, start)
		}
	}
	// the data dir has its own postgresql.conf, which stays in charge of
	// everything not overridden; io_method exists from 18 on only
	for _, unwanted := range []string{"config_file", "io_method"} {
		if strings.Contains(start, unwanted) {
			t.Errorf("pg_ctl start sets %s for a PG 17 data dir with a postgresql.conf:\n%s", unwanted, start)
		}
	}
	if !strings.Contains(stop, "-m fast -w stop") {
		t.Errorf("pg_ctl stop = %q, want a fast, waited stop", stop)
	}
	if got := r.file(t, "pg_ctl.env"); got != "PGCTLTIMEOUT=86400\n" {
		t.Errorf("pg_ctl env = %q, want the long PGCTLTIMEOUT", got)
	}
	// the private socket, reached with trust: the hba file allows nothing else
	hba, _, _ := strings.Cut(strings.SplitN(start, "-c hba_file=", 2)[1], " ")
	if got, err := os.ReadFile(hba); err == nil {
		t.Errorf("temporary hba file %s outlived the script: %q", hba, got)
	}
	vac := r.file(t, "vacuumdb")
	if strings.TrimSpace(vac) != "-U postgres --all --freeze --analyze --skip-locked" {
		t.Errorf("vacuumdb args = %q", vac)
	}
	venv := r.file(t, "vacuumdb.env")
	for _, want := range []string{
		"-c statement_timeout=0", "-c lock_timeout=0", "-c idle_in_transaction_session_timeout=0",
		"-c default_transaction_read_only=off", "-c max_parallel_maintenance_workers=0",
		"-c transaction_timeout=0", // 17+
	} {
		if !strings.Contains(venv, want) {
			t.Errorf("vacuumdb PGOPTIONS lacks %q: %s", want, venv)
		}
	}
	if !strings.Contains(venv, "PGHOST="+filepath.Dir(hba)) {
		t.Errorf("vacuumdb does not use the private socket dir %s: %s", filepath.Dir(hba), venv)
	}
	// the source's role is asked for the superuser first; the CHECKPOINT
	// runs as that superuser
	psql := r.file(t, "psql")
	if !strings.HasPrefix(psql, "replicator@template1: SELECT rolname FROM pg_roles WHERE rolsuper AND rolcanlogin") {
		t.Errorf("superuser discovery = %q", psql)
	}
	if !strings.Contains(psql, "postgres@template1: CHECKPOINT") {
		t.Errorf("no CHECKPOINT as the superuser: %q", psql)
	}
	if !strings.Contains(r.file(t, "psql.env"), "PGAPPNAME=pgoverlay-settle") {
		t.Errorf("psql env = %q", r.file(t, "psql.env"))
	}
	if got := strings.TrimSpace(r.file(t, "sync")); got != "-f "+r.data {
		t.Errorf("sync args = %q, want a syncfs of the data dir", got)
	}
	if _, err := os.Stat(filepath.Join(r.data, "postmaster.opts")); err == nil {
		t.Error("postmaster.opts (with the settle overrides) left in the seed")
	}
	rep := parseSettleReport(r.out)
	if rep.vacuum != "ok" || rep.superuser != "postgres" || rep.state != "shut down" || rep.locked != 0 {
		t.Errorf("report = %+v from:\n%s", rep, r.out)
	}
	// only the report reaches the helper output (the runtime keeps 20 lines)
	for _, line := range strings.Split(strings.TrimSpace(r.out), "\n") {
		if !strings.HasPrefix(line, "pgoverlay-settle-") {
			t.Errorf("stray output line %q", line)
		}
	}
}

func TestSettleScriptRecoverSkipsVacuum(t *testing.T) {
	r := runSettleScript(t, SettleRecover, "17", true)
	if r.failed {
		t.Fatalf("script failed:\n%s", r.out)
	}
	if got := strings.Join(strings.Fields(r.file(t, "order")), " "); got != "start checkpoint stop sync" {
		t.Fatalf("steps = %v", got)
	}
	if r.has("vacuumdb") {
		t.Fatal("recover ran vacuumdb")
	}
	if rep := parseSettleReport(r.out); rep.vacuum != "skipped" || rep.state != "shut down" {
		t.Errorf("report = %+v", rep)
	}
}

// Version-dependent settings: every override must exist in the cluster's
// major, or the start fails with "unrecognized configuration parameter".
func TestSettleScriptVersionGates(t *testing.T) {
	for _, tc := range []struct {
		major            string
		want, unwanted   []string
		optsWant, optsNo []string
	}{
		{major: "14", unwanted: []string{"event_triggers", "io_method"}, optsNo: []string{"transaction_timeout"}},
		{major: "16", unwanted: []string{"event_triggers", "io_method"}, optsNo: []string{"transaction_timeout"}},
		{major: "17", want: []string{"-c event_triggers=off"}, unwanted: []string{"io_method"}, optsWant: []string{"transaction_timeout=0"}},
		{major: "18", want: []string{"-c event_triggers=off", "-c io_method=worker"}, optsWant: []string{"transaction_timeout=0"}},
	} {
		r := runSettleScript(t, SettleFreeze, tc.major, true)
		if r.failed {
			t.Fatalf("PG %s: script failed:\n%s", tc.major, r.out)
		}
		start, _, _ := strings.Cut(r.file(t, "pg_ctl"), "\n")
		for _, w := range tc.want {
			if !strings.Contains(start, w) {
				t.Errorf("PG %s: start lacks %q", tc.major, w)
			}
		}
		for _, u := range tc.unwanted {
			if strings.Contains(start, u) {
				t.Errorf("PG %s: start sets %s, which this major does not know", tc.major, u)
			}
		}
		venv := r.file(t, "vacuumdb.env")
		for _, w := range tc.optsWant {
			if !strings.Contains(venv, w) {
				t.Errorf("PG %s: PGOPTIONS lacks %q", tc.major, w)
			}
		}
		for _, u := range tc.optsNo {
			if strings.Contains(venv, u) {
				t.Errorf("PG %s: PGOPTIONS sets %s, which this major does not know", tc.major, u)
			}
		}
	}
}

// Distro-packaged sources keep postgresql.conf in /etc, so the copy has
// none; the settle server gets an empty one, as branches do.
func TestSettleScriptMissingPostgresqlConf(t *testing.T) {
	r := runSettleScript(t, SettleFreeze, "15", false)
	if r.failed {
		t.Fatalf("script failed:\n%s", r.out)
	}
	start, _, _ := strings.Cut(r.file(t, "pg_ctl"), "\n")
	if !strings.Contains(start, "-c config_file=") || !strings.Contains(start, "/postgresql.conf") {
		t.Fatalf("no replacement config_file: %s", start)
	}
	if _, err := os.Stat(filepath.Join(r.data, "postgresql.conf")); err == nil {
		t.Fatal("settle wrote a postgresql.conf into the seed")
	}
}

// A start failure is the seed's failure: the script stops, says so and shows
// the server log's tail, and runs nothing else.
func TestSettleScriptStartFailure(t *testing.T) {
	r := runSettleScript(t, SettleFreeze, "17", true,
		`STUB_START_FAIL=FATAL:  could not access file "timescaledb": No such file or directory`)
	if !r.failed {
		t.Fatalf("script succeeded despite a failed start:\n%s", r.out)
	}
	for _, want := range []string{"postgres did not start on the seeded data directory", `could not access file "timescaledb"`} {
		if !strings.Contains(r.out, want) {
			t.Errorf("output lacks %q:\n%s", want, r.out)
		}
	}
	if got := strings.Join(strings.Fields(r.file(t, "order")), " "); got != "start" {
		t.Fatalf("steps after a failed start = %v", got)
	}
}

// A failed VACUUM is reported, not fatal: the seed is still stopped cleanly
// and synced. The report keeps the error's first part and drops the value
// the server quoted after it, which can be row data.
func TestSettleScriptVacuumFailure(t *testing.T) {
	r := runSettleScript(t, SettleFreeze, "17", true,
		"STUB_LOCKED=2",
		"STUB_VACUUM_FAIL=vacuumdb: error: processing of database \"app\" failed: ERROR:  invalid input syntax for type integer: \"4111 1111 1111 1111\"\n"+
			"CONTEXT:  SQL function \"card_digits\" statement 1: \"s3cr3t-row\"")
	if r.failed {
		t.Fatalf("a failed VACUUM failed the settle:\n%s", r.out)
	}
	if got := strings.Join(strings.Fields(r.file(t, "order")), " "); got != "start vacuum checkpoint stop sync" {
		t.Fatalf("steps = %v", got)
	}
	if strings.Contains(r.out, "4111") || strings.Contains(r.out, "s3cr3t") {
		t.Errorf("row data in the helper output:\n%s", r.out)
	}
	rep := parseSettleReport(r.out)
	if rep.vacuum != "failed" || rep.locked != 2 || rep.state != "shut down" {
		t.Errorf("report = %+v", rep)
	}
	if len(rep.errors) != 1 || rep.errors[0] != `vacuumdb: error: processing of database "app" failed: ERROR:  invalid input syntax for type integer: "[redacted]"` {
		t.Errorf("vacuum errors = %q", rep.errors)
	}
}

// Without a role that is both SUPERUSER and LOGIN the VACUUM cannot freeze
// every table; the seed is still recovered and stopped cleanly.
func TestSettleScriptNoSuperuser(t *testing.T) {
	r := runSettleScript(t, SettleFreeze, "17", true, "STUB_SUPERUSER=")
	if r.failed {
		t.Fatalf("script failed:\n%s", r.out)
	}
	if got := strings.Join(strings.Fields(r.file(t, "order")), " "); got != "start stop sync" {
		t.Fatalf("steps = %v", got)
	}
	if r.has("vacuumdb") {
		t.Fatal("vacuumdb ran without a superuser")
	}
	if rep := parseSettleReport(r.out); rep.vacuum != "no-superuser" {
		t.Errorf("report = %+v", rep)
	}
}

// The seed role may be gone or unable to log in to template1; discovery then
// falls back to postgres, and to the postgres database.
func TestSettleScriptSuperuserDiscoveryFallsBack(t *testing.T) {
	r := runSettleScript(t, SettleFreeze, "17", true, "STUB_LOGIN_OK=postgres@postgres", "STUB_SUPERUSER=admin")
	if r.failed {
		t.Fatalf("script failed:\n%s", r.out)
	}
	tries := strings.Split(strings.TrimSpace(r.file(t, "psql")), "\n")
	want := []string{"replicator@template1", "postgres@template1", "replicator@postgres", "postgres@postgres", "admin@postgres"}
	if len(tries) != len(want) {
		t.Fatalf("psql calls = %q, want %v", tries, want)
	}
	for i, w := range want {
		if !strings.HasPrefix(tries[i], w+": ") {
			t.Errorf("psql call %d = %q, want %s", i, tries[i], w)
		}
	}
	if vac := r.file(t, "vacuumdb"); !strings.HasPrefix(vac, "-U admin ") {
		t.Errorf("vacuumdb args = %q, want the discovered superuser", vac)
	}
}

func TestSettleScriptStopFailure(t *testing.T) {
	r := runSettleScript(t, SettleFreeze, "17", true, "STUB_STOP_FAIL="+
		"ERROR:  invalid input syntax for type integer: \"4111 1111 1111 1111\"\n"+
		"CONTEXT:  SQL function \"card_digits\" statement 1\n"+
		"DETAIL:  Key (email)=(someone@example.com) already exists.\n"+
		"STATEMENT:  VACUUM (FREEZE, ANALYZE) public.cards\n"+
		"PANIC:  could not write to file \"pg_wal/xlogtemp.61\": No space left on device")
	if !r.failed {
		t.Fatalf("script succeeded despite a failed stop:\n%s", r.out)
	}
	for _, want := range []string{"postgres did not stop cleanly", "No space left on device", `integer: "[redacted]"`} {
		if !strings.Contains(r.out, want) {
			t.Errorf("output lacks %q:\n%s", want, r.out)
		}
	}
	// the log tail that becomes the error message quotes no row
	for _, leak := range []string{"4111", "someone@example.com", "card_digits", "public.cards"} {
		if strings.Contains(r.out, leak) {
			t.Errorf("output leaks %q:\n%s", leak, r.out)
		}
	}
	if r.has("sync") || strings.Contains(r.out, "pgoverlay-settle-state=") {
		t.Error("a failed stop was reported as settled")
	}
}

// The final check: whatever pg_ctl said, the control file must record a
// clean shutdown, or branches would run crash recovery after all.
func TestSettleScriptUncleanStateFails(t *testing.T) {
	for _, state := range []string{"in production", ""} {
		r := runSettleScript(t, SettleFreeze, "17", true, "STUB_STATE="+state)
		if !r.failed {
			t.Fatalf("state %q accepted:\n%s", state, r.out)
		}
		if !strings.Contains(r.out, "not cleanly shut down") {
			t.Errorf("state %q: output:\n%s", state, r.out)
		}
		if r.has("sync") {
			t.Errorf("state %q: synced and reported an unclean seed", state)
		}
	}
}
