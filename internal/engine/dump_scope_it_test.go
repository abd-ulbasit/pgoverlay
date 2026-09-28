package engine

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/abd-ulbasit/pgoverlay/internal/pgctl/pgctltest"
	"github.com/abd-ulbasit/pgoverlay/internal/registry"
	"github.com/abd-ulbasit/pgoverlay/internal/runtime"
)

// TestDumpSeedScopedRestoresRolesAndExtensions covers what real managed
// databases put in a scoped dump: a row-level security policy naming a role,
// a column typed by an extension installed in public, and an index using an
// extension that lives in a schema outside the scope. The scope is written
// as PUBLIC, which pg_dump case-folds to public. Each of these used to abort
// the seed under ON_ERROR_STOP.
func TestDumpSeedScopedRestoresRolesAndExtensions(t *testing.T) {
	if os.Getenv("PGOVERLAY_IT") != "1" {
		t.Skip("set PGOVERLAY_IT=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	host, port, network, hostConn := pgctltest.StartSourcePG(t, ctx)
	mustExec(t, ctx, hostConn, `CREATE ROLE app_reader NOLOGIN;
		CREATE EXTENSION citext;
		CREATE SCHEMA ext;
		CREATE EXTENSION pg_trgm WITH SCHEMA ext;
		CREATE TABLE users(id int primary key, email citext NOT NULL);
		ALTER TABLE users ENABLE ROW LEVEL SECURITY;
		CREATE POLICY readers ON users FOR SELECT TO app_reader USING (true);
		CREATE INDEX users_email_trgm ON users USING gin ((email::text) ext.gin_trgm_ops);
		INSERT INTO users SELECT i, 'User' || i || '@Example.com' FROM generate_series(1,500) i`)

	d, err := runtime.NewDockerDriver()
	if err != nil {
		t.Fatal(err)
	}
	r, err := registry.Open(t.TempDir() + "/it.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	e := New(r, d, "postgres:17")

	src := &registry.Source{Name: "dscope-main", PGVersion: "17",
		ConnHost: host, ConnPort: port, ConnUser: "postgres", ConnDB: "postgres", Network: network,
		SeedVia: registry.SeedViaDump, DumpSchemas: []string{"PUBLIC"}}
	if err := e.AddSource(ctx, src, "secret"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.RemoveVolume(context.Background(), src.Volume) })

	b, err := e.CreateBranch(ctx, "dscope-pr-1", "dscope-main", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.DestroyBranch(context.Background(), "dscope-pr-1"); err != nil {
			t.Errorf("destroy dscope-pr-1: %v", err)
		}
	})
	br := branchConn(b)

	for q, want := range map[string]int{
		`SELECT count(*) FROM users`: 500,
		// citext came along: comparisons stay case-insensitive
		`SELECT count(*) FROM users WHERE email = 'user1@example.com'`:             1,
		`SELECT count(*) FROM pg_extension WHERE extname IN ('citext', 'pg_trgm')`: 2,
		`SELECT count(*) FROM pg_indexes WHERE indexname = 'users_email_trgm'`:     1,
		// the policy and a login-less shell of its role
		`SELECT count(*) FROM pg_policies WHERE policyname = 'readers' AND 'app_reader' = ANY(roles)`: 1,
		`SELECT count(*) FROM pg_roles WHERE rolname = 'app_reader' AND NOT rolcanlogin`:              1,
	} {
		if got := mustQueryInt(t, ctx, br, q); got != want {
			t.Errorf("%s = %d, want %d", q, got, want)
		}
	}
	mustExec(t, ctx, br, `INSERT INTO users VALUES (1000, 'new@example.com')`)
}
