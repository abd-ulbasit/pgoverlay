package apiclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/abd-ulbasit/pgbranch/internal/api"
	"github.com/abd-ulbasit/pgbranch/internal/engine"
)

// newStub records the last request and replies with the given status/body.
func newStub(t *testing.T, status int, body any) (*httptest.Server, *http.Request, *[]byte) {
	t.Helper()
	var lastReq http.Request
	var lastBody []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastReq = *r
		b := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			r.Body.Read(b)
		}
		lastBody = b
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(ts.Close)
	return ts, &lastReq, &lastBody
}

func TestClientSendsBearerTokenAndPaths(t *testing.T) {
	ts, req, _ := newStub(t, http.StatusOK, []api.Branch{{Name: "pr-1", Port: 5}})
	c := New(ts.URL+"/", "tok") // trailing slash must not double up

	branches, err := c.ListBranches(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(branches) != 1 || branches[0].Name != "pr-1" {
		t.Fatalf("branches=%v", branches)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer tok" {
		t.Fatalf("auth header %q", got)
	}
	if req.Method != "GET" || req.URL.Path != "/v1/branches" {
		t.Fatalf("%s %s", req.Method, req.URL.Path)
	}
}

func TestCreateBranchRequestBody(t *testing.T) {
	ts, req, body := newStub(t, http.StatusCreated, api.Branch{Name: "pr-1", Port: 5432, ProxyDatabase: "postgres@pr-1"})
	c := New(ts.URL, "tok")

	b, err := c.CreateBranch(context.Background(), api.CreateBranchRequest{Name: "pr-1", Source: "main", TTLSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	if b.ProxyDatabase != "postgres@pr-1" {
		t.Fatalf("branch=%+v", b)
	}
	if req.Method != "POST" || req.URL.Path != "/v1/branches" {
		t.Fatalf("%s %s", req.Method, req.URL.Path)
	}
	var sent api.CreateBranchRequest
	if err := json.Unmarshal(*body, &sent); err != nil {
		t.Fatalf("body %q: %v", *body, err)
	}
	if sent != (api.CreateBranchRequest{Name: "pr-1", Source: "main", TTLSeconds: 60}) {
		t.Fatalf("sent %+v", sent)
	}
}

func TestPathsForNamedResources(t *testing.T) {
	cases := []struct {
		call         func(c *Client) error
		method, path string
		status       int
	}{
		{func(c *Client) error { return c.DestroyBranch(context.Background(), "pr/1") },
			"DELETE", "/v1/branches/pr%2F1", http.StatusNoContent},
		{func(c *Client) error { _, err := c.ResetBranch(context.Background(), "pr-1"); return err },
			"POST", "/v1/branches/pr-1/reset", http.StatusOK},
		{func(c *Client) error { _, err := c.GetBranch(context.Background(), "pr-1"); return err },
			"GET", "/v1/branches/pr-1", http.StatusOK},
		{func(c *Client) error { return c.RemoveSource(context.Background(), "main") },
			"DELETE", "/v1/sources/main", http.StatusNoContent},
		{func(c *Client) error { _, err := c.RefreshSource(context.Background(), "main", "pw"); return err },
			"POST", "/v1/sources/main/refresh", http.StatusOK},
		{func(c *Client) error { _, err := c.ListSources(context.Background()); return err },
			"GET", "/v1/sources", http.StatusOK},
		{func(c *Client) error {
			_, err := c.CreateSource(context.Background(), api.CreateSourceRequest{Name: "main", Host: "h", Password: "pw"})
			return err
		}, "POST", "/v1/sources", http.StatusCreated},
	}
	for _, tc := range cases {
		var rbody any = api.Source{}
		if strings.Contains(tc.path, "branches") {
			rbody = api.Branch{}
		}
		if tc.method == "GET" && !strings.Contains(tc.path, "/v1/branches/") {
			rbody = []api.Source{}
		}
		ts, req, _ := newStub(t, tc.status, rbody)
		if err := tc.call(New(ts.URL, "tok")); err != nil {
			t.Fatalf("%s %s: %v", tc.method, tc.path, err)
		}
		if req.Method != tc.method || req.URL.EscapedPath() != tc.path {
			t.Fatalf("got %s %s want %s %s", req.Method, req.URL.EscapedPath(), tc.method, tc.path)
		}
	}
}

func TestMaskScriptsClient(t *testing.T) {
	scripts := []api.MaskScript{
		{Name: "emails.sql", SQL: "UPDATE users SET email = 'x@invalid'"},
		{Name: "names.sql", SQL: "UPDATE users SET name = 'redacted'"},
	}
	ts, req, body := newStub(t, http.StatusOK, scripts)
	c := New(ts.URL, "tok")

	got, err := c.SetMaskScripts(context.Background(), "main", scripts)
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != "PUT" || req.URL.Path != "/v1/sources/main/mask" {
		t.Fatalf("%s %s", req.Method, req.URL.Path)
	}
	var sent []api.MaskScript
	if err := json.Unmarshal(*body, &sent); err != nil {
		t.Fatalf("body %q: %v", *body, err)
	}
	if len(sent) != 2 || sent[0] != scripts[0] || sent[1] != scripts[1] {
		t.Fatalf("sent %v", sent)
	}
	if len(got) != 2 || got[0] != scripts[0] {
		t.Fatalf("got %v", got)
	}

	ts2, req2, _ := newStub(t, http.StatusOK, scripts)
	got, err = New(ts2.URL, "tok").GetMaskScripts(context.Background(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if req2.Method != "GET" || req2.URL.Path != "/v1/sources/main/mask" {
		t.Fatalf("%s %s", req2.Method, req2.URL.Path)
	}
	if len(got) != 2 || got[1] != scripts[1] {
		t.Fatalf("got %v", got)
	}
}

func TestBranchUsage(t *testing.T) {
	ts, req, _ := newStub(t, http.StatusOK, map[string]int64{"bytes": 5242880})
	c := New(ts.URL, "tok")

	n, err := c.BranchUsage(context.Background(), "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if n != 5242880 {
		t.Fatalf("usage = %d want 5242880", n)
	}
	if req.Method != "GET" || req.URL.Path != "/v1/branches/pr-1/usage" {
		t.Fatalf("%s %s", req.Method, req.URL.Path)
	}
}

func TestErrorResponsesSurfaceMessage(t *testing.T) {
	ts, _, _ := newStub(t, http.StatusConflict, map[string]string{"error": "branch exists already"})
	c := New(ts.URL, "tok")
	_, err := c.CreateBranch(context.Background(), api.CreateBranchRequest{Name: "pr-1", Source: "main"})
	if err == nil || !strings.Contains(err.Error(), "branch exists already") {
		t.Fatalf("err=%v", err)
	}
}

func TestNotFoundErrorsAreDetectable(t *testing.T) {
	ts, _, _ := newStub(t, http.StatusNotFound, map[string]string{"error": "not found"})
	c := New(ts.URL, "tok")
	_, err := c.GetBranch(context.Background(), "pr-404")
	if err == nil || !IsNotFound(err) {
		t.Fatalf("want IsNotFound, got %v", err)
	}

	ts2, _, _ := newStub(t, http.StatusConflict, map[string]string{"error": "boom"})
	_, err = New(ts2.URL, "tok").CreateBranch(context.Background(), api.CreateBranchRequest{Name: "x"})
	if err == nil || IsNotFound(err) {
		t.Fatalf("409 must not be IsNotFound: %v", err)
	}
}

// TestHTTPSBaseURL: the client works against https servers; with a
// self-signed cert it fails verification by default and succeeds when
// PGBRANCH_TLS_SKIP_VERIFY=1 is set (escape hatch for self-signed branchd).
func TestHTTPSBaseURL(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]api.Branch{{Name: "pr-1"}})
	}))
	t.Cleanup(ts.Close)
	if !strings.HasPrefix(ts.URL, "https://") {
		t.Fatalf("test server URL %q is not https", ts.URL)
	}

	// default client: self-signed cert must be rejected
	if _, err := New(ts.URL, "tok").ListBranches(context.Background()); err == nil {
		t.Fatal("expected certificate verification error against self-signed server")
	}

	// escape hatch: PGBRANCH_TLS_SKIP_VERIFY=1
	t.Setenv("PGBRANCH_TLS_SKIP_VERIFY", "1")
	got, err := New(ts.URL, "tok").ListBranches(context.Background())
	if err != nil {
		t.Fatalf("with PGBRANCH_TLS_SKIP_VERIFY=1: %v", err)
	}
	if len(got) != 1 || got[0].Name != "pr-1" {
		t.Fatalf("branches = %+v", got)
	}
}

