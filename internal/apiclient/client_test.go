package apiclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/abd-ulbasit/pgbranch/internal/api"
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

func TestErrorResponsesSurfaceMessage(t *testing.T) {
	ts, _, _ := newStub(t, http.StatusConflict, map[string]string{"error": "branch exists already"})
	c := New(ts.URL, "tok")
	_, err := c.CreateBranch(context.Background(), api.CreateBranchRequest{Name: "pr-1", Source: "main"})
	if err == nil || !strings.Contains(err.Error(), "branch exists already") {
		t.Fatalf("err=%v", err)
	}
}
