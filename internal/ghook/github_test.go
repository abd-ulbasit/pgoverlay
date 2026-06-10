package ghook

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeGitHub serves the issue-comments endpoints for acme/widgets#7.
type fakeGitHub struct {
	t        *testing.T
	existing []string // bodies returned by the list endpoint
	posted   []string // bodies received by the create endpoint
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
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(c)
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
	h := newService(Config{ProxyHost: "pg.example.com:30432"}, pg.srv.URL, gh.client()).Handler()

	if rr := signedPost(t, h, fixture(t, "pr_opened.json")); rr.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body)
	}
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
	h := newService(Config{ProxyHost: "pg.example.com"}, pg.srv.URL, gh.client()).Handler()

	if rr := signedPost(t, h, fixture(t, "pr_opened.json")); rr.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body)
	}
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
	h := newService(Config{}, pg.srv.URL, gh).Handler()

	if rr := signedPost(t, h, fixture(t, "pr_opened.json")); rr.Code != http.StatusOK {
		t.Fatalf("code=%d want 200 (comment failure is non-fatal)", rr.Code)
	}
	pg.assertCalls("GET /v1/branches/pr-7", "POST /v1/branches")
}

func TestClosedDoesNotTouchGitHub(t *testing.T) {
	pg := newFakePG(t, true)
	gh := newFakeGitHub(t) // any call → t.Errorf via mux assertions below
	h := newService(Config{}, pg.srv.URL, gh.client()).Handler()

	if rr := signedPost(t, h, fixture(t, "pr_closed.json")); rr.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body)
	}
	if gh.lastReq != nil {
		t.Fatalf("unexpected GitHub call on closed: %s", gh.lastReq.URL)
	}
}
