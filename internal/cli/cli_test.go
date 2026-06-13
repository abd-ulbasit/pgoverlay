package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abd-ulbasit/pgbranch/internal/api"
	"github.com/abd-ulbasit/pgbranch/internal/engine"
	"github.com/abd-ulbasit/pgbranch/internal/registry"
)

func TestCommandTree(t *testing.T) {
	root := NewRootCmd()
	for _, path := range [][]string{
		{"source", "add"}, {"source", "ls"}, {"source", "rm"}, {"source", "refresh"},
		{"source", "set-mask"}, {"source", "get-mask"},
		{"branch", "create"}, {"branch", "ls"}, {"branch", "destroy"}, {"branch", "reset"},
		{"connect"}, {"diff"}, {"doctor"}, {"gc"},
	} {
		cmd, _, err := root.Find(path)
		if err != nil || cmd.Name() != path[len(path)-1] {
			t.Fatalf("command %v not found: %v", path, err)
		}
	}
	if root.PersistentFlags().Lookup("server") == nil {
		t.Fatal("--server flag missing")
	}
	if f, _, _ := root.Find([]string{"branch", "create"}); f.Flags().Lookup("ttl") == nil {
		t.Fatal("branch create --ttl flag missing")
	}
	if f, _, _ := root.Find([]string{"branch", "create"}); f.Flags().Lookup("from-branch") == nil {
		t.Fatal("branch create --from-branch flag missing")
	}
	if f, _, _ := root.Find([]string{"source", "add"}); f.Flags().Lookup("via") == nil {
		t.Fatal("source add --via flag missing")
	}
	if f, _, _ := root.Find([]string{"source", "add"}); f.Flags().Lookup("dump-schema") == nil {
		t.Fatal("source add --dump-schema flag missing")
	}
	// help renders without side effects
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
}

// run executes the CLI with args and returns its stdout.
func run(t *testing.T, args ...string) string {
	t.Helper()
	root := NewRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		t.Fatalf("pgb %v: %v", args, err)
	}
	return buf.String()
}

func TestServerModeBranchLs(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("auth header %q", r.Header.Get("Authorization"))
		}
		if r.Method != "GET" || r.URL.Path != "/v1/branches" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		json.NewEncoder(w).Encode([]api.Branch{{Name: "pr-9", State: "ready", Port: 32788, CreatedAt: "2026-06-10"}})
	}))
	defer ts.Close()
	t.Setenv("PGBRANCH_TOKEN", "tok")

	out := run(t, "branch", "ls", "--server", ts.URL)
	if !strings.Contains(out, "pr-9") || !strings.Contains(out, "32788") {
		t.Fatalf("output %q", out)
	}
}

func TestServerModeBranchLsUsage(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/branches":
			json.NewEncoder(w).Encode([]api.Branch{{Name: "pr-9", State: "ready", Port: 32788, CreatedAt: "2026-06-10"}})
		case "/v1/branches/pr-9/usage":
			json.NewEncoder(w).Encode(map[string]int64{"bytes": 5242880})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer ts.Close()
	t.Setenv("PGBRANCH_TOKEN", "tok")

	// without --usage there is no SIZE column (it costs a helper run per branch)
	out := run(t, "branch", "ls", "--server", ts.URL)
	if strings.Contains(out, "SIZE") {
		t.Fatalf("SIZE column without --usage: %q", out)
	}
	out = run(t, "branch", "ls", "--usage", "--server", ts.URL)
	if !strings.Contains(out, "SIZE") || !strings.Contains(out, "5.0 MiB") {
		t.Fatalf("output %q, want SIZE column with 5.0 MiB", out)
	}
}

