package ghook

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/abd-ulbasit/pgbranch/internal/api"
)

// fakePG is an httptest stand-in for branchd's REST API. It records every
// call as "METHOD path" and serves a single configurable branch.
type fakePG struct {
	t      *testing.T
	calls  []string
	exists bool                    // GET /v1/branches/{name} → 200 vs 404
	create api.CreateBranchRequest // last create body
	srv    *httptest.Server
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
	pg.assertCalls("GET /v1/branches/pr-7", "POST /v1/branches")
	want := api.CreateBranchRequest{Name: "pr-7", Source: "staging", TTLSeconds: 259200}
	if pg.create != want {
		t.Fatalf("create request = %+v, want %+v", pg.create, want)
	}
}

func TestReopenedExistingBranchIsNoop(t *testing.T) {
	pg := newFakePG(t, true)
	svc := newService(Config{}, pg.srv.URL, nil)

	body := bytes.Replace(fixture(t, "pr_opened.json"), []byte(`"opened"`), []byte(`"reopened"`), 1)
	deliver(t, svc, body)
	pg.assertCalls("GET /v1/branches/pr-7")
}

func TestSynchronizeDefaultEnsuresWithoutReset(t *testing.T) {
	pg := newFakePG(t, true)
	deliver(t, newService(Config{}, pg.srv.URL, nil), fixture(t, "pr_synchronize.json"))
	pg.assertCalls("GET /v1/branches/pr-7") // no reset, no create

	// missing branch is (re)created even on synchronize
	pg2 := newFakePG(t, false)
	deliver(t, newService(Config{}, pg2.srv.URL, nil), fixture(t, "pr_synchronize.json"))
	pg2.assertCalls("GET /v1/branches/pr-7", "POST /v1/branches")
}

func TestSynchronizeWithResetOnPushResetsExistingBranch(t *testing.T) {
	pg := newFakePG(t, true)
	deliver(t, newService(Config{ResetOnPush: true}, pg.srv.URL, nil), fixture(t, "pr_synchronize.json"))
	pg.assertCalls("GET /v1/branches/pr-7", "POST /v1/branches/pr-7/reset")

	// freshly created branch needs no reset
	pg2 := newFakePG(t, false)
	deliver(t, newService(Config{ResetOnPush: true}, pg2.srv.URL, nil), fixture(t, "pr_synchronize.json"))
	pg2.assertCalls("GET /v1/branches/pr-7", "POST /v1/branches")
}

func TestClosedDestroysBranchAndToleratesMissing(t *testing.T) {
	pg := newFakePG(t, true)
	svc := newService(Config{}, pg.srv.URL, nil)

	deliver(t, svc, fixture(t, "pr_closed.json"))
	pg.assertCalls("DELETE /v1/branches/pr-7")

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
	pg.assertCalls("GET /v1/branches/pr-7", "POST /v1/branches")
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
