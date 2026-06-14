package ghook

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/abd-ulbasit/pgbranch/internal/api"
	"github.com/abd-ulbasit/pgbranch/internal/engine"
)

// recordedStatus is one POST /statuses/{sha} call seen by the fake.
type recordedStatus struct {
	SHA, State, Description, Context string
}

// ghComment is one issue comment held by the fake.
type ghComment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
}

// fakeGitHub serves the issue-comments and commit-status endpoints for
// acme/widgets#7. It is stateful: POSTed comments show up in later lists,
// PATCH rewrites them in place — so a live upsert flow exercises POST once
// then PATCH.
type fakeGitHub struct {
	t        *testing.T
	comments []ghComment // current comments (list endpoint state)
	posted   []string    // bodies received by the create endpoint
	patched  []string    // bodies received by the edit endpoint, in order
	statuses []recordedStatus
	lastReq  *http.Request
	lastList *http.Request // last GET of the comment list
	srv      *httptest.Server
}

func newFakeGitHub(t *testing.T, existing ...string) *fakeGitHub {
	f := &fakeGitHub{t: t}
	for i, b := range existing {
		f.comments = append(f.comments, ghComment{ID: int64(100 + i), Body: b})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/acme/widgets/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		f.lastReq, f.lastList = r, r
		out := f.comments
		if out == nil {
			out = []ghComment{}
		}
		json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("POST /repos/acme/widgets/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		f.lastReq = r
		var c ghComment
		if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
			t.Errorf("bad comment body: %v", err)
		}
		c.ID = int64(1000 + len(f.comments))
		f.posted = append(f.posted, c.Body)
		f.comments = append(f.comments, c)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(c)
	})
	mux.HandleFunc("PATCH /repos/acme/widgets/issues/comments/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.lastReq = r
		var c ghComment
		if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
			t.Errorf("bad comment body: %v", err)
		}
		for i := range f.comments {
			if fmt.Sprint(f.comments[i].ID) == r.PathValue("id") {
				f.comments[i].Body = c.Body
				f.patched = append(f.patched, c.Body)
				json.NewEncoder(w).Encode(f.comments[i])
				return
			}
		}
		t.Errorf("PATCH of unknown comment id %s", r.PathValue("id"))
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("POST /repos/acme/widgets/statuses/{sha}", func(w http.ResponseWriter, r *http.Request) {
		f.lastReq = r
		var s struct {
			State       string `json:"state"`
			Description string `json:"description"`
			Context     string `json:"context"`
		}
		if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
			t.Errorf("bad status body: %v", err)
		}
		f.statuses = append(f.statuses, recordedStatus{
			SHA: r.PathValue("sha"), State: s.State, Description: s.Description, Context: s.Context,
		})
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(s)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected GitHub call %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) client() *GitHub {
	return &GitHub{BaseURL: f.srv.URL, Token: StaticToken("gh-token"), HTTP: f.srv.Client()}
}

// Opened on a PR without our comment: the live comment is POSTed once
// ("creating", no connect info yet) and then PATCHed in place to "ready"
// with the connect string — never a second comment.
func TestOpenedPostsThenPatchesLiveComment(t *testing.T) {
	pg := newFakePG(t, false)
	gh := newFakeGitHub(t)
	deliver(t, newService(Config{ProxyHost: "pg.example.com:30432"}, pg.srv.URL, gh.client()), fixture(t, "pr_opened.json"))

	if len(gh.posted) != 1 {
		t.Fatalf("posted comments = %v, want exactly one", gh.posted)
	}
	for _, want := range []string{commentMarker, "`gh-pr-7`", "creating"} {
		if !strings.Contains(gh.posted[0], want) {
			t.Errorf("creating comment missing %q:\n%s", want, gh.posted[0])
		}
	}
	if len(gh.patched) != 1 {
		t.Fatalf("patched comments = %v, want exactly one (the ready update)", gh.patched)
	}
	final := gh.patched[0]
	for _, want := range []string{commentMarker, "`gh-pr-7`", "ready", "-h pg.example.com", "-p 30432", "appdb@pr-7"} {
		if !strings.Contains(final, want) {
			t.Errorf("ready comment missing %q:\n%s", want, final)
		}
	}
	if got := gh.lastReq.Header.Get("Authorization"); got != "Bearer gh-token" {
		t.Errorf("auth header = %q", got)
	}
	if got := gh.lastReq.Header.Get("Accept"); got != "application/vnd.github+json" {
		t.Errorf("accept header = %q", got)
	}
}

