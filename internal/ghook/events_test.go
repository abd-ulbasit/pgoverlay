package ghook

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/abd-ulbasit/pgoverlay/internal/api"
	"github.com/abd-ulbasit/pgoverlay/internal/engine"
)

// Branch names of the fixtures (acme/widgets, PR #7, head ref
// feat/Health_Endpoint); see pgoverlayconnect/testdata/branch_names.json.
const (
	pr7Branch  = "gh-d782c8-pr-7"
	refBranch  = "gh-d782c8-feat-health-endpoint"
	pr7ProxyDB = "appdb@" + pr7Branch
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
	branch := api.Branch{Name: pr7Branch, Source: "main", State: "ready",
		Database: "appdb", User: "app", ProxyDatabase: pr7ProxyDB}
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

// forRepo rewrites a fixture to come from another repository (the head
// branch included).
func forRepo(body []byte, repo string) []byte {
	return bytes.ReplaceAll(body, []byte(`"acme/widgets"`), []byte(`"`+repo+`"`))
}

func TestOpenedCreatesMissingBranch(t *testing.T) {
	pg := newFakePG(t, false)
	svc := newService(Config{Source: "staging", TTLSeconds: 259200}, pg.srv.URL, nil)

	deliver(t, svc, fixture(t, "pr_opened.json"))
	pg.assertCalls("GET /v1/branches/"+pr7Branch, "POST /v1/branches")
	want := api.CreateBranchRequest{Name: pr7Branch, Source: "staging", TTLSeconds: 259200}
	if pg.create != want {
		t.Fatalf("create request = %+v, want %+v", pg.create, want)
	}
}

// git-branch naming: the pgoverlay branch is keyed by the PR's head ref
// (sanitized), so preview platforms can derive it from the git ref alone —
// available from the very first build, before any PR association exists.
func TestGitBranchNamingUsesSanitizedHeadRef(t *testing.T) {
	pg := newFakePG(t, false)
	svc := newService(Config{Source: "main", BranchNaming: "git-branch"}, pg.srv.URL, nil)

	deliver(t, svc, fixture(t, "pr_opened.json")) // head.ref = feat/Health_Endpoint
	pg.assertCalls("GET /v1/branches/"+refBranch, "POST /v1/branches")
	if pg.create.Name != refBranch {
		t.Fatalf("created %q, want %s", pg.create.Name, refBranch)
	}
}

// The service names branches with the connect helpers' functions, checked
// here against the same golden table the Go and JavaScript helpers use: an
// app that derives a name finds the branch the service created.
func TestBranchNamesMatchConnectHelpersGolden(t *testing.T) {
	data, err := os.ReadFile("../../pgoverlayconnect/testdata/branch_names.json")
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		Names []struct {
			Repo string
			PR   int
			Ref  string
			Want string
		} `json:"names"`
	}
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatal(err)
	}
	if len(g.Names) == 0 {
		t.Fatal("golden table has no names")
	}
	for _, c := range g.Names {
		mode := "git-branch"
		if c.Ref == "" {
			mode = "pr-number"
		}
		p := &payload{Number: c.PR}
		p.Repository.FullName = c.Repo
		p.PullRequest.Head.Ref = c.Ref
		p.PullRequest.Head.Repo = &struct {
			FullName string `json:"full_name"`
		}{c.Repo}
		svc := &Service{cfg: Config{BranchNaming: mode}}
		if got := svc.branchName(p); got != c.Want {
			t.Errorf("%s mode, repo=%q pr=%d ref=%q: branch %q, want %q", mode, c.Repo, c.PR, c.Ref, got, c.Want)
		}
	}
}

// Branch names can never be shared across pull requests: every name is in
// the gh- namespace (a human's "pr-7" or "feat-login" is a different
// branch), carries the repository key (PR #7 of two allow-listed
// repositories gets two branches), and a fork's head ref, chosen by an
// outsider, never picks the name in git-branch mode.
func TestBranchNamesNeverCollideAcrossPullRequests(t *testing.T) {
	pr := func(repo, headRepo string, number int, ref string) *payload {
		p := &payload{Number: number}
		p.Repository.FullName = repo
		p.PullRequest.Head.Ref = ref
		if headRepo != "" {
			p.PullRequest.Head.Repo = &struct {
				FullName string `json:"full_name"`
			}{headRepo}
		}
		return p
	}
	for _, mode := range []string{"pr-number", "git-branch"} {
		svc := &Service{cfg: Config{BranchNaming: mode}}
		widgets := svc.branchName(pr("acme/widgets", "acme/widgets", 7, "dependabot/npm_and_yarn/lodash-4.17.21"))
		gadgets := svc.branchName(pr("acme/gadgets", "acme/gadgets", 7, "dependabot/npm_and_yarn/lodash-4.17.21"))
		if widgets == gadgets {
			t.Errorf("%s: PR #7 of two repositories share branch %q", mode, widgets)
		}
		if !strings.HasPrefix(widgets, "gh-") || widgets == "pr-7" || widgets == "feat-login" {
			t.Errorf("%s: %q is outside the gh- namespace", mode, widgets)
		}
		if a, b := svc.branchName(pr("acme/widgets", "acme/widgets", 1, "")), svc.branchName(pr("acme/widgets", "acme/widgets", 2, "")); a == b {
			t.Errorf("%s: distinct PRs collide: %q", mode, a)
		}
	}

	// git-branch mode: a fork PR whose head ref copies an in-repo branch
	// ("feat/login") must not land on that branch's database. It is named by
	// number, as is a PR whose fork was deleted (head.repo null).
	svc := &Service{cfg: Config{BranchNaming: "git-branch"}}
	inRepo := svc.branchName(pr("acme/widgets", "acme/widgets", 10, "feat/login"))
	if inRepo != "gh-d782c8-feat-login" {
		t.Fatalf("in-repo PR branch = %q, want gh-d782c8-feat-login", inRepo)
	}
	if got := svc.branchName(pr("acme/widgets", "mallory/widgets", 11, "feat/login")); got != "gh-d782c8-pr-11" {
		t.Errorf("fork PR branch = %q, want gh-d782c8-pr-11 (named by number)", got)
	}
	if got := svc.branchName(pr("acme/widgets", "", 12, "feat/login")); got != "gh-d782c8-pr-12" {
		t.Errorf("deleted-fork PR branch = %q, want gh-d782c8-pr-12", got)
	}
	// Head repository names compare case-insensitively.
	if got := svc.branchName(pr("acme/widgets", "Acme/Widgets", 10, "feat/login")); got != inRepo {
		t.Errorf("same repo in other case = %q, want %q", got, inRepo)
	}
}

