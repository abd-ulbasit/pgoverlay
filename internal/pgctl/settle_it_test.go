package pgctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/abd-ulbasit/pgoverlay/internal/pgctl/pgctltest"
	"github.com/abd-ulbasit/pgoverlay/internal/runtime"
)

// appendSourceConfig appends lines to the running source's postgresql.conf
// without reloading it: the source keeps running on its old settings, and
// pg_basebackup copies the file as it now is, the way a production
// configuration reaches a seed.
func appendSourceConfig(t *testing.T, ctx context.Context, conn *pgx.Conn, lines ...string) {
	t.Helper()
	var file string
	if err := conn.QueryRow(ctx, "SHOW config_file").Scan(&file); err != nil {
		t.Fatal(err)
	}
	q := fmt.Sprintf("COPY (SELECT unnest($1::text[])) TO PROGRAM 'cat >> %s'", strings.ReplaceAll(file, "'", "''"))
	// COPY cannot take parameters; inline the lines as an array literal
	arr := make([]string, len(lines))
	for i, l := range lines {
		arr[i] = "'" + strings.ReplaceAll(l, "'", "''") + "'"
	}
	q = strings.Replace(q, "$1::text[]", "ARRAY["+strings.Join(arr, ",")+"]", 1)
	if _, err := conn.Exec(ctx, q); err != nil {
		t.Fatalf("append to %s: %v", file, err)
	}
}

// seedCheckScript inspects a settled seed ($1): the control file's state and
// whether a backup_label is left, then starts it once (as a branch would, on
// a private socket) and reports how the server came up and the oldest
// relfrozenxid age.
const seedCheckScript = `set -eu
d=$1
echo "state=$(LC_ALL=C pg_controldata "$d" | sed -n 's/^Database cluster state: *//p')"
if [ -e "$d/backup_label" ]; then echo backup_label=present; else echo backup_label=absent; fi
t=$(mktemp -d)
printf 'local all all trust\n' > "$t/hba"
pg_ctl -D "$d" -l "$t/log" -o "-c listen_addresses= -c port=5432 -c unix_socket_directories=$t -c hba_file=$t/hba -c ssl=off -c logging_collector=off -c archive_mode=off -c shared_preload_libraries= -c shared_buffers=128MB -c huge_pages=off" -w start >/dev/null
grep -h -o "database system was [a-z ]*" "$t/log" | head -n 1 | sed 's/^/start=/'
if grep -q "redo starts" "$t/log"; then echo redo=yes; else echo redo=no; fi
echo "big=$(psql -X -h "$t" -U postgres -d postgres -At -c "SELECT c.relallvisible || '/' || c.relpages || '/' || s.n_dead_tup FROM pg_class c JOIN pg_stat_user_tables s ON s.relid = c.oid WHERE c.relname = 'big'")"
pg_ctl -D "$d" -m fast -w stop >/dev/null
`

func checkSeed(t *testing.T, ctx context.Context, d runtime.Driver, vol string) map[string]string {
	t.Helper()
	out, err := d.RunHelper(ctx, runtime.HelperSpec{
		Image: "postgres:17", User: "postgres",
		Cmd:    []string{"sh", "-c", seedCheckScript, "seed-check", "/seed/data"},
		Mounts: []runtime.Mount{{Volume: vol, Target: "/seed"}},
	})
	if err != nil {
		t.Fatalf("inspect seed %s: %v", vol, err)
	}
	kv := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			kv[k] = v
		}
	}
	return kv
}

func settleITSource(t *testing.T, ctx context.Context) (SeedSpec, *pgx.Conn) {
	t.Helper()
	host, port, networkName, hostConn := pgctltest.StartSourcePG(t, ctx)
	conn, err := pgx.Connect(ctx, hostConn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	// unfrozen, unhinted rows with dead tuples: what an online copy carries
	if _, err := conn.Exec(ctx, `CREATE TABLE big (id int PRIMARY KEY, v int, pad text) WITH (autovacuum_enabled = off);
		INSERT INTO big SELECT g, g % 100, repeat('x', 100) FROM generate_series(1, 200000) g;
		CREATE INDEX big_v ON big (v);
		UPDATE big SET v = v + 1 WHERE id % 10 = 0`); err != nil {
		t.Fatal(err)
	}
	return SeedSpec{Image: "postgres:17", Network: networkName, Host: host, Port: port,
		User: "postgres", Password: "secret"}, conn
}

func itVolume(t *testing.T, ctx context.Context, d runtime.Driver, name string) string {
	t.Helper()
	d.RemoveVolume(ctx, name)
	if err := d.CreateVolume(ctx, name, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.RemoveVolume(context.Background(), name) })
	return name
}

