// Package actiontest exercises the composite GitHub Action's shell entrypoints
// (action/entrypoint.sh, action/destroy/entrypoint.sh) against an httptest
// stub branchd, the same way a workflow run would invoke them.
package actiontest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	gets      int
	requests  int
	getStates []string // consumed one per GET; last repeats
	getStatus int      // non-zero: GET answers this status instead
	password  string   // per-branch password (rotation mode) when set
	reason    string   // reason of the last /history transition
	create    func(w http.ResponseWriter, body map[string]any)
	delete    func(w http.ResponseWriter)
	ts        *httptest.Server
}

func newStub(t *testing.T) *stub {
	s := &stub{getStates: []string{"ready"}}
	// branch renders a branch; the caller holds s.mu
	branch := func(name, state string) map[string]any {
		b := map[string]any{
			"name": name, "state": state, "host": "10.1.2.3", "port": 31999,
			"user": "appuser", "database": "appdb", "proxy_database": "appdb@" + name,
		}
		if s.password != "" {
			b["password"] = s.password
		}
		return b
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/branches", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.auths = append(s.auths, r.Header.Get("Authorization"))
		s.requests++
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
		s.requests++
		s.gets++
		if s.getStatus != 0 {
			w.WriteHeader(s.getStatus)
			w.Write([]byte(`{"error":"poll boom"}`))
			return
		}
		if len(s.getStates) > 1 {
			s.getStates = s.getStates[1:]
		}
		json.NewEncoder(w).Encode(branch(r.PathValue("name"), s.getStates[0]))
	})
	mux.HandleFunc("GET /v1/branches/{name}/history", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.auths = append(s.auths, r.Header.Get("Authorization"))
		s.requests++
		json.NewEncoder(w).Encode([]map[string]string{
			{"from_state": "", "to_state": "creating", "reason": "create"},
			{"from_state": "creating", "to_state": s.getStates[0], "reason": s.reason},
		})
	})
	mux.HandleFunc("DELETE /v1/branches/{name}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.auths = append(s.auths, r.Header.Get("Authorization"))
		s.requests++
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

// The setters below mutate fields the server's handler reads under s.mu. The
// httptest server serves requests on its own goroutines while the test goroutine
// is still running (the action shells out to curl), so every write must take the
// same lock the handler uses for its reads.

func (s *stub) setStates(states ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getStates = states
}

func (s *stub) setCreate(fn func(w http.ResponseWriter, body map[string]any)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.create = fn
}

func (s *stub) setDelete(fn func(w http.ResponseWriter)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delete = fn
}

func (s *stub) setPassword(pw string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.password = pw
}

func (s *stub) setGetStatus(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getStatus = code
}

func (s *stub) setReason(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reason = reason
}

func (s *stub) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

// host is the stub's hostname: the create action's default router host.
func (s *stub) host(t *testing.T) string {
	t.Helper()
	u, err := url.Parse(s.ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Hostname()
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
		"PGOVERLAY_POLL_INTERVAL=0", // no real sleeps in tests
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
	s.setStates("creating", "creating", "ready") // create returns creating; polls reach ready
	script := scriptPath(t, "action/entrypoint.sh")

	out, outputs, err := run(t, script, map[string]string{
		"PGOVERLAY_SERVER": s.ts.URL + "/", // trailing slash tolerated
		"PGOVERLAY_TOKEN":  "act-tok",
		"PGOVERLAY_SOURCE": "staging",
		"PGOVERLAY_TTL":    "900",
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

	want := map[string]string{
		"branch": name, "host": "10.1.2.3", "port": "31999", "database": "appdb", "user": "appuser",
		"proxy_host": s.host(t), "proxy_port": "6432", "proxy_database": "appdb@" + name,
	}
	for k, v := range want {
		if outputs[k] != v {
			t.Errorf("output %s = %q, want %q", k, outputs[k], v)
		}
	}
	if _, ok := outputs["password"]; ok {
		t.Error("inherit mode (no server password): password must not be an output")
	}
	if strings.Contains(out, "act-tok") {
		t.Error("token leaked into the log")
	}
	if !strings.Contains(out, "router at "+s.host(t)+":6432") {
		t.Errorf("log does not lead with the router endpoint:\n%s", out)
	}
}

// TestCreateRotatedPassword: when branchd rotates per-branch credentials the
// workflow cannot know the password, so it becomes an output, masked in the
// log before anything else can print it.
func TestCreateRotatedPassword(t *testing.T) {
	requireTools(t)
	const pw = "0123456789abcdef0123456789abcdef"
	s := newStub(t)
	s.setPassword(pw)
	script := scriptPath(t, "action/entrypoint.sh")
	out, outputs, err := run(t, script, map[string]string{
		"PGOVERLAY_SERVER": s.ts.URL, "PGOVERLAY_TOKEN": "t",
	})
	if err != nil {
		t.Fatalf("entrypoint failed: %v\n%s", err, out)
	}
	if outputs["password"] != pw {
		t.Errorf("password output = %q, want %q", outputs["password"], pw)
	}
	if !strings.HasPrefix(out, "::add-mask::"+pw+"\n") {
		t.Errorf("the password must be masked before any other log line:\n%s", out)
	}
	if strings.Count(out, pw) != 1 {
		t.Errorf("the password appears in the log outside the mask command:\n%s", out)
	}
}

// TestCreateProxyHost: the proxy_host input overrides the router endpoint
// (the Helm chart's separate pgoverlay-proxy Service).
func TestCreateProxyHost(t *testing.T) {
	requireTools(t)
	script := scriptPath(t, "action/entrypoint.sh")
	for _, tt := range []struct{ in, host, port string }{
		{"pgoverlay-proxy.pgoverlay-system:7432", "pgoverlay-proxy.pgoverlay-system", "7432"},
		{"proxy.internal", "proxy.internal", "6432"},
		{"10.0.0.9:6433", "10.0.0.9", "6433"},
		{"[fd00::1]:6433", "fd00::1", "6433"},
		{"[fd00::1]", "fd00::1", "6432"},
		{"fd00::1", "fd00::1", "6432"},
	} {
		s := newStub(t)
		out, outputs, err := run(t, script, map[string]string{
			"PGOVERLAY_SERVER": s.ts.URL, "PGOVERLAY_TOKEN": "t", "PGOVERLAY_PROXY_HOST": tt.in,
		})
		if err != nil {
			t.Fatalf("proxy_host %q: entrypoint failed: %v\n%s", tt.in, err, out)
		}
		if outputs["proxy_host"] != tt.host || outputs["proxy_port"] != tt.port {
			t.Errorf("proxy_host %q: outputs %q:%q, want %q:%q", tt.in, outputs["proxy_host"], outputs["proxy_port"], tt.host, tt.port)
		}
	}

	// a malformed value fails before a branch is created
	for _, bad := range []string{"proxy:notaport", "proxy:", ":6432", "proxy:70000", "http://proxy", "a b"} {
		s := newStub(t)
		out, outputs, err := run(t, script, map[string]string{
			"PGOVERLAY_SERVER": s.ts.URL, "PGOVERLAY_TOKEN": "t", "PGOVERLAY_PROXY_HOST": bad,
		})
		if err == nil {
			t.Errorf("proxy_host %q accepted:\n%s", bad, out)
		}
		if n := s.requestCount(); n != 0 || len(outputs) != 0 {
			t.Errorf("proxy_host %q: %d requests, outputs %v; want none", bad, n, outputs)
		}
	}
}

// TestCreateDefaultRouterHost: the default router host is the server URL's
// host with any credentials, port, path and query stripped.
func TestCreateDefaultRouterHost(t *testing.T) {
	requireTools(t)
	s := newStub(t)
	u, err := url.Parse(s.ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	script := scriptPath(t, "action/entrypoint.sh")
	for _, server := range []string{
		"http://" + u.Host,
		"http://user:pw@" + u.Host,
		"http://" + u.Host + "/",
	} {
		out, outputs, err := run(t, script, map[string]string{"PGOVERLAY_SERVER": server, "PGOVERLAY_TOKEN": "t"})
		if err != nil {
			t.Fatalf("server %q: entrypoint failed: %v\n%s", server, err, out)
		}
		if outputs["proxy_host"] != u.Hostname() || outputs["proxy_port"] != "6432" {
			t.Errorf("server %q: router %q:%q, want %q:6432", server, outputs["proxy_host"], outputs["proxy_port"], u.Hostname())
		}
	}
}

func TestCreateExplicitName(t *testing.T) {
	requireTools(t)
	s := newStub(t)
	script := scriptPath(t, "action/entrypoint.sh")
	out, outputs, err := run(t, script, map[string]string{
		"PGOVERLAY_SERVER": s.ts.URL,
		"PGOVERLAY_TOKEN":  "act-tok",
		"PGOVERLAY_BRANCH": "pr-77-ci",
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

// TestCreateInvalidName: a name the server would reject (e.g. a raw git ref)
// fails offline, before any request.
func TestCreateInvalidName(t *testing.T) {
	requireTools(t)
	s := newStub(t)
	script := scriptPath(t, "action/entrypoint.sh")
	out, outputs, err := run(t, script, map[string]string{
		"PGOVERLAY_SERVER": s.ts.URL, "PGOVERLAY_TOKEN": "t", "PGOVERLAY_BRANCH": "feat/login",
	})
	if err == nil {
		t.Fatalf("entrypoint accepted the branch name feat/login:\n%s", out)
	}
	if n := s.requestCount(); n != 0 || len(outputs) != 0 {
		t.Errorf("%d requests, outputs %v; want none for an invalid name", n, outputs)
	}
}

func TestCreateServerError(t *testing.T) {
	requireTools(t)
	s := newStub(t)
	s.setCreate(func(w http.ResponseWriter, _ map[string]any) {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":"branch exists"}`))
	})
	script := scriptPath(t, "action/entrypoint.sh")
	out, outputs, err := run(t, script, map[string]string{
		"PGOVERLAY_SERVER": s.ts.URL, "PGOVERLAY_TOKEN": "t", "PGOVERLAY_BRANCH": "someone-elses",
	})
	if err == nil {
		t.Fatalf("entrypoint succeeded on a 409:\n%s", out)
	}
	if !strings.Contains(out, "branch exists") {
		t.Errorf("error body not surfaced:\n%s", out)
	}
	// nothing was created: the paired destroy step must get nothing to delete
	// (a 409 names a branch this run does not own)
	if b, ok := outputs["branch"]; ok {
		t.Errorf("branch output %q set although the create failed", b)
	}
}

// TestCreateNeverReady: the branch exists but the wait times out. The step
// fails, but `branch` is already an output, so the paired destroy step (if:
// always()) removes the branch.
func TestCreateNeverReady(t *testing.T) {
	requireTools(t)
	s := newStub(t)
	s.setStates("creating")
	script := scriptPath(t, "action/entrypoint.sh")
	out, outputs, err := run(t, script, map[string]string{
		"PGOVERLAY_SERVER": s.ts.URL, "PGOVERLAY_TOKEN": "t",
		"PGOVERLAY_POLL_MAX": "3",
	})
	if err == nil {
		t.Fatalf("entrypoint succeeded although the branch never became ready:\n%s", out)
	}
	if !strings.Contains(out, "not ready") {
		t.Errorf("missing not-ready diagnostic:\n%s", out)
	}
	s.mu.Lock()
	created, _ := s.creates[0]["name"].(string)
	s.mu.Unlock()
	if created == "" || outputs["branch"] != created {
		t.Errorf("branch output = %q, want the created branch %q for the destroy step", outputs["branch"], created)
	}
	if _, ok := outputs["proxy_database"]; ok {
		t.Error("connection outputs set for a branch that never became ready")
	}

	destroy := scriptPath(t, "action/destroy/entrypoint.sh")
	if out, _, err := run(t, destroy, map[string]string{
		"PGOVERLAY_SERVER": s.ts.URL, "PGOVERLAY_TOKEN": "t", "PGOVERLAY_BRANCH": outputs["branch"],
	}); err != nil {
		t.Fatalf("paired destroy failed: %v\n%s", err, out)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.deletes) != 1 || s.deletes[0] != created {
		t.Errorf("deletes = %v, want [%s]", s.deletes, created)
	}
}

func TestCreatePollError(t *testing.T) {
	requireTools(t)
	s := newStub(t)
	s.setStates("creating")
	s.setGetStatus(http.StatusBadGateway)
	script := scriptPath(t, "action/entrypoint.sh")
	out, outputs, err := run(t, script, map[string]string{
		"PGOVERLAY_SERVER": s.ts.URL, "PGOVERLAY_TOKEN": "t", "PGOVERLAY_BRANCH": "pr-9-ci",
	})
	if err == nil {
		t.Fatalf("entrypoint succeeded although polling failed:\n%s", out)
	}
	if !strings.Contains(out, "poll boom") {
		t.Errorf("poll error body not surfaced:\n%s", out)
	}
	if outputs["branch"] != "pr-9-ci" {
		t.Errorf("branch output = %q, want pr-9-ci for the destroy step", outputs["branch"])
	}
}

// TestCreateTerminalState: a branch that turns failed/destroying/destroyed
// can never become ready; the action fails at once with the server's reason
// instead of polling for the full 5 minutes.
func TestCreateTerminalState(t *testing.T) {
	requireTools(t)
	script := scriptPath(t, "action/entrypoint.sh")
	for _, state := range []string{"failed", "destroying", "destroyed"} {
		s := newStub(t)
		s.setStates("creating", state)
		s.setReason("clone failed: no space left on device")
		out, outputs, err := run(t, script, map[string]string{
			"PGOVERLAY_SERVER": s.ts.URL, "PGOVERLAY_TOKEN": "t", "PGOVERLAY_BRANCH": "pr-5-ci",
			"PGOVERLAY_POLL_MAX": "50",
		})
		if err == nil {
			t.Fatalf("%s: entrypoint succeeded:\n%s", state, out)
		}
		if !strings.Contains(out, "is "+state) || !strings.Contains(out, "no space left on device") {
			t.Errorf("%s: missing state or reason in the diagnostic:\n%s", state, out)
		}
		s.mu.Lock()
		gets := s.gets
		s.mu.Unlock()
		// create answers "creating"; the first GET already reports state
		if gets != 1 {
			t.Errorf("%s: GET polls = %d, want 1 (stop at the first terminal state)", state, gets)
		}
		if outputs["branch"] != "pr-5-ci" {
			t.Errorf("%s: branch output = %q, want pr-5-ci", state, outputs["branch"])
		}
	}
}

func TestCreateMissingEnv(t *testing.T) {
	requireTools(t)
	script := scriptPath(t, "action/entrypoint.sh")
	if out, _, err := run(t, script, map[string]string{"PGOVERLAY_TOKEN": "t"}); err == nil {
		t.Fatalf("entrypoint succeeded without PGOVERLAY_SERVER:\n%s", out)
	}
}

func TestDestroy(t *testing.T) {
	requireTools(t)
	script := scriptPath(t, "action/destroy/entrypoint.sh")

	t.Run("deletes the branch", func(t *testing.T) {
		s := newStub(t)
		out, _, err := run(t, script, map[string]string{
			"PGOVERLAY_SERVER": s.ts.URL, "PGOVERLAY_TOKEN": "act-tok", "PGOVERLAY_BRANCH": "pr-77-ci",
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
		s.setDelete(func(w http.ResponseWriter) { w.WriteHeader(http.StatusNotFound) })
		if out, _, err := run(t, script, map[string]string{
			"PGOVERLAY_SERVER": s.ts.URL, "PGOVERLAY_TOKEN": "t", "PGOVERLAY_BRANCH": "gone",
		}); err != nil {
			t.Fatalf("destroy failed on 404: %v\n%s", err, out)
		}
	})

	t.Run("500 fails", func(t *testing.T) {
		s := newStub(t)
		s.setDelete(func(w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) })
		if out, _, err := run(t, script, map[string]string{
			"PGOVERLAY_SERVER": s.ts.URL, "PGOVERLAY_TOKEN": "t", "PGOVERLAY_BRANCH": "b",
		}); err == nil {
			t.Fatalf("destroy succeeded on a 500:\n%s", out)
		}
	})

	t.Run("missing branch name fails", func(t *testing.T) {
		s := newStub(t)
		if out, _, err := run(t, script, map[string]string{
			"PGOVERLAY_SERVER": s.ts.URL, "PGOVERLAY_TOKEN": "t",
		}); err == nil {
			t.Fatalf("destroy succeeded without a branch name:\n%s", out)
		}
	})
}