// Two allow-listed repositories each open PR #7: each gets its own branch,
// and closing one destroys only its own.
func TestCrossRepositoryPullRequestsKeepSeparateBranches(t *testing.T) {
	pg := newFakePG(t, false)
	svc := newService(Config{Repos: []string{"acme/widgets", "acme/gadgets"}}, pg.srv.URL, nil)

	deliver(t, svc, fixture(t, "pr_opened.json"))
	widgets := pg.create.Name
	pg.exists = false // the fake holds one branch; let gadgets create its own
	deliver(t, svc, forRepo(fixture(t, "pr_opened.json"), "acme/gadgets"))
	gadgets := pg.create.Name
	if widgets != pr7Branch || gadgets != "gh-9c2435-pr-7" {
		t.Fatalf("created %q and %q, want %s and gh-9c2435-pr-7", widgets, gadgets, pr7Branch)
	}

	deliver(t, svc, forRepo(fixture(t, "pr_closed.json"), "acme/gadgets"))
	calls := pg.calls
	for _, c := range calls {
		if strings.HasPrefix(c, "DELETE") && c != "DELETE /v1/branches/gh-9c2435-pr-7" {
			t.Fatalf("closing acme/gadgets#7 issued %q; calls = %v", c, calls)
		}
	}
	if last := calls[len(calls)-1]; last != "DELETE /v1/branches/gh-9c2435-pr-7" {
		t.Fatalf("last call = %q, want the gadgets branch destroyed; calls = %v", last, calls)
	}
}

func TestReopenedExistingBranchIsNoop(t *testing.T) {
	pg := newFakePG(t, true)
	svc := newService(Config{}, pg.srv.URL, nil)

	body := bytes.Replace(fixture(t, "pr_opened.json"), []byte(`"opened"`), []byte(`"reopened"`), 1)
	deliver(t, svc, body)
	pg.assertCalls("GET /v1/branches/" + pr7Branch)
}

func TestSynchronizeDefaultEnsuresWithoutReset(t *testing.T) {
	pg := newFakePG(t, true)
	deliver(t, newService(Config{}, pg.srv.URL, nil), fixture(t, "pr_synchronize.json"))
	pg.assertCalls("GET /v1/branches/" + pr7Branch) // no reset, no create

	// missing branch is (re)created even on synchronize
	pg2 := newFakePG(t, false)
	deliver(t, newService(Config{}, pg2.srv.URL, nil), fixture(t, "pr_synchronize.json"))
	pg2.assertCalls("GET /v1/branches/"+pr7Branch, "POST /v1/branches")
}

func TestSynchronizeWithResetOnPushResetsExistingBranch(t *testing.T) {
	pg := newFakePG(t, true)
	deliver(t, newService(Config{ResetOnPush: true}, pg.srv.URL, nil), fixture(t, "pr_synchronize.json"))
	pg.assertCalls("GET /v1/branches/"+pr7Branch, "POST /v1/branches/"+pr7Branch+"/reset")

	// freshly created branch needs no reset
	pg2 := newFakePG(t, false)
	deliver(t, newService(Config{ResetOnPush: true}, pg2.srv.URL, nil), fixture(t, "pr_synchronize.json"))
	pg2.assertCalls("GET /v1/branches/"+pr7Branch, "POST /v1/branches")
}

func TestClosedDestroysBranchAndToleratesMissing(t *testing.T) {
	pg := newFakePG(t, true)
	svc := newService(Config{}, pg.srv.URL, nil)

	deliver(t, svc, fixture(t, "pr_closed.json"))
	pg.assertCalls("DELETE /v1/branches/" + pr7Branch)

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
	pg.assertCalls("GET /v1/branches/"+pr7Branch, "POST /v1/branches")
}

// Branch-operation failures are logged by the detached worker, never
// surfaced to GitHub: the delivery was already acked with 202 (re-delivery
// wouldn't help, and slow operations must not look like webhook outages).
func TestPGOverlayFailureStillAcked(t *testing.T) {
	pg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "kaboom"})
	}))
	defer pg.Close()
	deliver(t, newService(Config{}, pg.URL, nil), fixture(t, "pr_opened.json"))
}