func TestServerModeConnectPrintsDirectAndProxyURLs(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/branches/pr-9" {
			t.Errorf("path %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(api.Branch{
			Name: "pr-9", State: "ready", Host: "10.0.0.7", Port: 32788,
			User: "postgres", Database: "postgres", ProxyDatabase: "postgres@pr-9",
		})
	}))
	defer ts.Close()
	t.Setenv("PGBRANCH_TOKEN", "tok")

	out := run(t, "connect", "pr-9", "--server", ts.URL)
	// direct URL uses the branch's recorded host (pod IP on k8s)
	if !strings.Contains(out, "@10.0.0.7:32788/postgres") {
		t.Fatalf("direct URL missing branch host in %q", out)
	}
	// proxy URL keeps the server's host
	if !strings.Contains(out, ":6432/postgres@pr-9") {
		t.Fatalf("proxy URL missing in %q", out)
	}
}

// Rotate mode: the server returns a per-branch password; connect must embed
// it in both printed DSNs so they are copy-pasteable.
func TestServerModeConnectIncludesRotatedPassword(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(api.Branch{
			Name: "pr-9", State: "ready", Host: "10.0.0.7", Port: 32788,
			User: "postgres", Password: "feedfacefeedfacefeedfacefeedface",
			Database: "postgres", ProxyDatabase: "postgres@pr-9",
		})
	}))
	defer ts.Close()
	t.Setenv("PGBRANCH_TOKEN", "tok")

	out := run(t, "connect", "pr-9", "--server", ts.URL)
	if !strings.Contains(out, "postgres://postgres:feedfacefeedfacefeedfacefeedface@10.0.0.7:32788/postgres") {
		t.Fatalf("direct URL missing password in %q", out)
	}
	if !strings.Contains(out, "postgres:feedfacefeedfacefeedfacefeedface@") ||
		!strings.Contains(out, ":6432/postgres@pr-9") {
		t.Fatalf("proxy URL missing password in %q", out)
	}
}

// Inherit mode keeps the historical password-less DSNs.
func TestServerModeConnectNoPasswordInheritMode(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(api.Branch{
			Name: "pr-9", State: "ready", Host: "10.0.0.7", Port: 32788,
			User: "postgres", Database: "postgres", ProxyDatabase: "postgres@pr-9",
		})
	}))
	defer ts.Close()
	t.Setenv("PGBRANCH_TOKEN", "tok")

	out := run(t, "connect", "pr-9", "--server", ts.URL)
	if !strings.Contains(out, "postgres://postgres@10.0.0.7:32788/postgres") {
		t.Fatalf("direct URL changed in inherit mode: %q", out)
	}
}

// branch ls never shows the password, even when the server sends one.
func TestServerModeBranchLsHidesPassword(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]api.Branch{{
			Name: "pr-9", State: "ready", Port: 32788, CreatedAt: "2026-06-10",
			Password: "feedfacefeedfacefeedfacefeedface",
		}})
	}))
	defer ts.Close()
	t.Setenv("PGBRANCH_TOKEN", "tok")

	out := run(t, "branch", "ls", "--server", ts.URL)
	if strings.Contains(out, "feedface") {
		t.Fatalf("branch ls leaked the password: %q", out)
	}
}

func TestServerModeConnectFallsBackToServerHost(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(api.Branch{
			Name: "pr-9", State: "ready", Port: 32788,
			User: "postgres", Database: "postgres", ProxyDatabase: "postgres@pr-9",
		})
	}))
	defer ts.Close()
	t.Setenv("PGBRANCH_TOKEN", "tok")

	out := run(t, "connect", "pr-9", "--server", ts.URL)
	// no host from the server (pre-v3 row): fall back to the server's host
	if !strings.Contains(out, ":32788/postgres") || strings.Contains(out, "@:32788") {
		t.Fatalf("direct URL fallback broken in %q", out)
	}
}

func TestServerModeFromEnv(t *testing.T) {
	called := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		json.NewEncoder(w).Encode([]api.Branch{})
	}))
	defer ts.Close()
	t.Setenv("PGBRANCH_SERVER", ts.URL)
	t.Setenv("PGBRANCH_TOKEN", "tok")

	run(t, "branch", "ls")
	if !called {
		t.Fatal("PGBRANCH_SERVER env did not enable server mode")
	}
}

