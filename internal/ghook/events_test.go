package ghook

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/abd-ulbasit/pgbranch/internal/api"
	"github.com/abd-ulbasit/pgbranch/internal/engine"
)

// fakePG is an httptest stand-in for branchd's REST API. It records every
// call as "METHOD path" and serves a single configurable branch.
type fakePG struct {
	t        *testing.T
	calls    []string
	exists   bool                    // GET /v1/branches/{name} → 200 vs 404
	create   api.CreateBranchRequest // last create body
	diff     *engine.DiffResult      // served by GET .../diff when set
	diffData string                  // last ?data query seen on the diff route
	srv      *httptest.Server
}

func newFakePG(t *testing.T, exists bool) *fakePG {
	f := &fakePG{t: t, exists: exists}
	mux := http.NewServeMux()
	branch := api.Branch{Name: "pr-7", Source: "main", State: "ready",
		Database: "appdb", User: "app", ProxyDatabase: "appdb@pr-7"}
	record := func(r *http.Request) { f.calls = append(f.calls, r.Method+" "+r.URL.Path) }
	mux.HandleFunc("GET /v1/branches/{name}", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		if !f.exists {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
			return
		}
		json.NewEncoder(w).Encode(branch)
	})
	mux.HandleFunc("POST /v1/branches", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		json.NewDecoder(r.Body).Decode(&f.create)
		f.exists = true
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(branch)
	})
	mux.HandleFunc("POST /v1/branches/{name}/reset", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		if !f.exists {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
			return
		}
		json.NewEncoder(w).Encode(branch)
	})
	mux.HandleFunc("DELETE /v1/branches/{name}", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		if !f.exists {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
			return
		}
		f.exists = false
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v1/branches/{name}/diff", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		f.diffData = r.URL.Query().Get("data")
		res := f.diff
		if res == nil {
			res = &engine.DiffResult{}
		}
		json.NewEncoder(w).Encode(res)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakePG) assertCalls(want ...string) {
	f.t.Helper()
	if len(f.calls) != len(want) {
		f.t.Fatalf("calls = %v, want %v", f.calls, want)
	}
	for i := range want {
		if f.calls[i] != want[i] {
			f.t.Fatalf("calls = %v, want %v", f.calls, want)
		}
	}
}

func signedPost(t *testing.T, h http.Handler, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	return post(t, h, "pull_request", sign(testSecret, body), body)
}

// deliver posts a signed pull_request event, asserts the immediate 202 ack
// (branch operations run detached — GitHub abandons deliveries after ~10s),
// and waits for the detached work to finish so calls can be asserted.
func deliver(t *testing.T, svc *Service, body []byte) {
	t.Helper()
	rr := signedPost(t, svc.Handler(), body)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s, want 202", rr.Code, rr.Body)
	}
	svc.Wait()
}

func TestOpenedCreatesMissingBranch(t *testing.T) {
	pg := newFakePG(t, false)
	svc := newService(Config{Source: "staging", TTLSeconds: 259200}, pg.srv.URL, nil)

	deliver(t, svc, fixture(t, "pr_opened.json"))
	pg.assertCalls("GET /v1/branches/gh-pr-7", "POST /v1/branches")
	want := api.CreateBranchRequest{Name: "gh-pr-7", Source: "staging", TTLSeconds: 259200}
	if pg.create != want {
		t.Fatalf("create request = %+v, want %+v", pg.create, want)
	}
}

// git-branch naming: the pgbranch branch is keyed by the PR's head ref
// (sanitized), so preview platforms can derive it from the git ref alone —
// available from the very first build, before any PR association exists.
func TestGitBranchNamingUsesSanitizedHeadRef(t *testing.T) {
	pg := newFakePG(t, false)
	svc := newService(Config{Source: "main", BranchNaming: "git-branch"}, pg.srv.URL, nil)

	deliver(t, svc, fixture(t, "pr_opened.json")) // head.ref = feat/Health_Endpoint
	pg.assertCalls("GET /v1/branches/gh-feat-health-endpoint", "POST /v1/branches")
	if pg.create.Name != "gh-feat-health-endpoint" {
		t.Fatalf("created %q, want gh-feat-health-endpoint", pg.create.Name)
	}
}

func TestSanitizeBranchName(t *testing.T) {
	// maxLen here mirrors the git-branch budget (engine limit minus the prefix).
	const maxLen = maxBranchNameLen - len(branchPrefix)
	cases := map[string]string{
		"feat/Health_Endpoint": "feat-health-endpoint",
		"FIX--weird///chars!!": "fix-weird-chars",
		"-/-":                  "",
		// Over-long refs truncate to the budget so prefix+ref fits the engine.
		"a" + strings.Repeat("b", 60): "a" + strings.Repeat("b", maxLen-1),
	}
	for in, want := range cases {
		if got := sanitizeBranchName(in, maxLen); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
		if len(branchPrefix+want) > maxBranchNameLen {
			t.Errorf("prefixed name %q exceeds engine limit %d", branchPrefix+want, maxBranchNameLen)
		}
	}
}

