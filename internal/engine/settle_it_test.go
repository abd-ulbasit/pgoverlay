package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/abd-ulbasit/pgoverlay/internal/pgctl"
	"github.com/abd-ulbasit/pgoverlay/internal/pgctl/pgctltest"
	"github.com/abd-ulbasit/pgoverlay/internal/registry"
	"github.com/abd-ulbasit/pgoverlay/internal/runtime"
)

// seqScanBuffers runs a sequential scan of table in the branch database db
// and returns the shared buffers it dirtied and wrote. A read that sets hint
// bits or prunes pages dirties them; on the overlay backend that write is
// what copies data into the branch.
func seqScanBuffers(t *testing.T, ctx context.Context, b *registry.Branch, db, table string) (dirtied, written int) {
	t.Helper()
	// statement_timeout as a startup option: it beats a per-database
	// setting before the first statement runs
	c, err := pgx.Connect(ctx, fmt.Sprintf("postgres://postgres:secret@localhost:%d/%s?statement_timeout=0", b.Port, db))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	for _, s := range []string{"SET enable_indexscan = off",
		"SET enable_indexonlyscan = off", "SET enable_bitmapscan = off"} {
		if _, err := c.Exec(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	var plan string
	if err := c.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) SELECT count(*) FROM "+table).Scan(&plan); err != nil {
		t.Fatal(err)
	}
	var p []struct {
		Plan struct {
			Dirtied int `json:"Shared Dirtied Blocks"`
			Written int `json:"Shared Written Blocks"`
		} `json:"Plan"`
	}
	if err := json.Unmarshal([]byte(plan), &p); err != nil || len(p) != 1 {
		t.Fatalf("plan %q: %v", plan, err)
	}
	return p[0].Plan.Dirtied, p[0].Plan.Written
}