// A pre-existing marker comment is updated in place (PATCH), never
// duplicated (no POST).
func TestExistingMarkerCommentIsPatchedNotDuplicated(t *testing.T) {
	pg := newFakePG(t, true)
	gh := newFakeGitHub(t, "unrelated comment", "Connect info\n"+commentMarker)
	deliver(t, newService(Config{ProxyHost: "pg.example.com"}, pg.srv.URL, gh.client()), fixture(t, "pr_opened.json"))
	if len(gh.posted) != 0 {
		t.Fatalf("posted comments = %v, want none (marker present)", gh.posted)
	}
	if len(gh.patched) == 0 || !strings.Contains(gh.patched[len(gh.patched)-1], "ready") {
		t.Fatalf("patched = %v, want the marker comment updated to ready", gh.patched)
	}
	if !strings.Contains(gh.comments[1].Body, "ready") {
		t.Fatalf("marker comment not rewritten in place: %q", gh.comments[1].Body)
	}
	// list endpoint must paginate at 100 per page
	if got := gh.lastList.URL.Query().Get("per_page"); got != "100" {
		t.Errorf("per_page = %q, want 100", got)
	}
}

func TestResetCommentShowsShortSHA(t *testing.T) {
	pg := newFakePG(t, true)
	gh := newFakeGitHub(t, commentMarker+" old body")
	deliver(t, newService(Config{ResetOnPush: true, ProxyHost: "pg.example.com"}, pg.srv.URL, gh.client()),
		fixture(t, "pr_synchronize.json")) // head sha 9f8e7d6c5b4a3210
	final := gh.patched[len(gh.patched)-1]
	if !strings.Contains(final, "reset @ 9f8e7d6") {
		t.Fatalf("reset comment missing short sha:\n%s", final)
	}
	if !strings.Contains(final, "appdb@pr-7") {
		t.Fatalf("reset comment lost the connect string:\n%s", final)
	}
}

// Closing the PR rewrites the comment to "destroyed" — connect string gone
// (the branch it pointed at no longer exists) — and posts no status.
func TestClosedUpdatesCommentToDestroyed(t *testing.T) {
	pg := newFakePG(t, true)
	gh := newFakeGitHub(t, commentMarker+" branch gh-pr-7 ready, psql -h pg.example.com")
	deliver(t, newService(Config{ProxyHost: "pg.example.com"}, pg.srv.URL, gh.client()), fixture(t, "pr_closed.json"))

	pg.assertCalls("DELETE /v1/branches/gh-pr-7")
	if len(gh.patched) != 1 {
		t.Fatalf("patched = %v, want exactly one destroyed update", gh.patched)
	}
	body := gh.patched[0]
	if !strings.Contains(body, "destroyed") || !strings.Contains(body, commentMarker) {
		t.Errorf("destroyed comment = %q", body)
	}
	if strings.Contains(body, "psql") {
		t.Errorf("destroyed comment still shows a connect string:\n%s", body)
	}
	if len(gh.statuses) != 0 {
		t.Errorf("statuses = %+v, want none on closed (branch is gone)", gh.statuses)
	}
	if len(gh.posted) != 0 {
		t.Errorf("posted = %v, want none on closed", gh.posted)
	}
}

