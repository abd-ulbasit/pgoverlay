package ghook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

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
// call as "METHOD path" and serves a single configurable branch whose state
// moves the way branchd's does: a create starts in "creating", a failed
// create leaves "failed", and a reset of a branch that is not ready is an
// illegal transition (409).
type fakePG struct {
	t            *testing.T
	mu           sync.Mutex
	calls        []string
	exists       bool                    // GET /v1/branches/{name} → 200 vs 404
	state        string                  // state of an existing branch ("" = ready)
	getStates    []string                // served by the next GETs, one each, before state
	createErr    int                     // non-zero: POST /v1/branches fails with this status
	createErrMsg string                  // its error message ("" = "internal server error")
	create       api.CreateBranchRequest // last create body
	// createStarted/createRelease, when set, make the create handler signal
	// that it is running and wait to be released (a create in flight).
	createStarted chan struct{}
	createRelease chan struct{}
	diff          *engine.DiffResult // served by GET .../diff when set
	diffData      string             // last ?data query seen on the diff route
	srv           *httptest.Server
}

func newFakePG(t *testing.T, exists bool) *fakePG {
	f := &fakePG{t: t, exists: exists}
	mux := http.NewServeMux()
	branch := func(name, state string) api.Branch {
		if state == "" {
			state = "ready"
		}
		return api.Branch{Name: name, Source: "main", State: state,
			Database: "appdb", User: "app", ProxyDatabase: "appdb@" + name}
	}
	record := func(r *http.Request) { f.calls = append(f.calls, r.Method+" "+r.URL.Path) }
	notFound := func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
	}
	mux.HandleFunc("GET /v1/branches/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		record(r)
		if !f.exists {
			notFound(w)
			return
		}
		state := f.state
		if len(f.getStates) > 0 {
			state, f.getStates = f.getStates[0], f.getStates[1:]
		}
		json.NewEncoder(w).Encode(branch(r.PathValue("name"), state))
	})
	mux.HandleFunc("POST /v1/branches", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		record(r)
		json.NewDecoder(r.Body).Decode(&f.create)
		f.exists, f.state = true, "creating"
		started, release := f.createStarted, f.createRelease
		f.createStarted, f.createRelease = nil, nil // block one create only
		f.mu.Unlock()
		if started != nil {
			close(started)
			<-release
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.createErr != 0 {
			f.state = "failed"
			msg := f.createErrMsg
			if msg == "" {
				msg = "internal server error"
			}
			w.WriteHeader(f.createErr)
			json.NewEncoder(w).Encode(map[string]string{"error": msg})
			return
		}
		f.state = "ready"
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(branch(f.create.Name, "ready"))
	})
	mux.HandleFunc("POST /v1/branches/{name}/reset", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		record(r)
		if !f.exists {
			notFound(w)
			return
		}
		if f.state != "" && f.state != "ready" {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]string{"error": "illegal branch transition " + f.state + " -> resetting"})
			return
		}
		json.NewEncoder(w).Encode(branch(r.PathValue("name"), "ready"))
	})
	mux.HandleFunc("DELETE /v1/branches/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		record(r)
		if !f.exists {
			notFound(w)
			return
		}
		f.exists, f.state = false, ""
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v1/branches/{name}/diff", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
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

// blockCreate makes the next create signal on the returned channel once it
// is running and then wait until release is called (a create in flight).
// The test's cleanup releases it too, so a failing test cannot hang the
// server's shutdown.
func (f *fakePG) blockCreate(t *testing.T) (started <-chan struct{}, release func()) {
	s, r := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(r) }) }
	t.Cleanup(release) // runs before the server's Close (cleanups are LIFO)
	f.set(func(f *fakePG) { f.createStarted, f.createRelease = s, r })
	return s, release
}

