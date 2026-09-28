package registry

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// currentSchemaVersion is the user_version a fully migrated database must
// report: one bump per entry in migrations.
func currentSchemaVersion() int { return len(migrations) }

func userVersion(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestMigrateIdempotentAcrossReopen is the plain contract: migrating a
// database that is already migrated must be a no-op, not a re-run. Most
// migrations are not replay-safe (SQLite has no ALTER TABLE ADD COLUMN IF NOT
// EXISTS), so "no-op" has to mean "applies nothing", not "fails harmlessly".
func TestMigrateIdempotentAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reopen.db")
	for i := 1; i <= 3; i++ {
		r, err := Open(path) // Open runs migrate
		if err != nil {
			t.Fatalf("open #%d: %v", i, err)
		}
		if err := r.Close(); err != nil {
			t.Fatalf("close #%d: %v", i, err)
		}
		if v := userVersion(t, path); v != currentSchemaVersion() {
			t.Fatalf("after open #%d: user_version=%d want %d", i, v, currentSchemaVersion())
		}
	}
}

// TestMigrateConcurrentOpensAllSucceed is the regression test for the crash
// loop that took branchd down in HA mode.
//
// Every replica calls registry.Open at startup, before leader election, so two
// replicas sharing one hostPath state dir migrate the same file at the same
// time. The old migrate() read PRAGMA user_version once, outside any
// transaction, and then wrote `PRAGMA user_version=<loop counter>` — an
// absolute write, not a compare-and-set. A second opener that snapshotted 0
// before the first committed would replay migrations[0] (which is all CREATE
// ... IF NOT EXISTS, so it succeeds silently), then commit user_version=1 over
// an already-current database. That rewind is permanent: every later open,
// including a lone single-replica one, then re-ran migrateV2 against columns
// that already existed and died with "duplicate column name: expires_at".
//
// The invariant is that concurrent openers must all succeed and must leave the
// file fully migrated and openable.
func TestMigrateConcurrentOpensAllSucceed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.db")
	const openers = 4

	regs := make([]*Registry, openers)
	errs := make([]error, openers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < openers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release them together so they race on a fresh file
			regs[i], errs[i] = Open(path)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, r := range regs {
		if r != nil {
			t.Cleanup(func() { r.Close() })
		}
		if errs[i] != nil {
			t.Errorf("concurrent opener %d: Open = %v", i, errs[i])
		}
	}
	if v := userVersion(t, path); v != currentSchemaVersion() {
		t.Errorf("user_version=%d want %d: a concurrent opener rewound the schema version", v, currentSchemaVersion())
	}

	// The wedge the crash loop actually reported: once the version has been
	// rewound, even a lone restart can never open the database again.
	lone, err := Open(path)
	if err != nil {
		t.Fatalf("lone reopen after concurrent opens: %v", err)
	}
	defer lone.Close()
	if err := lone.CreateSource(&Source{Name: "main", PGVersion: "17", Volume: "pgoverlay-src-main"}); err != nil {
		t.Fatalf("registry unusable after concurrent opens: %v", err)
	}
}

// TestMigrateConcurrentOpensOnMigratedFile covers the steady-state HA case:
// the file is already current and several replicas restart onto it at once.
// Nothing should be applied and nothing should fail.
func TestMigrateConcurrentOpensOnMigratedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "warm.db")
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	const openers = 4
	errs := make([]error, openers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < openers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			var r *Registry
			r, errs[i] = Open(path)
			if r != nil {
				r.Close()
			}
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("warm opener %d: Open = %v", i, err)
		}
	}
	if v := userVersion(t, path); v != currentSchemaVersion() {
		t.Errorf("user_version=%d want %d", v, currentSchemaVersion())
	}
}

// TestIsBusyMatchesDriverError guards the retry in connect(). isBusy pattern
// matches on the driver's concrete error type, so a driver upgrade that changed
// that type would turn the SQLITE_BUSY retry into dead code silently — and the
// only symptom would be branchd replicas intermittently failing to start on a
// fresh state dir, which is exactly the kind of flake nobody traces back here.
func TestIsBusyMatchesDriverError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "busy.db")
	dsn := path + "?_pragma=busy_timeout(0)" // no waiting: we want the error

	a, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.SetMaxOpenConns(1)
	if _, err := a.Exec(`CREATE TABLE t (x)`); err != nil {
		t.Fatal(err)
	}
	tx, err := a.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO t VALUES (1)`); err != nil { // take the write lock
		t.Fatal(err)
	}

	b, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	b.SetMaxOpenConns(1)
	_, err = b.Exec(`INSERT INTO t VALUES (2)`)
	if err == nil {
		t.Fatal("second writer succeeded while the first held the lock; test no longer provokes SQLITE_BUSY")
	}
	if !isBusy(err) {
		t.Fatalf("isBusy(%v) = false, want true: the SQLITE_BUSY retry in connect() is now dead code", err)
	}
	if isBusy(nil) || isBusy(errors.New("boom")) {
		t.Error("isBusy matched a non-SQLITE_BUSY error; connect() would retry real faults")
	}
}

// TestMigrateRejectsFutureSchema: a database written by a newer build must be
// refused with a clear error rather than silently accepted. Skipping ahead
// leaves the daemon querying columns that its own schema.go never created,
// which surfaces far from the cause. This is the rolling-downgrade case.
func TestMigrateRejectsFutureSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.Exec(`PRAGMA user_version=999`); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("Open on a future schema version = nil, want error")
	}
}

// TestMigrateV12ClearsTombstonePasswords: upgrading a registry whose destroyed
// branch rows still hold passwords clears them, leaves live rows alone, and
// adds transitions.entity_name for the rows a source removal would orphan.
func TestMigrateV12ClearsTombstonePasswords(t *testing.T) {
	v12 := -1
	for i, m := range migrations {
		if m == migrateV12 {
			v12 = i
		}
	}
	if v12 < 0 {
		t.Fatal("migrateV12 is not registered")
	}
	path := filepath.Join(t.TempDir(), "pre-v12.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:v12] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version=%d`, v12)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sources (id,name,pg_version,volume,state) VALUES ('s1','main','17','v','ready');
		INSERT INTO branches (id,name,source_id,state,rw_volume,password) VALUES
		  ('b-dead','old','s1','destroyed','rw1','enc:v1:c2VjcmV0'),
		  ('b-live','new','s1','ready','rw2','livepassword0001');`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if raw := rawBranchPassword(t, r, "b-dead"); raw != "" {
		t.Fatalf("destroyed row kept its password: %q", raw)
	}
	if raw := rawBranchPassword(t, r, "b-live"); raw != "livepassword0001" {
		t.Fatalf("live row password changed: %q", raw)
	}
	var n int
	if err := r.db.QueryRow(`SELECT count(*) FROM transitions WHERE entity_name = ''`).Scan(&n); err != nil {
		t.Fatalf("transitions.entity_name missing after v12: %v", err)
	}
}
