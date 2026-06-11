// Package actiontest exercises the composite GitHub Action's shell entrypoints
// (action/entrypoint.sh, action/destroy/entrypoint.sh) against an httptest
// stub branchd, the same way a workflow run would invoke them.
package actiontest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)

func scriptPath(t *testing.T, rel string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("entrypoint missing: %v", err)
	}
	return p
}

func requireTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"bash", "curl", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
}

type stub struct {
	mu        sync.Mutex
	auths     []string
	creates   []map[string]any
	deletes   []string
	getStates []string // consumed one per GET; last repeats
	create    func(w http.ResponseWriter, body map[string]any)
	delete    func(w http.ResponseWriter)
	ts        *httptest.Server
}

func newStub(t *testing.T) *stub {
	s := &stub{getStates: []string{"ready"}}
	branch := func(name, state string) map[string]any {
		return map[string]any{
			"name": name, "state": state, "host": "10.1.2.3", "port": 31999,
			"user": "appuser", "database": "appdb", "proxy_database": "appdb@" + name,
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/branches", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.auths = append(s.auths, r.Header.Get("Authorization"))
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		s.creates = append(s.creates, body)
		if s.create != nil {
			s.create(w, body)
			return
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(branch(body["name"].(string), s.getStates[0]))
	})
	mux.HandleFunc("GET /v1/branches/{name}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.auths = append(s.auths, r.Header.Get("Authorization"))
		if len(s.getStates) > 1 {
			s.getStates = s.getStates[1:]
		}
		json.NewEncoder(w).Encode(branch(r.PathValue("name"), s.getStates[0]))
	})
	mux.HandleFunc("DELETE /v1/branches/{name}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.auths = append(s.auths, r.Header.Get("Authorization"))
		s.deletes = append(s.deletes, r.PathValue("name"))
		if s.delete != nil {
			s.delete(w)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	s.ts = httptest.NewServer(mux)
	t.Cleanup(s.ts.Close)
	return s
}

// run executes a script with the action's env contract and returns combined
// output, the parsed GITHUB_OUTPUT key/values, and the error (nil on exit 0).
func run(t *testing.T, script string, env map[string]string) (string, map[string]string, error) {
	t.Helper()
	outFile := filepath.Join(t.TempDir(), "github_output")
	cmd := exec.Command("bash", script)
	cmd.Env = append(os.Environ(),
		"GITHUB_OUTPUT="+outFile,
		"GITHUB_RUN_ID=4242",
		"PGBRANCH_POLL_INTERVAL=0", // no real sleeps in tests
	)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	outputs := map[string]string{}
	if data, rerr := os.ReadFile(outFile); rerr == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if k, v, ok := strings.Cut(line, "="); ok {
				outputs[k] = v
			}
		}
	}
	return string(out), outputs, err
}

func TestCreateHappyPath(t *testing.T) {
	requireTools(t)
	s := newStub(t)
	s.getStates = []string{"creating", "creating", "ready"} // create returns creating; polls reach ready
	script := scriptPath(t, "action/entrypoint.sh")

	out, outputs, err := run(t, script, map[string]string{
		"PGBRANCH_SERVER": s.ts.URL + "/", // trailing slash tolerated
		"PGBRANCH_TOKEN":  "act-tok",
		"PGBRANCH_SOURCE": "staging",
		"PGBRANCH_TTL":    "900",
	})
	if err != nil {
		t.Fatalf("entrypoint failed: %v\n%s", err, out)
	}

	s.mu.Lock()
	if len(s.creates) != 1 {
		t.Fatalf("creates = %d, want 1", len(s.creates))
	}
	create := s.creates[0]
	for _, a := range s.auths {
		if a != "Bearer act-tok" {
			t.Errorf("auth = %q", a)
		}
	}
	s.mu.Unlock()

	if create["source"] != "staging" {
		t.Errorf("source = %v", create["source"])
	}
	if ttl, ok := create["ttl_seconds"].(float64); !ok || ttl != 900 {
		t.Errorf("ttl_seconds = %v (must be a JSON number)", create["ttl_seconds"])
	}
	name, _ := create["name"].(string)
	if !nameRe.MatchString(name) || !strings.HasPrefix(name, "t-") {
		t.Errorf("generated name %q", name)
	}

	want := map[string]string{"branch": name, "host": "10.1.2.3", "port": "31999", "database": "appdb"}
	for k, v := range want {
		if outputs[k] != v {
			t.Errorf("output %s = %q, want %q", k, outputs[k], v)
		}
	}
	if _, ok := outputs["password"]; ok {
		t.Error("password must never be an output")
	}
	if strings.Contains(out, "act-tok") {
		t.Error("token leaked into the log")
	}
}