// set changes the fake's state between deliveries.
func (f *fakePG) set(change func(*fakePG)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

func (f *fakePG) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakePG) assertCalls(want ...string) {
	f.t.Helper()
	calls := f.callLog()
	if len(calls) != len(want) {
		f.t.Fatalf("calls = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			f.t.Fatalf("calls = %v, want %v", calls, want)
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
	pg.set(func(f *fakePG) { f.exists = false }) // the fake holds one branch; let gadgets create its own
	deliver(t, svc, forRepo(fixture(t, "pr_opened.json"), "acme/gadgets"))
	gadgets := pg.create.Name
	if widgets != pr7Branch || gadgets != "gh-9c2435-pr-7" {
		t.Fatalf("created %q and %q, want %s and gh-9c2435-pr-7", widgets, gadgets, pr7Branch)
	}

	deliver(t, svc, forRepo(fixture(t, "pr_closed.json"), "acme/gadgets"))
	calls := pg.callLog()
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
	pg.assertCalls("GET /v1/branches/"+pr7Branch, "DELETE /v1/branches/"+pr7Branch)

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

// Deliveries for one pull request run one at a time in arrival order. A
// push that arrives while the branch is still being created waits for the
// create, then resets a ready branch; without the queue its reset would hit
// a branch in "creating" and fail (409), leaving a red status on the newest
// commit.
func TestDeliveriesForOneBranchRunInArrivalOrder(t *testing.T) {
	pg := newFakePG(t, false)
	svc := newService(Config{ResetOnPush: true}, pg.srv.URL, nil)
	svc.pollEvery = time.Millisecond
	t.Cleanup(svc.Wait)
	started, release := pg.blockCreate(t) // cleanups: release, then Wait, then the server's Close
	h := svc.Handler()

	if rr := signedPost(t, h, fixture(t, "pr_opened.json")); rr.Code != http.StatusAccepted {
		t.Fatalf("opened: code=%d", rr.Code)
	}
	<-started // the create is in flight
	if rr := signedPost(t, h, fixture(t, "pr_synchronize.json")); rr.Code != http.StatusAccepted {
		t.Fatalf("synchronize: code=%d", rr.Code)
	}
	svc.mu.Lock()
	queued := len(svc.queues[pr7Branch])
	svc.mu.Unlock()
	if queued != 1 {
		t.Fatalf("queued jobs for %s = %d, want the push waiting behind the create", pr7Branch, queued)
	}
	pg.assertCalls("GET /v1/branches/"+pr7Branch, "POST /v1/branches") // the push has not started

	release()
	svc.Wait()
	pg.assertCalls("GET /v1/branches/"+pr7Branch, "POST /v1/branches",
		"GET /v1/branches/"+pr7Branch, "POST /v1/branches/"+pr7Branch+"/reset")
	svc.mu.Lock()
	defer svc.mu.Unlock()
	if len(svc.queues) != 0 {
		t.Errorf("queues not retired: %v", svc.queues)
	}
}

// Different branches do not wait for each other.
func TestDeliveriesForDifferentBranchesRunConcurrently(t *testing.T) {
	pg := newFakePG(t, false)
	svc := newService(Config{}, pg.srv.URL, nil)
	svc.pollEvery = time.Millisecond
	t.Cleanup(svc.Wait)
	started, release := pg.blockCreate(t)
	h := svc.Handler()

	signedPost(t, h, fixture(t, "pr_opened.json"))
	<-started
	// PR #8 of the same repository closes while #7's create is blocked.
	closed8 := bytes.Replace(fixture(t, "pr_closed.json"), []byte(`"number": 7`), []byte(`"number": 8`), 1)
	signedPost(t, h, closed8)
	deadline := time.Now().Add(5 * time.Second)
	for !contains(pg.callLog(), "GET /v1/branches/gh-d782c8-pr-8") {
		if time.Now().After(deadline) {
			t.Fatalf("PR #8 waited behind PR #7: calls = %v", pg.callLog())
		}
		time.Sleep(5 * time.Millisecond)
	}
	release()
	svc.Wait()
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// A branch another operation is still creating is waited for; the status
// turns success only once it is ready.
func TestEnsureWaitsForABranchStillCreating(t *testing.T) {
	pg := newFakePG(t, true)
	pg.getStates = []string{"creating", "creating"} // then ready
	gh := newFakeGitHub(t)
	svc := newService(Config{}, pg.srv.URL, gh.client())
	svc.pollEvery = time.Millisecond
	deliver(t, svc, fixture(t, "pr_synchronize.json"))

	pg.assertCalls("GET /v1/branches/"+pr7Branch, "GET /v1/branches/"+pr7Branch, "GET /v1/branches/"+pr7Branch)
	if n := len(gh.statuses); n != 2 || gh.statuses[1].State != "success" {
		t.Fatalf("statuses = %+v, want pending then success", gh.statuses)
	}
}

// A branch that never settles fails the delivery with a timeout, not a
// success.
func TestEnsureTimesOutOnABranchThatNeverSettles(t *testing.T) {
	pg := newFakePG(t, true)
	pg.state = "resetting"
	svc := newService(Config{}, pg.srv.URL, nil)
	svc.pollEvery = time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	p := &payload{Action: "synchronize", Number: 7}
	_, _, err := svc.ensureBranch(ctx, testLogger(), p, pr7Branch)
	if err == nil || !strings.Contains(err.Error(), "timed out waiting for branch "+pr7Branch+" (still resetting)") {
		t.Fatalf("err = %v, want a timeout naming the state", err)
	}
}

// A failed branch (its create or reset did not finish) is destroyed and
// created again on the next event, never reported ready and never reset
// (failed -> resetting is illegal).
func TestFailedBranchIsReplacedNotReportedReady(t *testing.T) {
	for _, reset := range []bool{false, true} {
		t.Run(fmt.Sprintf("resetOnPush=%v", reset), func(t *testing.T) {
			pg := newFakePG(t, true)
			pg.state = "failed"
			gh := newFakeGitHub(t)
			deliver(t, newService(Config{ResetOnPush: reset, ProxyHost: "pg.example.com"}, pg.srv.URL, gh.client()),
				fixture(t, "pr_synchronize.json"))

			pg.assertCalls("GET /v1/branches/"+pr7Branch, "DELETE /v1/branches/"+pr7Branch, "POST /v1/branches")
			if n := len(gh.statuses); n != 2 || gh.statuses[1].State != "success" {
				t.Fatalf("statuses = %+v, want pending then success for the new branch", gh.statuses)
			}
			final := gh.patched[len(gh.patched)-1]
			if !strings.Contains(final, "| State | ready |") {
				t.Errorf("comment = %s, want ready", final)
			}
		})
	}
}

// A create that fails is reported as a failure — status and comment — and
// the next push replaces the failed branch.
func TestFailedCreateIsReportedAndRepairedByTheNextPush(t *testing.T) {
	pg := newFakePG(t, false)
	pg.createErr = http.StatusInternalServerError
	gh := newFakeGitHub(t)
	svc := newService(Config{ProxyHost: "pg.example.com"}, pg.srv.URL, gh.client())

	deliver(t, svc, fixture(t, "pr_opened.json"))
	if n := len(gh.statuses); n != 2 || gh.statuses[1].State != "failure" {
		t.Fatalf("statuses = %+v, want pending then failure", gh.statuses)
	}
	final := gh.comments[len(gh.comments)-1].Body
	if !strings.Contains(final, "failed") || strings.Contains(final, "psql") {
		t.Errorf("comment after a failed create = %s, want failed and no connect string", final)
	}

	pg.set(func(f *fakePG) { f.createErr = 0 })
	deliver(t, svc, fixture(t, "pr_synchronize.json"))
	pg.assertCalls("GET /v1/branches/"+pr7Branch, "POST /v1/branches", // the failed create
		"GET /v1/branches/"+pr7Branch, "DELETE /v1/branches/"+pr7Branch, "POST /v1/branches")
	if last := gh.statuses[len(gh.statuses)-1]; last.State != "success" || last.SHA != "9f8e7d6c5b4a3210" {
		t.Errorf("last status = %+v, want success on the pushed SHA", last)
	}
}

// Each X-GitHub-Delivery id runs once; a repeat is acknowledged but does
// nothing.
func TestDuplicateDeliveryRunsOnce(t *testing.T) {
	pg := newFakePG(t, true)
	svc := newService(Config{}, pg.srv.URL, nil)
	body := fixture(t, "pr_closed.json")
	send := func(id string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/webhook", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Event", "pull_request")
		req.Header.Set("X-Hub-Signature-256", sign(testSecret, body))
		req.Header.Set("X-GitHub-Delivery", id)
		rr := httptest.NewRecorder()
		svc.Handler().ServeHTTP(rr, req)
		svc.Wait()
		return rr
	}
	if rr := send("72d3162e-cc78-11e3-81ab-4c9367dc0958"); rr.Code != http.StatusAccepted {
		t.Fatalf("first delivery: code=%d", rr.Code)
	}
	rr := send("72d3162e-cc78-11e3-81ab-4c9367dc0958")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "duplicate") {
		t.Fatalf("repeat delivery: code=%d body=%s, want 200 duplicate", rr.Code, rr.Body)
	}
	pg.assertCalls("GET /v1/branches/"+pr7Branch, "DELETE /v1/branches/"+pr7Branch)

	// A different delivery of the same event runs.
	pg.set(func(f *fakePG) { f.exists = true })
	if rr := send("8c1a2b3c-0000-11e3-81ab-4c9367dc0958"); rr.Code != http.StatusAccepted {
		t.Fatalf("new delivery: code=%d", rr.Code)
	}
}

func TestDeliveryCacheIsBounded(t *testing.T) {
	c := newDeliveryCache(2)
	for _, id := range []string{"a", "b"} {
		if !c.add(id) {
			t.Fatalf("add(%q) = false on first sight", id)
		}
	}
	if c.add("a") {
		t.Fatal("add(a) again = true, want duplicate")
	}
	c.add("c") // evicts a, the oldest
	if !c.add("a") {
		t.Fatal("a still remembered after eviction")
	}
	if len(c.ids) != 2 {
		t.Fatalf("cache holds %d ids, want 2", len(c.ids))
	}
}