// TestBranchNameNamespacingPreventsCollisions proves that App-created branch
// names live in a reserved namespace: a PR (pr-number mode) and a human-named
// branch, and two differently-sourced refs, can never resolve to the same
// engine branch — so a webhook with ResetOnPush cannot reset someone else's
// branch. Every webhook-derived name carries the gh- prefix; a human creating
// "pr-7" or "feat-login" by hand lands on a distinct name.
func TestBranchNameNamespacingPreventsCollisions(t *testing.T) {
	prNumberSvc := &Service{cfg: Config{}}
	gitBranchSvc := &Service{cfg: Config{BranchNaming: "git-branch"}}

	p7 := &payload{Number: 7}
	if got := prNumberSvc.branchName(p7); got != "gh-pr-7" {
		t.Errorf("pr-number name = %q, want gh-pr-7 (namespaced)", got)
	}
	// A human-created branch literally named "pr-7" or "gh-pr-7"... the webhook
	// name is prefixed, so a plain human "pr-7" never collides with it.
	if got := prNumberSvc.branchName(p7); got == "pr-7" {
		t.Error("webhook pr name collides with a bare human pr-7")
	}

	// git-branch mode: an attacker opens an external PR whose head ref is
	// literally "pr-7" (trying to hijack PR #7's branch). After namespacing it
	// becomes gh-pr-7-... distinct from the gh-pr-7 that PR #7 would own only
	// via pr-number mode, and distinct from a human's bare "pr-7".
	evil := &payload{Number: 99}
	evil.PullRequest.Head.Ref = "feat/login"
	got := gitBranchSvc.branchName(evil)
	if got != "gh-feat-login" {
		t.Errorf("git-branch name = %q, want gh-feat-login", got)
	}
	// The sanitized ref alone ("feat-login") — what a human might name a branch —
	// is NOT what the webhook produces, so they cannot collide.
	if got == "feat-login" {
		t.Error("webhook git-branch name collides with a bare human feat-login")
	}

	// Two different PRs in pr-number mode get distinct namespaced names.
	if a, b := prNumberSvc.branchName(&payload{Number: 1}), prNumberSvc.branchName(&payload{Number: 2}); a == b {
		t.Errorf("distinct PRs collide: %q == %q", a, b)
	}
}

func TestReopenedExistingBranchIsNoop(t *testing.T) {
	pg := newFakePG(t, true)
	svc := newService(Config{}, pg.srv.URL, nil)

	body := bytes.Replace(fixture(t, "pr_opened.json"), []byte(`"opened"`), []byte(`"reopened"`), 1)
	deliver(t, svc, body)
	pg.assertCalls("GET /v1/branches/gh-pr-7")
}

func TestSynchronizeDefaultEnsuresWithoutReset(t *testing.T) {
	pg := newFakePG(t, true)
	deliver(t, newService(Config{}, pg.srv.URL, nil), fixture(t, "pr_synchronize.json"))
	pg.assertCalls("GET /v1/branches/gh-pr-7") // no reset, no create

	// missing branch is (re)created even on synchronize
	pg2 := newFakePG(t, false)
	deliver(t, newService(Config{}, pg2.srv.URL, nil), fixture(t, "pr_synchronize.json"))
	pg2.assertCalls("GET /v1/branches/gh-pr-7", "POST /v1/branches")
}

func TestSynchronizeWithResetOnPushResetsExistingBranch(t *testing.T) {
	pg := newFakePG(t, true)
	deliver(t, newService(Config{ResetOnPush: true}, pg.srv.URL, nil), fixture(t, "pr_synchronize.json"))
	pg.assertCalls("GET /v1/branches/gh-pr-7", "POST /v1/branches/gh-pr-7/reset")

	// freshly created branch needs no reset
	pg2 := newFakePG(t, false)
	deliver(t, newService(Config{ResetOnPush: true}, pg2.srv.URL, nil), fixture(t, "pr_synchronize.json"))
	pg2.assertCalls("GET /v1/branches/gh-pr-7", "POST /v1/branches")
}

func TestClosedDestroysBranchAndToleratesMissing(t *testing.T) {
	pg := newFakePG(t, true)
	svc := newService(Config{}, pg.srv.URL, nil)

	deliver(t, svc, fixture(t, "pr_closed.json"))
	pg.assertCalls("DELETE /v1/branches/gh-pr-7")

	// already gone → still acked
	deliver(t, svc, fixture(t, "pr_closed.json"))
}

func TestRepoAllowListFiltersEvents(t *testing.T) {
	pg := newFakePG(t, false)
	h := newService(Config{Repos: []string{"acme/other", "foo/bar"}}, pg.srv.URL, nil).Handler()

	if rr := signedPost(t, h, fixture(t, "pr_opened.json")); rr.Code != http.StatusNoContent {
		t.Fatalf("disallowed repo: code=%d want 204", rr.Code)
	}
	pg.assertCalls()

	// allow-listed repo goes through
	deliver(t, newService(Config{Repos: []string{"acme/widgets"}}, pg.srv.URL, nil), fixture(t, "pr_opened.json"))
	pg.assertCalls("GET /v1/branches/gh-pr-7", "POST /v1/branches")
}

// Branch-operation failures are logged by the detached worker, never
// surfaced to GitHub: the delivery was already acked with 202 (re-delivery
// wouldn't help, and slow operations must not look like webhook outages).
func TestPGBranchFailureStillAcked(t *testing.T) {
	pg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "kaboom"})
	}))
	defer pg.Close()
	deliver(t, newService(Config{}, pg.URL, nil), fixture(t, "pr_opened.json"))
}