func TestCreateExplicitName(t *testing.T) {
	requireTools(t)
	s := newStub(t)
	script := scriptPath(t, "action/entrypoint.sh")
	out, outputs, err := run(t, script, map[string]string{
		"PGBRANCH_SERVER": s.ts.URL,
		"PGBRANCH_TOKEN":  "act-tok",
		"PGBRANCH_BRANCH": "pr-77-ci",
	})
	if err != nil {
		t.Fatalf("entrypoint failed: %v\n%s", err, out)
	}
	if outputs["branch"] != "pr-77-ci" {
		t.Errorf("branch output = %q", outputs["branch"])
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.creates[0]["name"] != "pr-77-ci" {
		t.Errorf("created name = %v", s.creates[0]["name"])
	}
	// default source/ttl
	if s.creates[0]["source"] != "main" {
		t.Errorf("default source = %v", s.creates[0]["source"])
	}
	if ttl := s.creates[0]["ttl_seconds"].(float64); ttl != 3600 {
		t.Errorf("default ttl = %v", ttl)
	}
}

func TestCreateServerError(t *testing.T) {
	requireTools(t)
	s := newStub(t)
	s.create = func(w http.ResponseWriter, _ map[string]any) {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":"branch exists"}`))
	}
	script := scriptPath(t, "action/entrypoint.sh")
	out, _, err := run(t, script, map[string]string{
		"PGBRANCH_SERVER": s.ts.URL, "PGBRANCH_TOKEN": "t",
	})
	if err == nil {
		t.Fatalf("entrypoint succeeded on a 409:\n%s", out)
	}
	if !strings.Contains(out, "branch exists") {
		t.Errorf("error body not surfaced:\n%s", out)
	}
}

func TestCreateNeverReady(t *testing.T) {
	requireTools(t)
	s := newStub(t)
	s.getStates = []string{"creating"}
	script := scriptPath(t, "action/entrypoint.sh")
	out, _, err := run(t, script, map[string]string{
		"PGBRANCH_SERVER": s.ts.URL, "PGBRANCH_TOKEN": "t",
		"PGBRANCH_POLL_MAX": "3",
	})
	if err == nil {
		t.Fatalf("entrypoint succeeded although the branch never became ready:\n%s", out)
	}
	if !strings.Contains(out, "not ready") {
		t.Errorf("missing not-ready diagnostic:\n%s", out)
	}
}

func TestCreateMissingEnv(t *testing.T) {
	requireTools(t)
	script := scriptPath(t, "action/entrypoint.sh")
	if out, _, err := run(t, script, map[string]string{"PGBRANCH_TOKEN": "t"}); err == nil {
		t.Fatalf("entrypoint succeeded without PGBRANCH_SERVER:\n%s", out)
	}
}

func TestDestroy(t *testing.T) {
	requireTools(t)
	script := scriptPath(t, "action/destroy/entrypoint.sh")

	t.Run("deletes the branch", func(t *testing.T) {
		s := newStub(t)
		out, _, err := run(t, script, map[string]string{
			"PGBRANCH_SERVER": s.ts.URL, "PGBRANCH_TOKEN": "act-tok", "PGBRANCH_BRANCH": "pr-77-ci",
		})
		if err != nil {
			t.Fatalf("destroy failed: %v\n%s", err, out)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.deletes) != 1 || s.deletes[0] != "pr-77-ci" {
			t.Errorf("deletes = %v", s.deletes)
		}
		if s.auths[0] != "Bearer act-tok" {
			t.Errorf("auth = %q", s.auths[0])
		}
	})

	t.Run("404 is success (already gone)", func(t *testing.T) {
		s := newStub(t)
		s.delete = func(w http.ResponseWriter) { w.WriteHeader(http.StatusNotFound) }
		if out, _, err := run(t, script, map[string]string{
			"PGBRANCH_SERVER": s.ts.URL, "PGBRANCH_TOKEN": "t", "PGBRANCH_BRANCH": "gone",
		}); err != nil {
			t.Fatalf("destroy failed on 404: %v\n%s", err, out)
		}
	})

	t.Run("500 fails", func(t *testing.T) {
		s := newStub(t)
		s.delete = func(w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) }
		if out, _, err := run(t, script, map[string]string{
			"PGBRANCH_SERVER": s.ts.URL, "PGBRANCH_TOKEN": "t", "PGBRANCH_BRANCH": "b",
		}); err == nil {
			t.Fatalf("destroy succeeded on a 500:\n%s", out)
		}
	})

	t.Run("missing branch name fails", func(t *testing.T) {
		s := newStub(t)
		if out, _, err := run(t, script, map[string]string{
			"PGBRANCH_SERVER": s.ts.URL, "PGBRANCH_TOKEN": "t",
		}); err == nil {
			t.Fatalf("destroy succeeded without a branch name:\n%s", out)
		}
	})
}
