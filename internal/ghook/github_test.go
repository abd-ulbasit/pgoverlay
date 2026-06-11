package ghook

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// recordedStatus is one POST /statuses/{sha} call seen by the fake.
type recordedStatus struct {
	SHA, State, Description, Context string
}

// fakeGitHub serves the issue-comments and commit-status endpoints for
// acme/widgets#7.
type fakeGitHub struct {
	t        *testing.T
	existing []string // bodies returned by the list endpoint
	posted   []string // bodies received by the create endpoint
	statuses []recordedStatus
	lastReq  *http.Request
	srv      *httptest.Server
}

func newFakeGitHub(t *testing.T, existing ...string) *fakeGitHub {
	f := &fakeGitHub{t: t, existing: existing}
	mux := http.NewServeMux()
	type comment struct {
		Body string `json:"body"`
	}
	mux.HandleFunc("GET /repos/acme/widgets/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		f.lastReq = r
		var out []comment
		for _, b := range f.existing {
			out = append(out, comment{Body: b})
		}
		json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("POST /repos/acme/widgets/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		f.lastReq = r
		var c comment
		if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
			t.Errorf("bad comment body: %v", err)
		}
		f.posted = append(f.posted, c.Body)
		f.existing = append(f.existing, c.Body)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(c)
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
	return &GitHub{BaseURL: f.srv.URL, Token: "gh-token", HTTP: f.srv.Client()}
}

func TestOpenedPostsConnectInfoComment(t *testing.T) {
	pg := newFakePG(t, false)
	gh := newFakeGitHub(t)
	deliver(t, newService(Config{ProxyHost: "pg.example.com:30432"}, pg.srv.URL, gh.client()), fixture(t, "pr_opened.json"))
	if len(gh.posted) != 1 {
		t.Fatalf("posted comments = %v, want exactly one", gh.posted)
	}
	body := gh.posted[0]
	for _, want := range []string{commentMarker, "-h pg.example.com", "-p 30432", "appdb@pr-7", "`pr-7`"} {
		if !strings.Contains(body, want) {
			t.Errorf("comment body missing %q:\n%s", want, body)
		}
	}
	if got := gh.lastReq.Header.Get("Authorization"); got != "Bearer gh-token" {
		t.Errorf("auth header = %q", got)
	}
	if got := gh.lastReq.Header.Get("Accept"); got != "application/vnd.github+json" {
		t.Errorf("accept header = %q", got)
	}
}

func TestMarkerCommentDeduplicates(t *testing.T) {
	pg := newFakePG(t, true)
	gh := newFakeGitHub(t, "unrelated comment", "Connect info\n"+commentMarker)
	deliver(t, newService(Config{ProxyHost: "pg.example.com"}, pg.srv.URL, gh.client()), fixture(t, "pr_opened.json"))
	if len(gh.posted) != 0 {
		t.Fatalf("posted comments = %v, want none (marker present)", gh.posted)
	}
	// list endpoint must paginate at 100 per page
	if got := gh.lastReq.URL.Query().Get("per_page"); got != "100" {
		t.Errorf("per_page = %q, want 100", got)
	}
}

func TestGitHubFailureDoesNotFailWebhook(t *testing.T) {
	pg := newFakePG(t, false)
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer down.Close()
	gh := &GitHub{BaseURL: down.URL, Token: "gh-token", HTTP: down.Client()}
	// comment failure is non-fatal: the branch operation still completes
	deliver(t, newService(Config{}, pg.srv.URL, gh), fixture(t, "pr_opened.json"))
	pg.assertCalls("GET /v1/branches/pr-7", "POST /v1/branches")
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
		pending.Context != "pgbranch/branch" || !strings.Contains(pending.Description, "creating branch pr-7") {
		t.Errorf("pending status = %+v", pending)
	}
	if success.State != "success" || success.SHA != "0d1e2f3a4b5c6d7e" ||
		!strings.Contains(success.Description, "branch pr-7 ready") ||
		!strings.Contains(success.Description, "pg.example.com:30432") {
		t.Errorf("success status = %+v", success)
	}
}

func TestSynchronizeWithResetPostsResettingPending(t *testing.T) {
	pg := newFakePG(t, true)
	gh := newFakeGitHub(t)
	deliver(t, newService(Config{ResetOnPush: true}, pg.srv.URL, gh.client()), fixture(t, "pr_synchronize.json"))
	if len(gh.statuses) < 1 || !strings.Contains(gh.statuses[0].Description, "resetting branch pr-7") {
		t.Fatalf("statuses = %+v, want pending 'resetting branch pr-7' first", gh.statuses)
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

func TestClosedDoesNotTouchGitHub(t *testing.T) {
	pg := newFakePG(t, true)
	gh := newFakeGitHub(t) // any call → t.Errorf via mux assertions below
	deliver(t, newService(Config{}, pg.srv.URL, gh.client()), fixture(t, "pr_closed.json"))
	if gh.lastReq != nil {
		t.Fatalf("unexpected GitHub call on closed: %s", gh.lastReq.URL)
	}
}
