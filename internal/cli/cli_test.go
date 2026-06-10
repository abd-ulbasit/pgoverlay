package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/abd-ulbasit/pgbranch/internal/api"
)

func TestCommandTree(t *testing.T) {
	root := NewRootCmd()
	for _, path := range [][]string{
		{"source", "add"}, {"source", "ls"}, {"source", "rm"}, {"source", "refresh"},
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