func TestServerModeSetMask(t *testing.T) {
	dir := t.TempDir()
	f1 := filepath.Join(dir, "emails.sql")
	f2 := filepath.Join(dir, "names.sql")
	if err := os.WriteFile(f1, []byte("UPDATE users SET email = 'x@invalid'"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f2, []byte("UPDATE users SET name = 'redacted'"), 0o644); err != nil {
		t.Fatal(err)
	}

	var gotMethod, gotPath string
	var got []api.MaskScript
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		json.NewDecoder(r.Body).Decode(&got)
		json.NewEncoder(w).Encode(got)
	}))
	defer ts.Close()
	t.Setenv("PGBRANCH_TOKEN", "tok")

	out := run(t, "source", "set-mask", "main", f1, f2, "--server", ts.URL)
	if gotMethod != "PUT" || gotPath != "/v1/sources/main/mask" {
		t.Fatalf("%s %s", gotMethod, gotPath)
	}
	// order = argv, script name = basename
	if len(got) != 2 || got[0].Name != "emails.sql" || got[1].Name != "names.sql" {
		t.Fatalf("sent %v", got)
	}
	if got[0].SQL != "UPDATE users SET email = 'x@invalid'" || got[1].SQL != "UPDATE users SET name = 'redacted'" {
		t.Fatalf("sent SQL %v", got)
	}
	if !strings.Contains(out, "2") {
		t.Fatalf("output %q", out)
	}
}

func TestServerModeGetMask(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v1/sources/main/mask" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		json.NewEncoder(w).Encode([]api.MaskScript{{Name: "emails.sql", SQL: "..."}, {Name: "names.sql", SQL: "..."}})
	}))
	defer ts.Close()
	t.Setenv("PGBRANCH_TOKEN", "tok")

	out := run(t, "source", "get-mask", "main", "--server", ts.URL)
	if !strings.Contains(out, "emails.sql") || !strings.Contains(out, "names.sql") {
		t.Fatalf("output %q", out)
	}
}

func TestLocalModeSetAndGetMask(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PGBRANCH_HOME", home)
	t.Setenv("PGBRANCH_SERVER", "")

	// seed a source row the commands can resolve
	reg, err := registry.Open(filepath.Join(home, "pgbranch.db"))
	if err != nil {
		t.Fatal(err)
	}
	src := &registry.Source{Name: "main", PGVersion: "17", Volume: "v"}
	if err := reg.CreateSource(src); err != nil {
		t.Fatal(err)
	}
	reg.Close()

	f := filepath.Join(t.TempDir(), "mask.sql")
	if err := os.WriteFile(f, []byte("UPDATE t SET x=1"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "source", "set-mask", "main", f)

	out := run(t, "source", "get-mask", "main")
	if !strings.Contains(out, "mask.sql") {
		t.Fatalf("output %q", out)
	}

	reg, err = registry.Open(filepath.Join(home, "pgbranch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	scripts, err := reg.GetMaskScripts(src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(scripts) != 1 || scripts[0].Name != "mask.sql" || scripts[0].SQL != "UPDATE t SET x=1" {
		t.Fatalf("stored %v", scripts)
	}
}

func TestServerModeBranchCreateTTL(t *testing.T) {
	var got api.CreateBranchRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(api.Branch{Name: got.Name, State: "ready", Port: 1, ProxyDatabase: "postgres@" + got.Name})
	}))
	defer ts.Close()
	t.Setenv("PGBRANCH_TOKEN", "tok")

	run(t, "branch", "create", "pr-9", "--from", "main", "--ttl", "24h", "--server", ts.URL)
	if got.Name != "pr-9" || got.Source != "main" || got.TTLSeconds != 86400 {
		t.Fatalf("request %+v", got)
	}
	if got.Parent != "" {
		t.Fatalf("request %+v: --from must not send a parent", got)
	}
}

func TestServerModeBranchCreateFromBranch(t *testing.T) {
	var got api.CreateBranchRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(api.Branch{Name: got.Name, Parent: got.Parent, State: "ready", Port: 1, ProxyDatabase: "postgres@" + got.Name})
	}))
	defer ts.Close()
	t.Setenv("PGBRANCH_TOKEN", "tok")

	run(t, "branch", "create", "pr-9", "--from-branch", "pr-1", "--server", ts.URL)
	if got.Name != "pr-9" || got.Parent != "pr-1" || got.Source != "" {
		t.Fatalf("request %+v", got)
	}
}