// Settle survives a production configuration no throwaway container could
// start with: preload libraries the image lacks, huge pages, a 64 GB buffer
// pool, TLS with certificates elsewhere, a log directory it cannot create,
// archiving, an hba file outside the data dir, and a login event trigger. The
// result is a cleanly shut down, frozen seed that starts without redo.
func TestSettleProductionConfigIT(t *testing.T) {
	if os.Getenv("PGOVERLAY_IT") != "1" {
		t.Skip("set PGOVERLAY_IT=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	spec, conn := settleITSource(t, ctx)
	if _, err := conn.Exec(ctx, `CREATE FUNCTION refuse_login() RETURNS event_trigger LANGUAGE plpgsql AS
		$$BEGIN IF current_setting('application_name') IN ('pgoverlay-settle', 'vacuumdb') THEN RAISE EXCEPTION 'login refused'; END IF; END$$;
		CREATE EVENT TRIGGER refuse_login ON login EXECUTE FUNCTION refuse_login();
		ALTER DATABASE template1 SET statement_timeout = '1ms';
		ALTER DATABASE template1 SET default_transaction_read_only = on`); err != nil {
		t.Fatal(err)
	}
	appendSourceConfig(t, ctx, conn,
		"shared_preload_libraries = 'pg_stat_statements,timescaledb,pg_cron'",
		"huge_pages = on",
		"shared_buffers = '64GB'",
		"port = 5433",
		"ssl = on",
		"ssl_cert_file = '/etc/ssl/certs/ssl-cert-snakeoil.pem'",
		"ssl_key_file = '/etc/ssl/private/ssl-cert-snakeoil.key'",
		"logging_collector = on",
		"log_directory = '/var/log/postgresql/missing'",
		"archive_mode = on",
		"archive_command = '/bin/false'",
		"hba_file = '/etc/postgresql/17/main/pg_hba.conf'",
		"max_wal_size = '8GB'",
		"wal_keep_size = '4GB'",
		"maintenance_work_mem = '2GB'",
		"max_parallel_maintenance_workers = 8",
	)

	d, err := runtime.NewDockerDriver()
	if err != nil {
		t.Fatal(err)
	}
	spec.Volume = itVolume(t, ctx, d, "pgoverlay-it-settle-prod")
	if err := Seed(ctx, d, spec); err != nil {
		t.Fatal(err)
	}
	if err := Settle(ctx, d, spec); err != nil {
		t.Fatal(err)
	}
	// no WAL recycled into the seed despite max_wal_size and wal_keep_size
	out, err := d.RunHelper(ctx, runtime.HelperSpec{
		Image: "postgres:17", User: "postgres",
		Cmd:    []string{"sh", "-c", `ls /seed/data/pg_wal | grep -c '^[0-9A-F]\{24\}$'`},
		Mounts: []runtime.Mount{{Volume: spec.Volume, Target: "/seed", ReadOnly: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := strconv.Atoi(strings.TrimSpace(out)); n < 1 || n > 2 {
		t.Errorf("settled seed keeps %q WAL segments, want 1-2", strings.TrimSpace(out))
	}
	got := checkSeed(t, ctx, d, spec.Volume)
	if got["state"] != "shut down" || got["backup_label"] != "absent" {
		t.Fatalf("seed = %v, want a clean shutdown with no backup_label", got)
	}
	if got["start"] != "database system was shut down at" || got["redo"] != "no" {
		t.Fatalf("seed started with %v, want no recovery", got)
	}
	// VACUUM ran on big despite the login trigger that refuses the settle's
	// connections and the per-database timeouts: every page all-visible
	// (the source had none, autovacuum is off on big), no dead tuples
	f := strings.Split(got["big"], "/")
	if len(f) != 3 || f[0] != f[1] || f[0] == "0" || f[2] != "0" {
		t.Fatalf("big relallvisible/relpages/n_dead_tup = %q, want every page all-visible and no dead tuples", got["big"])
	}
}

// Configuration a branch could not start with either fails the seed, early,
// with the server's own words, instead of every branch 90 seconds into its
// readiness wait.
func TestSettleBrokenConfigFailsSeedIT(t *testing.T) {
	if os.Getenv("PGOVERLAY_IT") != "1" {
		t.Skip("set PGOVERLAY_IT=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	spec, conn := settleITSource(t, ctx)
	appendSourceConfig(t, ctx, conn, "include '/etc/postgresql/17/main/conf.d/tuning.conf'")

	d, err := runtime.NewDockerDriver()
	if err != nil {
		t.Fatal(err)
	}
	spec.Volume = itVolume(t, ctx, d, "pgoverlay-it-settle-broken")
	if err := Seed(ctx, d, spec); err != nil {
		t.Fatal(err)
	}
	err = Settle(ctx, d, spec)
	if !errors.Is(err, ErrSeedFailed) {
		t.Fatalf("Settle = %v, want ErrSeedFailed", err)
	}
	if !regexp.MustCompile(`tuning\.conf.*No such file`).MatchString(err.Error()) {
		t.Fatalf("error does not name the missing include:\n%v", err)
	}
}
