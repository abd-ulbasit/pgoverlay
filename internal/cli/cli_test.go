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
	"github.com/abd-ulbasit/pgbranch/internal/registry"
)

func TestCommandTree(t *testing.T) {
	root := NewRootCmd()
	for _, path := range [][]string{
		{"source", "add"}, {"source", "ls"}, {"source", "rm"}, {"source", "refresh"},
		{"source", "set-mask"}, {"source", "get-mask"},
		{"branch", "create"}, {"branch", "ls"}, {"branch", "destroy"}, {"branch", "reset"},
		{"connect"},
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
}