func TestBranchCreateFromFlagsMutuallyExclusive(t *testing.T) {
	for name, args := range map[string][]string{
		"both":    {"branch", "create", "pr-9", "--from", "main", "--from-branch", "pr-1"},
		"neither": {"branch", "create", "pr-9"},
	} {
		root := NewRootCmd()
		root.SetOut(&bytes.Buffer{})
		root.SetErr(&bytes.Buffer{})
		root.SetArgs(args)
		err := root.Execute()
		if err == nil || !strings.Contains(err.Error(), "--from") || !strings.Contains(err.Error(), "--from-branch") {
			t.Errorf("%s: err=%v, want one explaining --from/--from-branch exclusivity", name, err)
		}
	}
}

func TestSourceAddDumpSchemaRequiresViaDump(t *testing.T) {
	t.Setenv("PGPASSWORD", "secret")
	for name, args := range map[string][]string{
		"default via": {"source", "add", "main", "--host", "h", "--dump-schema", "public"},
		"basebackup":  {"source", "add", "main", "--host", "h", "--via", "basebackup", "--dump-schema", "public"},
	} {
		root := NewRootCmd()
		root.SetOut(&bytes.Buffer{})
		root.SetErr(&bytes.Buffer{})
		root.SetArgs(args)
		err := root.Execute()
		if err == nil || !strings.Contains(err.Error(), "--dump-schema") || !strings.Contains(err.Error(), "--via dump") {
			t.Errorf("%s: err=%v, want one explaining --dump-schema needs --via dump", name, err)
		}
	}
}

func TestServerModeSourceAddViaDump(t *testing.T) {
	var got api.CreateSourceRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(api.Source{Name: got.Name, Via: got.Via, State: "ready"})
	}))
	defer ts.Close()
	t.Setenv("PGBRANCH_TOKEN", "tok")
	t.Setenv("PGPASSWORD", "secret")

	run(t, "source", "add", "prod", "--via", "dump",
		"--dump-schema", "public", "--dump-schema", "audit",
		"--host", "db.proj.supabase.co", "--user", "postgres", "--pg-version", "17",
		"--server", ts.URL)
	if got.Name != "prod" || got.Via != "dump" {
		t.Fatalf("request %+v", got)
	}
	if len(got.DumpSchemas) != 2 || got.DumpSchemas[0] != "public" || got.DumpSchemas[1] != "audit" {
		t.Fatalf("dump_schemas=%v want [public audit]", got.DumpSchemas)
	}
	if got.Password != "secret" {
		t.Fatalf("password=%q", got.Password)
	}
}

func TestServerModeBranchLsShowsParent(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]api.Branch{
			{Name: "pr-1", State: "ready", Port: 1, CreatedAt: "2026-06-11"},
			{Name: "pr-2", Parent: "pr-1", State: "ready", Port: 2, CreatedAt: "2026-06-11"},
		})
	}))
	defer ts.Close()
	t.Setenv("PGBRANCH_TOKEN", "tok")

	out := run(t, "branch", "ls", "--server", ts.URL)
	if !strings.Contains(out, "PARENT") {
		t.Fatalf("no PARENT column: %q", out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("output %q", out)
	}
	if !strings.Contains(lines[2], "pr-2") || !strings.Contains(lines[2], "pr-1") {
		t.Fatalf("child row lacks parent: %q", lines[2])
	}
}