// seedCheckpoint reads the seed's control file: its state and the location of
// its latest checkpoint.
func seedCheckpoint(t *testing.T, ctx context.Context, d runtime.Driver, vol string) (state, checkpoint string) {
	t.Helper()
	out, err := d.RunHelper(ctx, runtime.HelperSpec{
		Image: "postgres:17", User: "postgres",
		// only the two lines: the runtime keeps the last 20 of a helper's output
		Cmd:    []string{"sh", "-c", "LC_ALL=C pg_controldata /seed/data | grep -E '^(Database cluster state|Latest checkpoint location):'"},
		Mounts: []runtime.Mount{{Volume: vol, Target: "/seed", ReadOnly: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch k {
		case "Database cluster state":
			state = strings.TrimSpace(v)
		case "Latest checkpoint location":
			checkpoint = strings.TrimSpace(v)
		}
	}
	return state, checkpoint
}

// TestSeedSettleIT: a branch of a settled seed starts from the seed's clean
// shutdown checkpoint (no crash recovery) and a read in it dirties nothing,
// even on tables the source never vacuumed; a branch of an unsettled seed
// (--seed-settle=off, the control) replays WAL and dirties every page it
// reads. The source carries production settings the settle must get past.
func TestSeedSettleIT(t *testing.T) {
	if os.Getenv("PGOVERLAY_IT") != "1" {
		t.Skip("set PGOVERLAY_IT=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	host, port, network, hostConn := pgctltest.StartSourcePG(t, ctx)
	mustExec(t, ctx, hostConn, `CREATE EXTENSION pg_stat_statements;
		CREATE TABLE big (id int PRIMARY KEY, v int, pad text) WITH (autovacuum_enabled = off);
		INSERT INTO big SELECT g, g % 100, repeat('x', 100) FROM generate_series(1, 200000) g;
		CREATE INDEX big_v ON big (v);
		UPDATE big SET v = v + 1 WHERE id % 10 = 0;
		CREATE TABLE login_audit (app text);
		CREATE FUNCTION audit_login() RETURNS event_trigger LANGUAGE plpgsql AS
		$$BEGIN INSERT INTO login_audit VALUES (current_setting('application_name')); END$$;
		CREATE EVENT TRIGGER audit_login ON login EXECUTE FUNCTION audit_login()`)
	mustExec(t, ctx, hostConn, `CREATE DATABASE appdb`)
	appConn := strings.TrimSuffix(hostConn, "/postgres") + "/appdb"
	mustExec(t, ctx, appConn, `CREATE TABLE t WITH (autovacuum_enabled = off) AS SELECT g AS id, md5(g::text) AS h FROM generate_series(1, 50000) g`)
	// production settings a branch can start with (it overrides the ports,
	// sockets, hba file, TLS, logging and archiving itself) but a naive
	// settle could not, plus per-database settings that would stop a VACUUM
	mustExec(t, ctx, hostConn, `ALTER DATABASE appdb SET default_transaction_read_only = on;
		ALTER DATABASE appdb SET statement_timeout = '1ms';
		COPY (SELECT unnest(ARRAY[
			'shared_preload_libraries = ''pg_stat_statements,auto_explain''',
			'port = 5433',
			'ssl = on',
			'ssl_cert_file = ''/etc/ssl/certs/ssl-cert-snakeoil.pem''',
			'logging_collector = on',
			'log_directory = ''/var/log/postgresql/missing''',
			'archive_mode = on',
			'archive_command = ''/bin/false''',
			'hba_file = ''/etc/postgresql/17/main/pg_hba.conf''',
			'max_wal_size = ''8GB''',
			'wal_keep_size = ''4GB''',
			'max_parallel_maintenance_workers = 8'])) TO PROGRAM 'cat >> "$PGDATA/postgresql.conf"'`)

	d, err := runtime.NewDockerDriver()
	if err != nil {
		t.Fatal(err)
	}
	r, err := registry.Open(t.TempDir() + "/it.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })

	for _, tc := range []struct {
		mode    pgctl.SettleMode
		source  string
		settled bool
	}{
		{pgctl.SettleFreeze, "settle-it-frozen", true},
		{pgctl.SettleOff, "settle-it-raw", false},
	} {
		e := New(r, d, "postgres:17", WithSeedSettle(tc.mode))
		src := &registry.Source{Name: tc.source, PGVersion: "17", ConnHost: host, ConnPort: port, ConnUser: "postgres", Network: network}
		if err := e.AddSource(ctx, src, "secret"); err != nil {
			t.Fatalf("%s: %v", tc.mode, err)
		}
		t.Cleanup(func() { d.RemoveVolume(context.Background(), src.Volume) })
		state, seedCkpt := seedCheckpoint(t, ctx, d, src.Volume)
		if tc.settled != (state == "shut down") {
			t.Fatalf("%s: seed state %q", tc.mode, state)
		}

		bname := tc.source + "-b"
		b, err := e.CreateBranch(ctx, bname, tc.source, 0)
		if err != nil {
			t.Fatalf("%s: %v", tc.mode, err)
		}
		t.Cleanup(func() {
			if err := e.DestroyBranch(context.Background(), bname); err != nil {
				t.Errorf("destroy %s: %v", bname, err)
			}
		})
		// a branch that ran crash recovery has written an end-of-recovery
		// checkpoint of its own; one that started from a clean shutdown
		// still has the seed's
		var branchCkpt string
		c, err := pgx.Connect(ctx, branchConn(b))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.QueryRow(ctx, "SELECT checkpoint_lsn::text FROM pg_control_checkpoint()").Scan(&branchCkpt); err != nil {
			t.Fatal(err)
		}
		var preload string
		var settleLogins int
		if err := c.QueryRow(ctx, "SELECT current_setting('shared_preload_libraries'), (SELECT count(*) FROM login_audit WHERE app = 'pgoverlay-settle')").Scan(&preload, &settleLogins); err != nil {
			t.Fatal(err)
		}
		c.Close(ctx)
		if recovered := branchCkpt != seedCkpt; recovered == tc.settled {
			t.Errorf("%s: branch checkpoint %s, seed checkpoint %s: crash recovery %v, want %v", tc.mode, branchCkpt, seedCkpt, recovered, !tc.settled)
		}
		// the settle overrode the source's configuration only for itself
		if preload != "pg_stat_statements,auto_explain" {
			t.Errorf("%s: branch shared_preload_libraries = %q, want the source's", tc.mode, preload)
		}
		if settleLogins != 0 {
			t.Errorf("%s: the source's login event trigger ran for the settle %d time(s)", tc.mode, settleLogins)
		}

		for _, rel := range []struct{ db, table string }{{"postgres", "big"}, {"appdb", "t"}} {
			dirtied, written := seqScanBuffers(t, ctx, b, rel.db, rel.table)
			t.Logf("%s: seq scan of %s.%s dirtied %d, wrote %d shared buffers", tc.mode, rel.db, rel.table, dirtied, written)
			if tc.settled && dirtied+written != 0 {
				t.Errorf("%s: reading %s.%s in a branch of a settled seed dirtied %d and wrote %d buffers, want none", tc.mode, rel.db, rel.table, dirtied, written)
			}
			if !tc.settled && dirtied == 0 {
				t.Errorf("control: reading %s.%s in a branch of an unsettled seed dirtied nothing; the check cannot tell settled from unsettled", rel.db, rel.table)
			}
		}
	}
}