func TestDiffBranch(t *testing.T) {
	ts, req, _ := newStub(t, http.StatusOK, engine.DiffResult{
		SchemaDiff: "@@ -1 +1,2 @@\n stay\n+added\n",
		Tables:     []engine.TableDelta{{Table: "added", BranchRows: 7, Delta: 7}},
	})
	c := New(ts.URL, "tok")

	res, err := c.DiffBranch(context.Background(), "pr-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != "GET" || req.URL.Path != "/v1/branches/pr-1/diff" {
		t.Fatalf("%s %s", req.Method, req.URL.Path)
	}
	if req.URL.RawQuery != "" {
		t.Fatalf("dataSample 0 sent a query: %q", req.URL.RawQuery)
	}
	if !strings.Contains(res.SchemaDiff, "+added") {
		t.Fatalf("schema diff = %q", res.SchemaDiff)
	}
	want := engine.TableDelta{Table: "added", BranchRows: 7, Delta: 7}
	if len(res.Tables) != 1 || res.Tables[0].Table != want.Table ||
		res.Tables[0].BranchRows != want.BranchRows || res.Tables[0].Delta != want.Delta {
		t.Fatalf("tables = %+v", res.Tables)
	}

	// dataSample > 0 appends ?data=N
	if _, err := c.DiffBranch(context.Background(), "pr-1", 5); err != nil {
		t.Fatal(err)
	}
	if req.URL.RawQuery != "data=5" {
		t.Fatalf("query = %q, want data=5", req.URL.RawQuery)
	}
}