func TestServerModeDiff(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v1/branches/pr-7/diff" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		json.NewEncoder(w).Encode(engine.DiffResult{
			SchemaDiff: "@@ -1,2 +1,3 @@\n CREATE TABLE users (\n+    extra text,\n     id integer\n",
			Tables: []engine.TableDelta{
				{Table: "diffdemo", BaseRows: 0, BranchRows: 100, Delta: 100},
				{Table: "untouched", BaseRows: 50, BranchRows: 50, Delta: 0},
				{Table: "users", BaseRows: 1000, BranchRows: 990, Delta: -10},
			},
		})
	}))
	defer ts.Close()

	out := run(t, "diff", "pr-7", "--server", ts.URL)
	want := `@@ -1,2 +1,3 @@
 CREATE TABLE users (
+    extra text,
     id integer

TABLE     BASE  BRANCH  DELTA
diffdemo  0     100     +100
users     1000  990     -10
(row counts are planner estimates)
`
	if out != want {
		t.Errorf("pgb diff output:\n%q\nwant:\n%q", out, want)
	}

	// --all also lists tables without row-count changes
	out = run(t, "diff", "pr-7", "--all", "--server", ts.URL)
	if !strings.Contains(out, "untouched  50    50      0") {
		t.Errorf("pgb diff --all missing unchanged table:\n%s", out)
	}
}

// runErr executes the CLI and returns stdout plus the command error (for
// commands like `pgb doctor` that exit non-zero on drift).
func runErr(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := NewRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(args)
	err := root.Execute()
	return buf.String(), err
}

// pgb doctor against a drifted fake server: prints the plan and exits non-zero.
func TestServerModeDoctorDriftExitsNonZero(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v1/reconcile/plan" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		json.NewEncoder(w).Encode(engine.ReconcilePlan{Actions: []engine.Action{
			{Kind: engine.ActionFailStuck, Target: "stuck-1", Reason: "stuck in creating longer than 10m0s"},
			{Kind: engine.ActionGCVolume, Target: "pgbranch-br-orphan-rw", Reason: "managed volume owned by no live branch or source"},
		}})
	}))
	defer ts.Close()
	t.Setenv("PGBRANCH_TOKEN", "tok")

	out, err := runErr(t, "doctor", "--server", ts.URL)
	if err == nil {
		t.Fatal("doctor exited 0 on drift; want non-zero")
	}
	if !strings.Contains(out, "stuck-1") || !strings.Contains(out, "pgbranch-br-orphan-rw") {
		t.Fatalf("doctor output missing drift rows: %q", out)
	}
	if !strings.Contains(out, "fail_stuck") || !strings.Contains(out, "gc_volume") {
		t.Fatalf("doctor output missing action kinds: %q", out)
	}
}

// pgb doctor against a clean fake server: prints "no drift", exits zero.
func TestServerModeDoctorCleanExitsZero(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(engine.ReconcilePlan{})
	}))
	defer ts.Close()
	t.Setenv("PGBRANCH_TOKEN", "tok")

	out, err := runErr(t, "doctor", "--server", ts.URL)
	if err != nil {
		t.Fatalf("doctor exited non-zero on a clean system: %v (out %q)", err, out)
	}
	if !strings.Contains(out, "no drift") {
		t.Fatalf("doctor output %q", out)
	}
}

// pgb gc applies and reports the actions taken.
func TestServerModeGCApplies(t *testing.T) {
	var gotMethod, gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		json.NewEncoder(w).Encode(engine.ReconcilePlan{Actions: []engine.Action{
			{Kind: engine.ActionReap, Target: "expired-1", Reason: "ttl expired"},
		}})
	}))
	defer ts.Close()
	t.Setenv("PGBRANCH_TOKEN", "tok")

	out := run(t, "gc", "--server", ts.URL)
	if gotMethod != "POST" || gotPath != "/v1/reconcile" {
		t.Fatalf("%s %s", gotMethod, gotPath)
	}
	if !strings.Contains(out, "expired-1") || !strings.Contains(out, "applied") {
		t.Fatalf("gc output %q", out)
	}
}

func TestServerModeDiffNoChanges(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(engine.DiffResult{Tables: []engine.TableDelta{
			{Table: "users", BaseRows: 10, BranchRows: 10, Delta: 0},
		}})
	}))
	defer ts.Close()

	out := run(t, "diff", "pr-7", "--server", ts.URL)
	want := "schema: no differences\ntables: no row-count changes\n"
	if out != want {
		t.Errorf("pgb diff output: %q want %q", out, want)
	}
}