func TestCommentBodyIncludesExpiry(t *testing.T) {
	b := &api.Branch{Name: "pr-7", User: "app", ProxyDatabase: "appdb@pr-7",
		ExpiresAt: "2026-06-14T12:00:00Z"}
	body := commentBody("pg.example.com:30432", "pr-7", "ready", b)
	for _, want := range []string{"2026-06-14T12:00:00Z", "-h pg.example.com", "-p 30432"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	b.ExpiresAt = ""
	if body := commentBody("pg.example.com", "pr-7", "ready", b); strings.Contains(strings.ToLower(body), "expires") {
		t.Errorf("no-TTL branch shows an expiry:\n%s", body)
	}
}

func TestGitHubFailureDoesNotFailWebhook(t *testing.T) {
	pg := newFakePG(t, false)
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer down.Close()
	gh := &GitHub{BaseURL: down.URL, Token: StaticToken("gh-token"), HTTP: down.Client()}
	// comment failure is non-fatal: the branch operation still completes
	deliver(t, newService(Config{}, pg.srv.URL, gh), fixture(t, "pr_opened.json"))
	pg.assertCalls("GET /v1/branches/gh-pr-7", "POST /v1/branches")
}

func TestSetStatusRequestShape(t *testing.T) {
	gh := newFakeGitHub(t)
	err := gh.client().SetStatus(t.Context(), "acme/widgets", "0d1e2f3a4b5c6d7e", "pending", "creating branch pr-7")
	if err != nil {
		t.Fatal(err)
	}
	want := recordedStatus{SHA: "0d1e2f3a4b5c6d7e", State: "pending",
		Description: "creating branch pr-7", Context: "pgbranch/branch"}
	if len(gh.statuses) != 1 || gh.statuses[0] != want {
		t.Fatalf("statuses = %+v, want [%+v]", gh.statuses, want)
	}
	if got := gh.lastReq.Header.Get("Authorization"); got != "Bearer gh-token" {
		t.Errorf("auth header = %q", got)
	}
}

func TestSetStatusTruncatesDescriptionTo140(t *testing.T) {
	gh := newFakeGitHub(t)
	long := strings.Repeat("é", 200) // rune-counted, not bytes
	if err := gh.client().SetStatus(t.Context(), "acme/widgets", "abc", "failure", long); err != nil {
		t.Fatal(err)
	}
	if got := len([]rune(gh.statuses[0].Description)); got > 140 {
		t.Fatalf("description is %d runes, want <= 140", got)
	}
}

// Every handled ensure event brackets the branch operation with commit
// statuses on the PR head SHA: pending before, success after — so CI can
// gate on context pgbranch/branch instead of psql retry loops.
func TestEnsureSetsPendingThenSuccessStatus(t *testing.T) {
	pg := newFakePG(t, false)
	gh := newFakeGitHub(t)
	deliver(t, newService(Config{ProxyHost: "pg.example.com:30432"}, pg.srv.URL, gh.client()), fixture(t, "pr_opened.json"))

	if len(gh.statuses) != 2 {
		t.Fatalf("statuses = %+v, want pending then success", gh.statuses)
	}
	pending, success := gh.statuses[0], gh.statuses[1]
	if pending.State != "pending" || pending.SHA != "0d1e2f3a4b5c6d7e" ||
		pending.Context != "pgbranch/branch" || !strings.Contains(pending.Description, "creating branch gh-pr-7") {
		t.Errorf("pending status = %+v", pending)
	}
	if success.State != "success" || success.SHA != "0d1e2f3a4b5c6d7e" ||
		!strings.Contains(success.Description, "branch gh-pr-7 ready") ||
		!strings.Contains(success.Description, "pg.example.com:30432") {
		t.Errorf("success status = %+v", success)
	}
}

func TestSynchronizeWithResetPostsResettingPending(t *testing.T) {
	pg := newFakePG(t, true)
	gh := newFakeGitHub(t)
	deliver(t, newService(Config{ResetOnPush: true}, pg.srv.URL, gh.client()), fixture(t, "pr_synchronize.json"))
	if len(gh.statuses) < 1 || !strings.Contains(gh.statuses[0].Description, "resetting branch gh-pr-7") {
		t.Fatalf("statuses = %+v, want pending 'resetting branch gh-pr-7' first", gh.statuses)
	}
}

// branchd failing mid-operation surfaces as a failure status on the head
// SHA (the 202 ack already went out; the status is the visible outcome).
func TestEnsureFailurePostsFailureStatus(t *testing.T) {
	pg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "kaboom"})
	}))
	defer pg.Close()
	gh := newFakeGitHub(t)
	deliver(t, newService(Config{}, pg.URL, gh.client()), fixture(t, "pr_opened.json"))

	if len(gh.statuses) != 2 {
		t.Fatalf("statuses = %+v, want pending then failure", gh.statuses)
	}
	if gh.statuses[0].State != "pending" {
		t.Errorf("first status = %+v, want pending", gh.statuses[0])
	}
	fail := gh.statuses[1]
	if fail.State != "failure" || fail.Description == "" || len([]rune(fail.Description)) > 140 {
		t.Errorf("failure status = %+v", fail)
	}
}

// sampleDiff is the DiffResult the fake PG serves for diff-comment tests.
func sampleDiff() *engine.DiffResult {
	return &engine.DiffResult{
		SchemaDiff: "@@ -1,1 +1,2 @@\n CREATE TABLE users (\n+    extra text\n",
		Tables: []engine.TableDelta{
			{Table: "diffdemo", BaseRows: 0, BranchRows: 100, Delta: 100},
			{Table: "untouched", BaseRows: 50, BranchRows: 50, Delta: 0},
			{Table: "users", BaseRows: 1000, BranchRows: 990, Delta: -10},
		},
	}
}

// With DiffOnPush + a GitHub client, an opened PR gets a diff comment under
// the diff marker (schema fence + delta table), separate from the connect
// comment — both upserted, neither clobbering the other.
func TestDiffOnPushPostsDiffCommentUnderOwnMarker(t *testing.T) {
	pg := newFakePG(t, false)
	pg.diff = sampleDiff()
	gh := newFakeGitHub(t)
	deliver(t, newService(Config{ProxyHost: "pg.example.com:30432", DiffOnPush: true}, pg.srv.URL, gh.client()),
		fixture(t, "pr_opened.json"))

	// the connect comment (POST creating + PATCH ready) AND a diff comment
	// were posted; locate the diff comment by its marker.
	var connect, diff string
	for _, c := range gh.comments {
		switch {
		case strings.Contains(c.Body, diffMarker):
			diff = c.Body
		case strings.Contains(c.Body, commentMarker):
			connect = c.Body
		}
	}
	if connect == "" {
		t.Fatal("connect comment missing")
	}
	if diff == "" {
		t.Fatalf("no diff comment under %q; comments=%v", diffMarker, gh.comments)
	}
	// diff comment carries its own marker, the schema fence and the delta table
	for _, want := range []string{diffMarker, "```diff", "extra text", "| `diffdemo` | 0 | 100 | +100 |"} {
		if !strings.Contains(diff, want) {
			t.Errorf("diff comment missing %q:\n%s", want, diff)
		}
	}
	// the two markers live in different comments — neither clobbered the other
	if strings.Contains(connect, diffMarker) || strings.Contains(diff, commentMarker) {
		t.Errorf("markers bled across comments:\nconnect=%s\ndiff=%s", connect, diff)
	}
	// diff was requested without data sampling (schema + deltas only)
	if pg.diffData != "" {
		t.Errorf("diff ?data = %q, want empty (no sampling in the comment)", pg.diffData)
	}
}

// Without DiffOnPush, no diff comment is posted and the connect comment is
// unaffected; branchd's diff endpoint is never called.
func TestNoDiffCommentWhenDiffOnPushOff(t *testing.T) {
	pg := newFakePG(t, false)
	pg.diff = sampleDiff()
	gh := newFakeGitHub(t)
	deliver(t, newService(Config{ProxyHost: "pg.example.com:30432"}, pg.srv.URL, gh.client()),
		fixture(t, "pr_opened.json"))

	for _, c := range gh.comments {
		if strings.Contains(c.Body, diffMarker) {
			t.Errorf("diff comment posted with DiffOnPush off: %s", c.Body)
		}
	}
	for _, call := range pg.calls {
		if strings.Contains(call, "/diff") {
			t.Errorf("diff endpoint called with DiffOnPush off: %v", pg.calls)
		}
	}
	// connect comment still works (creating -> ready)
	if len(gh.patched) == 0 || !strings.Contains(gh.patched[len(gh.patched)-1], "ready") {
		t.Errorf("connect comment broken: patched=%v", gh.patched)
	}
}

// DiffOnPush with no GitHub client configured posts nothing and does not call
// the diff endpoint (the comment is the only consumer).
func TestDiffOnPushNoopWithoutGitHubClient(t *testing.T) {
	pg := newFakePG(t, false)
	pg.diff = sampleDiff()
	deliver(t, newService(Config{DiffOnPush: true}, pg.srv.URL, nil), fixture(t, "pr_opened.json"))
	for _, call := range pg.calls {
		if strings.Contains(call, "/diff") {
			t.Errorf("diff endpoint called without a GitHub client: %v", pg.calls)
		}
	}
}

// A failing diff endpoint is non-fatal: the branch op completes, the connect
// comment is still posted, and no diff comment appears.
func TestDiffCommentFailureIsNonFatal(t *testing.T) {
	pg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/diff"):
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "diff boom"})
		case r.Method == "GET":
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
		default: // POST create
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(api.Branch{Name: "pr-7", State: "ready", ProxyDatabase: "appdb@pr-7"})
		}
	}))
	defer pg.Close()
	gh := newFakeGitHub(t)
	deliver(t, newService(Config{DiffOnPush: true}, pg.URL, gh.client()), fixture(t, "pr_opened.json"))

	for _, c := range gh.comments {
		if strings.Contains(c.Body, diffMarker) {
			t.Errorf("diff comment posted despite diff failure: %s", c.Body)
		}
	}
	// connect comment still made it
	if len(gh.posted) == 0 {
		t.Error("connect comment not posted after diff failure")
	}
}

// Closed on a PR that never got our comment: nothing to update, nothing is
// created (a fresh "destroyed" comment on a closed PR is just noise), and no
// status is posted.
func TestClosedWithoutMarkerCommentCreatesNothing(t *testing.T) {
	pg := newFakePG(t, true)
	gh := newFakeGitHub(t, "unrelated comment")
	deliver(t, newService(Config{}, pg.srv.URL, gh.client()), fixture(t, "pr_closed.json"))
	pg.assertCalls("DELETE /v1/branches/gh-pr-7")
	if len(gh.posted) != 0 || len(gh.patched) != 0 || len(gh.statuses) != 0 {
		t.Fatalf("posted=%v patched=%v statuses=%+v, want no GitHub writes on closed without a marker comment",
			gh.posted, gh.patched, gh.statuses)
	}
}
